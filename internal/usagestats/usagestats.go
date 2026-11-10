// Package usagestats keeps hourly per-credential and per-model token usage, plus
// per-session usage, in process so the management API can report request
// volume, cache efficiency and API-equivalent value without draining the
// shared usage queue.
package usagestats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/hubsidecar"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

const (
	// FileName is the persistence file inside the auth directory.
	FileName = "usage-stats.json"
	// Retention is how long hourly buckets are kept.
	Retention = 30 * 24 * time.Hour
	// UnknownModel buckets records that carry no model name.
	UnknownModel = "unknown"

	pluginName        = "hub-usage-stats"
	defaultFlushDelay = 30 * time.Second
)

// Tokens are additive usage counters for one bucket or aggregate.
type Tokens struct {
	Requests              int64 `json:"requests"`
	InputTokens           int64 `json:"input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	CacheReadTokens       int64 `json:"cache_read_tokens"`
	CacheCreationTokens   int64 `json:"cache_creation_tokens"`
	CacheCreation5mTokens int64 `json:"cache_creation_5m_tokens"`
	CacheCreation1hTokens int64 `json:"cache_creation_1h_tokens"`
	// UncachedInputTokens is input billed at the full rate, normalized across
	// providers (see TokensFromRecord).
	UncachedInputTokens int64 `json:"uncached_input_tokens"`
}

// Add accumulates other into t.
func (t *Tokens) Add(other Tokens) {
	t.Requests += other.Requests
	t.InputTokens += other.InputTokens
	t.OutputTokens += other.OutputTokens
	t.CacheReadTokens += other.CacheReadTokens
	t.CacheCreationTokens += other.CacheCreationTokens
	t.CacheCreation5mTokens += other.CacheCreation5mTokens
	t.CacheCreation1hTokens += other.CacheCreation1hTokens
	t.UncachedInputTokens += other.UncachedInputTokens
}

// CacheHitRatio is cache_read / (cache_read + cache_creation + uncached_input),
// rounded to four decimals, or 0 when the denominator is 0.
func (t Tokens) CacheHitRatio() float64 {
	denominator := t.CacheReadTokens + t.CacheCreationTokens + t.UncachedInputTokens
	if denominator <= 0 || t.CacheReadTokens <= 0 {
		return 0
	}
	return math.Round(float64(t.CacheReadTokens)/float64(denominator)*10000) / 10000
}

// HourBucket is one hour of usage. Hour is the UTC hour start in Unix seconds.
type HourBucket struct {
	Hour int64 `json:"t"`
	Tokens
}

// AccountUsage is one credential's usage over a queried window.
type AccountUsage struct {
	AuthIndex string
	AuthID    string
	Provider  string
	Totals    Tokens
	// Series holds one bucket per hour (oldest first), zero-filled.
	Series []HourBucket
}

// TokensFromRecord converts one usage record into bucket counters.
//
// Providers disagree on whether input_tokens includes cached tokens: the Claude
// Messages API reports input_tokens excluding cache reads and cache writes,
// while Codex/OpenAI Responses (and Gemini) report input_tokens including
// cached tokens. InputTokens keeps the provider-reported value (matching the
// usage queue's tokens.input_tokens); UncachedInputTokens is normalized from
// the canonical v2 TokenBreakdown when it is complete, otherwise from the
// provider's known semantics, so the cache hit ratio is comparable across
// providers.
func TokensFromRecord(record coreusage.Record) Tokens {
	detail := coreusage.EnsureTokenBreakdownForProvider(record.Detail, record.Provider, record.ExecutorType)
	tokens := Tokens{
		Requests:            1,
		InputTokens:         nonNegative(detail.InputTokens),
		OutputTokens:        nonNegative(detail.OutputTokens),
		CacheReadTokens:     nonNegative(detail.CacheReadTokens),
		CacheCreationTokens: nonNegative(detail.CacheCreationTokens),
	}
	if detail.CacheCreation5mTokens != nil {
		tokens.CacheCreation5mTokens = nonNegative(*detail.CacheCreation5mTokens)
	}
	if detail.CacheCreation1hTokens != nil {
		tokens.CacheCreation1hTokens = nonNegative(*detail.CacheCreation1hTokens)
	}
	breakdown := detail.TokenBreakdown
	if breakdown.Valid() && breakdown.Quality == coreusage.TokenAccountingQualityComplete {
		tokens.CacheReadTokens = breakdown.Input.CacheReadTokens
		tokens.CacheCreationTokens = breakdown.Input.CacheWriteTokens
		tokens.UncachedInputTokens = breakdown.Input.UncachedTokens
		return tokens
	}
	if inputExcludesCache(record.Provider, record.ExecutorType) {
		tokens.UncachedInputTokens = tokens.InputTokens
		return tokens
	}
	if tokens.CacheReadTokens == 0 && detail.CachedTokens > 0 {
		tokens.CacheReadTokens = detail.CachedTokens
	}
	tokens.UncachedInputTokens = nonNegative(tokens.InputTokens - tokens.CacheReadTokens - tokens.CacheCreationTokens)
	return tokens
}

func inputExcludesCache(provider, executorType string) bool {
	value := strings.ToLower(provider + " " + executorType)
	return strings.Contains(value, "claude") || strings.Contains(value, "anthropic")
}

func nonNegative(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

type account struct {
	authID   string
	provider string
	buckets  map[int64]*Tokens
	// models holds the same usage split by model (model -> hour -> tokens).
	models map[string]map[int64]*Tokens
}

func newAccount() *account {
	return &account{buckets: make(map[int64]*Tokens), models: make(map[string]map[int64]*Tokens)}
}

func (a *account) modelBucket(model string, hour int64) *Tokens {
	if a.models == nil {
		a.models = make(map[string]map[int64]*Tokens)
	}
	hours := a.models[model]
	if hours == nil {
		hours = make(map[int64]*Tokens)
		a.models[model] = hours
	}
	bucket := hours[hour]
	if bucket == nil {
		bucket = &Tokens{}
		hours[hour] = bucket
	}
	return bucket
}

// ModelUsage is one credential's usage of one model over a queried window.
type ModelUsage struct {
	AuthIndex string
	AuthID    string
	Provider  string
	Model     string
	Totals    Tokens
}

// RecordModel returns the model a usage record is bucketed under.
func RecordModel(record coreusage.Record) string {
	for _, candidate := range []string{record.Model, record.ResponseModel, record.Alias} {
		if model := strings.ToLower(strings.TrimSpace(candidate)); model != "" {
			return model
		}
	}
	return UnknownModel
}

// Options configures an Aggregator.
type Options struct {
	// Now overrides the clock in tests.
	Now func() time.Time
	// FlushDelay is the persistence debounce (default 30s).
	FlushDelay time.Duration
	// Schedule overrides time.AfterFunc in tests; it returns a stop function.
	Schedule func(delay time.Duration, fn func()) (stop func() bool)
}

// Aggregator keeps hourly usage buckets per credential (keyed by auth_index)
// and per model, plus per-session usage, retains 30 days, and persists them
// atomically with a debounce.
type Aggregator struct {
	opts Options

	mu           sync.Mutex
	accounts     map[string]*account
	sessions     map[string]*sessionEntry
	path         string
	sessionsPath string
	loaded       bool
	dirty        bool
	stopTimer    func() bool

	writeMu sync.Mutex
}

var defaultAggregator = New(Options{})

// Default returns the process-wide aggregator used by the service and the
// management API.
func Default() *Aggregator { return defaultAggregator }

// New constructs an aggregator.
func New(opts Options) *Aggregator {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.FlushDelay <= 0 {
		opts.FlushDelay = defaultFlushDelay
	}
	if opts.Schedule == nil {
		opts.Schedule = func(delay time.Duration, fn func()) func() bool {
			return time.AfterFunc(delay, fn).Stop
		}
	}
	return &Aggregator{opts: opts, accounts: make(map[string]*account), sessions: make(map[string]*sessionEntry)}
}

// Start loads <authDir>/usage-stats.json and <authDir>/usage-sessions.json,
// enables persistence, and registers the aggregator as an in-process usage
// plugin (alongside, not instead of, the usage queue).
func (a *Aggregator) Start(authDir string) error {
	if a == nil {
		return nil
	}
	authDir = strings.TrimSpace(authDir)
	if authDir == "" {
		return errors.New("usage stats: auth directory is empty")
	}
	path := filepath.Join(authDir, FileName)
	sessionsPath := filepath.Join(authDir, SessionsFileName)
	a.mu.Lock()
	alreadyLoaded := a.loaded
	a.loaded = true
	a.mu.Unlock()
	var errLoad error
	if !alreadyLoaded {
		// A restarted service keeps its in-memory buckets instead of re-adding
		// the file contents on top of them.
		errLoad = a.load(path)
		if errSessions := a.loadSessions(sessionsPath); errSessions != nil && errLoad == nil {
			errLoad = errSessions
		}
	}
	a.mu.Lock()
	a.path = path
	a.sessionsPath = sessionsPath
	a.mu.Unlock()
	coreusage.RegisterNamedPlugin(pluginName, a)
	return errLoad
}

// Stop cancels the pending debounce and flushes synchronously. Records that
// arrive afterwards are kept in memory only.
func (a *Aggregator) Stop() error {
	if a == nil {
		return nil
	}
	errFlush := a.Flush()
	a.mu.Lock()
	if a.stopTimer != nil {
		a.stopTimer()
		a.stopTimer = nil
	}
	a.path = ""
	a.sessionsPath = ""
	a.mu.Unlock()
	return errFlush
}

// HandleUsage implements the usage plugin interface.
func (a *Aggregator) HandleUsage(ctx context.Context, record coreusage.Record) {
	_ = ctx
	if a == nil {
		return
	}
	authIndex := strings.TrimSpace(record.AuthIndex)
	if authIndex == "" {
		return
	}
	at := record.RequestedAt
	if at.IsZero() {
		at = a.opts.Now()
	}
	provider := strings.ToLower(strings.TrimSpace(record.Provider))
	model := RecordModel(record)
	tokens := TokensFromRecord(record)
	a.add(at, authIndex, strings.TrimSpace(record.AuthID), provider, model, tokens)
	if sessionKey := strings.TrimSpace(record.SessionID); sessionKey != "" {
		a.addSession(at, sessionKey, authIndex, provider, model, tokens)
	}
}

func (a *Aggregator) add(at time.Time, authIndex, authID, provider, model string, tokens Tokens) {
	hour := at.UTC().Truncate(time.Hour).Unix()
	a.mu.Lock()
	defer a.mu.Unlock()
	if hour < a.retentionCutoffLocked() {
		return
	}
	entry := a.accounts[authIndex]
	if entry == nil {
		entry = newAccount()
		a.accounts[authIndex] = entry
	}
	if authID != "" {
		entry.authID = authID
	}
	if provider != "" {
		entry.provider = provider
	}
	bucket := entry.buckets[hour]
	if bucket == nil {
		bucket = &Tokens{}
		entry.buckets[hour] = bucket
	}
	bucket.Add(tokens)
	if model == "" {
		model = UnknownModel
	}
	entry.modelBucket(model, hour).Add(tokens)
	a.markDirtyLocked()
}

func (a *Aggregator) markDirtyLocked() {
	a.dirty = true
	if a.path != "" && a.stopTimer == nil {
		a.stopTimer = a.opts.Schedule(a.opts.FlushDelay, a.flushFromTimer)
	}
}

func (a *Aggregator) flushFromTimer() {
	a.mu.Lock()
	a.stopTimer = nil
	a.mu.Unlock()
	if errFlush := a.Flush(); errFlush != nil {
		log.WithError(errFlush).Warn("usage stats: persist failed")
	}
}

func (a *Aggregator) retentionCutoffLocked() int64 {
	return a.opts.Now().UTC().Truncate(time.Hour).Add(-Retention).Unix()
}

func (a *Aggregator) pruneLocked() {
	cutoff := a.retentionCutoffLocked()
	for authIndex, entry := range a.accounts {
		for hour := range entry.buckets {
			if hour < cutoff {
				delete(entry.buckets, hour)
			}
		}
		for model, hours := range entry.models {
			for hour := range hours {
				if hour < cutoff {
					delete(hours, hour)
				}
			}
			if len(hours) == 0 {
				delete(entry.models, model)
			}
		}
		if len(entry.buckets) == 0 {
			delete(a.accounts, authIndex)
		}
	}
	a.pruneSessionsLocked(cutoff)
}

// Query returns usage for every credential with activity in the window. The
// series covers each hour that overlaps [now-window, now].
func (a *Aggregator) Query(window time.Duration) []AccountUsage {
	if a == nil {
		return nil
	}
	now := a.opts.Now().UTC()
	start := now.Add(-window).Truncate(time.Hour).Unix()
	end := now.Truncate(time.Hour).Unix()
	hours := int((end-start)/3600) + 1

	a.mu.Lock()
	defer a.mu.Unlock()
	a.pruneLocked()
	out := make([]AccountUsage, 0, len(a.accounts))
	for authIndex, entry := range a.accounts {
		usage := AccountUsage{AuthIndex: authIndex, AuthID: entry.authID, Provider: entry.provider, Series: make([]HourBucket, hours)}
		active := false
		for i := range usage.Series {
			hour := start + int64(i)*3600
			usage.Series[i].Hour = hour
			if bucket := entry.buckets[hour]; bucket != nil {
				usage.Series[i].Tokens = *bucket
				usage.Totals.Add(*bucket)
				active = true
			}
		}
		if active {
			out = append(out, usage)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AuthIndex < out[j].AuthIndex })
	return out
}

// QueryModels returns per-credential, per-model totals for every hour that
// overlaps [now-window, now], skipping idle combinations.
func (a *Aggregator) QueryModels(window time.Duration) []ModelUsage {
	if a == nil {
		return nil
	}
	now := a.opts.Now().UTC()
	start := now.Add(-window).Truncate(time.Hour).Unix()
	end := now.Truncate(time.Hour).Unix()

	a.mu.Lock()
	defer a.mu.Unlock()
	a.pruneLocked()
	var out []ModelUsage
	for authIndex, entry := range a.accounts {
		for model, hours := range entry.models {
			usage := ModelUsage{AuthIndex: authIndex, AuthID: entry.authID, Provider: entry.provider, Model: model}
			for hour, bucket := range hours {
				if hour >= start && hour <= end {
					usage.Totals.Add(*bucket)
				}
			}
			if usage.Totals.Requests > 0 {
				out = append(out, usage)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].AuthIndex != out[j].AuthIndex {
			return out[i].AuthIndex < out[j].AuthIndex
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// persistedModel is one model's hourly buckets inside a persisted account.
type persistedModel struct {
	Model   string       `json:"model"`
	Buckets []HourBucket `json:"buckets"`
}

// persistedAccount is one entry of usage-stats.json. The file is a top-level
// JSON array on purpose: it lives in the auth directory, and every credential
// loader only accepts JSON objects, so it can never be mistaken for a credential.
//
// Models was added later; files without it still load (per-model history then
// starts empty) and older builds ignore it.
type persistedAccount struct {
	AuthIndex string           `json:"auth_index"`
	AuthID    string           `json:"auth_id,omitempty"`
	Provider  string           `json:"provider,omitempty"`
	Buckets   []HourBucket     `json:"buckets"`
	Models    []persistedModel `json:"models,omitempty"`
}

func sortedHourBuckets(hours map[int64]*Tokens) []HourBucket {
	buckets := make([]HourBucket, 0, len(hours))
	for hour, bucket := range hours {
		buckets = append(buckets, HourBucket{Hour: hour, Tokens: *bucket})
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].Hour < buckets[j].Hour })
	return buckets
}

// Flush writes the current state when it changed since the last write.
func (a *Aggregator) Flush() error {
	if a == nil {
		return nil
	}
	a.writeMu.Lock()
	defer a.writeMu.Unlock()

	a.mu.Lock()
	path, sessionsPath := a.path, a.sessionsPath
	if path == "" || !a.dirty {
		a.mu.Unlock()
		return nil
	}
	a.pruneLocked()
	snapshot := make([]persistedAccount, 0, len(a.accounts))
	for authIndex, entry := range a.accounts {
		persisted := persistedAccount{AuthIndex: authIndex, AuthID: entry.authID, Provider: entry.provider, Buckets: sortedHourBuckets(entry.buckets)}
		for model, hours := range entry.models {
			persisted.Models = append(persisted.Models, persistedModel{Model: model, Buckets: sortedHourBuckets(hours)})
		}
		sort.Slice(persisted.Models, func(i, j int) bool { return persisted.Models[i].Model < persisted.Models[j].Model })
		snapshot = append(snapshot, persisted)
	}
	sessions := a.sessionSnapshotLocked()
	a.dirty = false
	a.mu.Unlock()

	sort.Slice(snapshot, func(i, j int) bool { return snapshot[i].AuthIndex < snapshot[j].AuthIndex })
	errWrite := hubsidecar.WriteJSONAtomic(path, snapshot)
	if errWrite == nil && sessionsPath != "" {
		errWrite = hubsidecar.WriteJSONAtomic(sessionsPath, sessions)
	}
	if errWrite != nil {
		a.mu.Lock()
		a.dirty = true
		a.mu.Unlock()
		return fmt.Errorf("usage stats: %w", errWrite)
	}
	return nil
}

func (a *Aggregator) load(path string) error {
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		if errors.Is(errRead, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("usage stats: read %s: %w", filepath.Base(path), errRead)
	}
	var persisted []persistedAccount
	if errUnmarshal := json.Unmarshal(data, &persisted); errUnmarshal != nil {
		return fmt.Errorf("usage stats: decode %s: %w", filepath.Base(path), errUnmarshal)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	cutoff := a.retentionCutoffLocked()
	for _, item := range persisted {
		authIndex := strings.TrimSpace(item.AuthIndex)
		if authIndex == "" {
			continue
		}
		entry := a.accounts[authIndex]
		if entry == nil {
			entry = newAccount()
			a.accounts[authIndex] = entry
		}
		if entry.authID == "" {
			entry.authID = item.AuthID
		}
		if entry.provider == "" {
			entry.provider = item.Provider
		}
		for _, bucket := range item.Buckets {
			if bucket.Hour < cutoff || bucket.Hour%3600 != 0 {
				continue
			}
			current := entry.buckets[bucket.Hour]
			if current == nil {
				current = &Tokens{}
				entry.buckets[bucket.Hour] = current
			}
			current.Add(bucket.Tokens)
		}
		for _, model := range item.Models {
			name := strings.ToLower(strings.TrimSpace(model.Model))
			if name == "" {
				name = UnknownModel
			}
			for _, bucket := range model.Buckets {
				if bucket.Hour < cutoff || bucket.Hour%3600 != 0 {
					continue
				}
				entry.modelBucket(name, bucket.Hour).Add(bucket.Tokens)
			}
		}
		if len(entry.buckets) == 0 {
			delete(a.accounts, authIndex)
		}
	}
	return nil
}
