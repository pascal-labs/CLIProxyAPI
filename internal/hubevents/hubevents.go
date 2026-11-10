// Package hubevents keeps a bounded, persisted ring buffer of routing and
// credential-health events (rate limits, transient and network errors,
// failovers, rebinds, cooldowns, dead logins, recoveries, hub starts) for the
// management API.
package hubevents

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/hubsidecar"
	log "github.com/sirupsen/logrus"
)

// Event kinds.
const (
	KindRateLimited    = "rate_limited"
	KindTransientError = "transient_error"
	KindNetworkError   = "network_error"
	KindFailover       = "failover"
	KindRebind         = "rebind"
	KindCooldown       = "cooldown"
	KindDeadLogin      = "dead_login"
	KindRecovered      = "recovered"
	KindHubStart       = "hub_start"
)

const (
	// FileName is the persistence file inside the auth directory.
	FileName = hubsidecar.HubEventsFile
	// MaxEvents bounds the ring buffer.
	MaxEvents = 10_000
	// Retention is how long events are kept.
	Retention = 30 * 24 * time.Hour
	// maxDetailLength truncates details; they never carry request or response bodies.
	maxDetailLength = 200

	defaultFlushDelay = 30 * time.Second
)

// Event is one recorded event. AuthID and RelatedAuthID are kept for resolving
// auth indexes later and are never exposed by the management API.
type Event struct {
	T          time.Time `json:"t"`
	Kind       string    `json:"kind"`
	AuthID     string    `json:"auth_id,omitempty"`
	AuthIndex  string    `json:"auth_index,omitempty"`
	Provider   string    `json:"provider,omitempty"`
	Model      string    `json:"model,omitempty"`
	SessionKey string    `json:"session_key,omitempty"`
	StatusCode int       `json:"status_code,omitempty"`
	Detail     string    `json:"detail,omitempty"`
	// RelatedAuthID is the credential a failover or rebind moved away from;
	// like AuthID it is resolved to an auth_index for display.
	RelatedAuthID string `json:"related_auth_id,omitempty"`
}

// Options configures a Store.
type Options struct {
	Now        func() time.Time
	FlushDelay time.Duration
	Schedule   func(delay time.Duration, fn func()) (stop func() bool)
}

// Store is the ring buffer.
type Store struct {
	opts Options

	mu        sync.Mutex
	events    []Event
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
	return &Store{opts: opts}
}

// Start loads previous events and enables persistence.
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
		var persisted []Event
		if _, errRead := hubsidecar.ReadJSON(path, &persisted); errRead != nil {
			errLoad = errRead
		}
		s.mu.Lock()
		s.events = append(persisted, s.events...)
		sort.SliceStable(s.events, func(i, j int) bool { return s.events[i].T.Before(s.events[j].T) })
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

// Record appends an event. A zero time is set to now.
func (s *Store) Record(event Event) {
	if s == nil || strings.TrimSpace(event.Kind) == "" {
		return
	}
	if event.T.IsZero() {
		event.T = s.opts.Now()
	}
	event.T = event.T.UTC()
	event.Detail = truncate(strings.TrimSpace(event.Detail), maxDetailLength)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
	if len(s.events) > MaxEvents+MaxEvents/10 {
		s.pruneLocked()
	}
	s.dirty = true
	if s.path != "" && s.stopTimer == nil {
		s.stopTimer = s.opts.Schedule(s.opts.FlushDelay, s.flushFromTimer)
	}
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	cut := value[:limit]
	for len(cut) > 0 && cut[len(cut)-1]&0xC0 == 0x80 {
		cut = cut[:len(cut)-1]
	}
	if len(cut) > 0 && cut[len(cut)-1] >= 0xC0 {
		cut = cut[:len(cut)-1]
	}
	return cut + "..."
}

func (s *Store) pruneLocked() {
	cutoff := s.opts.Now().Add(-Retention)
	start := 0
	for start < len(s.events) && s.events[start].T.Before(cutoff) {
		start++
	}
	if len(s.events)-start > MaxEvents {
		start = len(s.events) - MaxEvents
	}
	if start > 0 {
		s.events = append([]Event(nil), s.events[start:]...)
	}
}

func (s *Store) flushFromTimer() {
	s.mu.Lock()
	s.stopTimer = nil
	s.mu.Unlock()
	if errFlush := s.Flush(); errFlush != nil {
		log.WithError(errFlush).Warn("hub events: persist failed")
	}
}

// Flush writes the events when they changed since the last write.
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
	snapshot := append([]Event(nil), s.events...)
	s.dirty = false
	s.mu.Unlock()
	if errWrite := hubsidecar.WriteJSONAtomic(path, snapshot); errWrite != nil {
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
		return fmt.Errorf("hub events: %w", errWrite)
	}
	return nil
}

// Query returns events at or after since, newest first.
func (s *Store) Query(since time.Time) []Event {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	var out []Event
	for i := len(s.events) - 1; i >= 0; i-- {
		if !s.events[i].T.Before(since) {
			out = append(out, s.events[i])
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].T.After(out[j].T) })
	return out
}
