// Package quotahistory keeps a bounded history of quota-window observations
// per credential and window so the management API can chart usage, estimate
// burn rate, project exhaustion, and report how much of each finished cycle
// went unused.
package quotahistory

import (
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/hubsidecar"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	// FileName is the persistence file inside the auth directory.
	FileName = hubsidecar.QuotaHistoryFile
	// Retention is how long samples are kept.
	Retention = 35 * 24 * time.Hour
	// FullResolution is how long samples keep their recorded spacing; older
	// samples are thinned to one per hour (cycle-final samples are kept).
	FullResolution = 7 * 24 * time.Hour
	// MinSpacing is the minimum spacing between retained samples of one
	// series; observations in between update the newest sample in place.
	MinSpacing = 5 * time.Minute
	// cycleSlack tolerates small jitter in reported reset times.
	cycleSlack = 10 * time.Minute

	defaultFlushDelay = 2 * time.Minute
)

// Sample is one observation of a window's utilization.
type Sample struct {
	T           time.Time
	UsedPercent float64
	ResetsAt    time.Time
}

// MarshalJSON encodes a sample compactly as [unix_seconds, used_percent, reset_unix_seconds].
func (s Sample) MarshalJSON() ([]byte, error) {
	reset := int64(0)
	if !s.ResetsAt.IsZero() {
		reset = s.ResetsAt.Unix()
	}
	return json.Marshal([]float64{float64(s.T.Unix()), s.UsedPercent, float64(reset)})
}

// UnmarshalJSON decodes the compact array form.
func (s *Sample) UnmarshalJSON(data []byte) error {
	var values []float64
	if errUnmarshal := json.Unmarshal(data, &values); errUnmarshal != nil {
		return errUnmarshal
	}
	if len(values) < 3 {
		return fmt.Errorf("quota sample has %d fields, want 3", len(values))
	}
	s.T = time.Unix(int64(values[0]), 0).UTC()
	s.UsedPercent = values[1]
	s.ResetsAt = time.Time{}
	if values[2] > 0 {
		s.ResetsAt = time.Unix(int64(values[2]), 0).UTC()
	}
	return nil
}

// SameCycle reports whether next belongs to the same window cycle as prev.
func SameCycle(prev, next Sample) bool {
	if prev.ResetsAt.IsZero() || next.ResetsAt.IsZero() {
		return true
	}
	if next.ResetsAt.After(prev.ResetsAt.Add(cycleSlack)) {
		return false
	}
	return next.T.Before(prev.ResetsAt)
}

type series struct {
	AuthID    string   `json:"auth_id"`
	AuthIndex string   `json:"auth_index,omitempty"`
	Provider  string   `json:"provider,omitempty"`
	Key       string   `json:"key"`
	Kind      string   `json:"kind,omitempty"`
	Samples   []Sample `json:"samples"`
}

// Options configures a Store.
type Options struct {
	Now        func() time.Time
	FlushDelay time.Duration
	Schedule   func(delay time.Duration, fn func()) (stop func() bool)
}

// Store keeps samples per (credential, window key) and persists them to
// <auth-dir>/quota-history.json with a debounce.
type Store struct {
	opts Options

	mu        sync.Mutex
	series    map[string]*series
	path      string
	loaded    bool
	dirty     bool
	stopTimer func() bool
	writeMu   sync.Mutex
}

var defaultStore = New(Options{})

// Default returns the process-wide store.
func Default() *Store { return defaultStore }

// New constructs a store.
func New(opts Options) *Store {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.FlushDelay <= 0 {
		opts.FlushDelay = defaultFlushDelay
	}
	if opts.Schedule == nil {
		opts.Schedule = func(delay time.Duration, fn func()) func() bool { return time.AfterFunc(delay, fn).Stop }
	}
	return &Store{opts: opts, series: make(map[string]*series)}
}

func seriesKey(authID, key string) string { return authID + "\x00" + key }

// Start loads previous samples and enables persistence.
func (s *Store) Start(authDir string) error {
	if s == nil || strings.TrimSpace(authDir) == "" {
		return nil
	}
	path := filepath.Join(authDir, FileName)
	s.mu.Lock()
	alreadyLoaded := s.loaded
	s.loaded = true
	s.mu.Unlock()
	var errLoad error
	if !alreadyLoaded {
		var persisted []series
		if _, errRead := hubsidecar.ReadJSON(path, &persisted); errRead != nil {
			errLoad = errRead
		}
		s.mu.Lock()
		for i := range persisted {
			item := persisted[i]
			if item.AuthID == "" || item.Key == "" {
				continue
			}
			sort.Slice(item.Samples, func(a, b int) bool { return item.Samples[a].T.Before(item.Samples[b].T) })
			copyItem := item
			s.series[seriesKey(item.AuthID, item.Key)] = &copyItem
		}
		s.pruneLocked()
		s.mu.Unlock()
	}
	s.mu.Lock()
	s.path = path
	s.mu.Unlock()
	return errLoad
}

// Stop flushes and disables persistence.
func (s *Store) Stop() error {
	if s == nil {
		return nil
	}
	errFlush := s.Flush()
	s.mu.Lock()
	if s.stopTimer != nil {
		s.stopTimer()
		s.stopTimer = nil
	}
	s.path = ""
	s.mu.Unlock()
	return errFlush
}

// Record adds the observations in windows for one credential. Observations
// that are not newer than the series' last sample are ignored.
func (s *Store) Record(authID, authIndex, provider string, windows []coreauth.QuotaWindow) {
	if s == nil || authID == "" || len(windows) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	cutoff := s.opts.Now().Add(-Retention)
	for _, window := range windows {
		if window.Key == "" || window.ObservedAt.IsZero() || window.ObservedAt.Before(cutoff) {
			continue
		}
		key := seriesKey(authID, window.Key)
		entry := s.series[key]
		if entry == nil {
			entry = &series{AuthID: authID, Key: window.Key}
			s.series[key] = entry
		}
		entry.AuthIndex, entry.Provider, entry.Kind = authIndex, provider, window.Kind
		sample := Sample{T: window.ObservedAt.UTC().Truncate(time.Second), UsedPercent: window.UsedPercent, ResetsAt: window.ResetsAt.UTC().Truncate(time.Second)}
		if appendSample(entry, sample) {
			changed = true
		}
	}
	if changed {
		s.dirty = true
		if s.path != "" && s.stopTimer == nil {
			s.stopTimer = s.opts.Schedule(s.opts.FlushDelay, s.flushFromTimer)
		}
	}
}

// appendSample keeps retained samples at least MinSpacing apart within a
// cycle. The newest sample is provisional while it is closer than that to its
// predecessor and is replaced by the next observation, so the newest
// observation (and thus each cycle's final utilization) is always kept.
func appendSample(entry *series, sample Sample) bool {
	n := len(entry.Samples)
	if n > 0 && !sample.T.After(entry.Samples[n-1].T) {
		return false
	}
	if n >= 2 {
		prev, last := entry.Samples[n-2], entry.Samples[n-1]
		if SameCycle(prev, last) && SameCycle(last, sample) && last.T.Sub(prev.T) < MinSpacing {
			entry.Samples[n-1] = sample
			return true
		}
	}
	entry.Samples = append(entry.Samples, sample)
	return true
}

func (s *Store) flushFromTimer() {
	s.mu.Lock()
	s.stopTimer = nil
	s.mu.Unlock()
	if errFlush := s.Flush(); errFlush != nil {
		log.WithError(errFlush).Warn("quota history: persist failed")
	}
}

func (s *Store) pruneLocked() {
	now := s.opts.Now()
	cutoff := now.Add(-Retention)
	thinBefore := now.Add(-FullResolution)
	for key, entry := range s.series {
		entry.Samples = thin(entry.Samples, cutoff, thinBefore)
		if len(entry.Samples) == 0 {
			delete(s.series, key)
		}
	}
}

// thin drops samples before cutoff and keeps one sample per hour (the last of
// its cycle within that hour) before thinBefore.
func thin(samples []Sample, cutoff, thinBefore time.Time) []Sample {
	out := samples[:0]
	for i, sample := range samples {
		if sample.T.Before(cutoff) {
			continue
		}
		if sample.T.Before(thinBefore) && i+1 < len(samples) {
			next := samples[i+1]
			if next.T.Truncate(time.Hour).Equal(sample.T.Truncate(time.Hour)) && SameCycle(sample, next) {
				continue
			}
		}
		out = append(out, sample)
	}
	return out
}

// Flush writes the samples when they changed since the last write.
func (s *Store) Flush() error {
	if s == nil {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.Lock()
	path := s.path
	if path == "" || !s.dirty {
		s.mu.Unlock()
		return nil
	}
	s.pruneLocked()
	snapshot := make([]series, 0, len(s.series))
	for _, entry := range s.series {
		copyEntry := *entry
		copyEntry.Samples = append([]Sample(nil), entry.Samples...)
		snapshot = append(snapshot, copyEntry)
	}
	s.dirty = false
	s.mu.Unlock()
	sort.Slice(snapshot, func(i, j int) bool {
		if snapshot[i].AuthID != snapshot[j].AuthID {
			return snapshot[i].AuthID < snapshot[j].AuthID
		}
		return snapshot[i].Key < snapshot[j].Key
	})
	if errWrite := hubsidecar.WriteJSONAtomic(path, snapshot); errWrite != nil {
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
		return fmt.Errorf("quota history: %w", errWrite)
	}
	return nil
}

// Samples returns a copy of one series' samples at or after since, oldest first.
func (s *Store) Samples(authID, key string, since time.Time) []Sample {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.series[seriesKey(authID, key)]
	if entry == nil {
		return nil
	}
	out := make([]Sample, 0, len(entry.Samples))
	for _, sample := range entry.Samples {
		if !sample.T.Before(since) {
			out = append(out, sample)
		}
	}
	return out
}

// Keys returns the window keys recorded for a credential.
func (s *Store) Keys(authID string) []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	for _, entry := range s.series {
		if entry.AuthID == authID {
			keys = append(keys, entry.Key)
		}
	}
	sort.Strings(keys)
	return keys
}

// Cycle is a finished window cycle and its utilization just before reset
// (100 - FinalUsedPercent is the allowance left unused).
type Cycle struct {
	ResetAt          time.Time
	FinalUsedPercent float64
}

// Cycles detects finished cycles in chronologically ordered samples. A cycle
// ends when the reported reset moves forward or a sample is taken after the
// previous reset; the last sample before that is the cycle's final reading. A
// trailing cycle whose reset has already passed also counts as finished.
func Cycles(samples []Sample, now time.Time) []Cycle {
	var cycles []Cycle
	for i := 1; i < len(samples); i++ {
		prev := samples[i-1]
		if !prev.ResetsAt.IsZero() && !SameCycle(prev, samples[i]) {
			cycles = append(cycles, Cycle{ResetAt: prev.ResetsAt, FinalUsedPercent: prev.UsedPercent})
		}
	}
	if n := len(samples); n > 0 {
		last := samples[n-1]
		if !last.ResetsAt.IsZero() && !last.ResetsAt.After(now) {
			cycles = append(cycles, Cycle{ResetAt: last.ResetsAt, FinalUsedPercent: last.UsedPercent})
		}
	}
	return cycles
}

// inCycle reports whether sample belongs to the cycle ending at resetsAt
// (within cycleSlack); samples or windows without a reset always match.
func inCycle(sample Sample, resetsAt time.Time) bool {
	if resetsAt.IsZero() || sample.ResetsAt.IsZero() {
		return true
	}
	return sample.ResetsAt.Sub(resetsAt) <= cycleSlack && resetsAt.Sub(sample.ResetsAt) <= cycleSlack
}

// CycleCoverage reports how many of the chronologically ordered samples
// belong to the cycle ending at resetsAt, and when the first and last of them
// were taken, so callers can tell whether a cycle has enough history to
// project from.
func CycleCoverage(samples []Sample, resetsAt time.Time) (count int, first, last time.Time) {
	for _, sample := range samples {
		if !inCycle(sample, resetsAt) {
			continue
		}
		if count == 0 {
			first = sample.T
		}
		last = sample.T
		count++
	}
	return count, first, last
}

// BurnRate returns the least-squares slope (percentage points per hour) of the
// samples that belong to the cycle ending at resetsAt and were taken within
// lookback of now. ok is false with fewer than two such samples.
func BurnRate(samples []Sample, resetsAt, now time.Time, lookback time.Duration) (rate float64, ok bool) {
	since := now.Add(-lookback)
	var xs, ys []float64
	for _, sample := range samples {
		if sample.T.Before(since) || sample.T.After(now) || !inCycle(sample, resetsAt) {
			continue
		}
		xs = append(xs, sample.T.Sub(since).Hours())
		ys = append(ys, sample.UsedPercent)
	}
	if len(xs) < 2 {
		return 0, false
	}
	var meanX, meanY float64
	for i := range xs {
		meanX += xs[i]
		meanY += ys[i]
	}
	meanX /= float64(len(xs))
	meanY /= float64(len(ys))
	var numerator, denominator float64
	for i := range xs {
		numerator += (xs[i] - meanX) * (ys[i] - meanY)
		denominator += (xs[i] - meanX) * (xs[i] - meanX)
	}
	if denominator == 0 {
		return 0, false
	}
	return numerator / denominator, true
}

// Downsample reduces samples to at most limit points by splitting the time
// range into equal slices and keeping the highest reading of each slice, so
// peaks before resets survive.
func Downsample(samples []Sample, limit int) []Sample {
	if limit <= 0 || len(samples) <= limit {
		return samples
	}
	first, last := samples[0].T, samples[len(samples)-1].T
	span := last.Sub(first)
	if span <= 0 {
		return samples[len(samples)-limit:]
	}
	out := make([]Sample, 0, limit)
	slot := -1
	for _, sample := range samples {
		current := int(float64(sample.T.Sub(first)) / float64(span) * float64(limit))
		if current >= limit {
			current = limit - 1
		}
		if current != slot {
			out = append(out, sample)
			slot = current
			continue
		}
		if sample.UsedPercent >= out[len(out)-1].UsedPercent {
			out[len(out)-1] = sample
		}
	}
	return out
}

// RoundPercent rounds to two decimals.
func RoundPercent(value float64) float64 {
	return math.Round(value*100) / 100
}
