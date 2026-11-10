package usagestats

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/hubsidecar"
)

const (
	// SessionsFileName is the per-session persistence file inside the auth directory.
	SessionsFileName = hubsidecar.UsageSessionsFile
	// ColdCacheWriteThreshold is the cache-write size above which a request
	// after a session's first one counts as a cold prompt-cache write.
	ColdCacheWriteThreshold = 20_000
	// maxSessions bounds memory; the least recently seen sessions are dropped first.
	maxSessions = 20_000
)

// sessionBucket is one hour of one model's usage within a session.
type sessionBucket struct {
	Tokens
	ColdCacheWrites int64
}

type sessionEntry struct {
	provider    string
	firstSeen   time.Time
	lastSeen    time.Time
	authIndexes map[string]struct{}
	// requests counts the session's token-producing requests, so the first one
	// (which legitimately writes the whole prompt cache) is never counted as cold.
	requests int64
	buckets  map[int64]map[string]*sessionBucket
}

// SessionUsage is one session's usage over a queried window. FirstSeen and
// LastSeen span the session's whole retained history.
type SessionUsage struct {
	Key             string
	Provider        string
	FirstSeen       time.Time
	LastSeen        time.Time
	AuthIndexes     []string
	Totals          Tokens
	ColdCacheWrites int64
	// Models splits Totals by model so callers can price the session.
	Models map[string]Tokens
}

func (a *Aggregator) addSession(at time.Time, key, authIndex, provider, model string, tokens Tokens) {
	at = at.UTC()
	hour := at.Truncate(time.Hour).Unix()
	a.mu.Lock()
	defer a.mu.Unlock()
	if hour < a.retentionCutoffLocked() {
		return
	}
	entry := a.sessions[key]
	if entry == nil {
		if len(a.sessions) >= maxSessions {
			a.evictOldestSessionLocked()
		}
		entry = &sessionEntry{firstSeen: at, lastSeen: at, authIndexes: make(map[string]struct{}), buckets: make(map[int64]map[string]*sessionBucket)}
		a.sessions[key] = entry
	}
	if provider != "" {
		entry.provider = provider
	}
	if at.Before(entry.firstSeen) {
		entry.firstSeen = at
	}
	if at.After(entry.lastSeen) {
		entry.lastSeen = at
	}
	if authIndex != "" {
		entry.authIndexes[authIndex] = struct{}{}
	}
	bucket := entry.bucket(hour, model)
	bucket.Add(tokens)
	// Only requests that produced tokens advance the counter, so a failed
	// first attempt does not make the real first request look cold.
	if tokens.InputTokens+tokens.OutputTokens+tokens.CacheReadTokens+tokens.CacheCreationTokens > 0 {
		entry.requests++
		if entry.requests > 1 && tokens.CacheCreationTokens > ColdCacheWriteThreshold {
			bucket.ColdCacheWrites++
		}
	}
	a.markDirtyLocked()
}

func (e *sessionEntry) bucket(hour int64, model string) *sessionBucket {
	if model == "" {
		model = UnknownModel
	}
	models := e.buckets[hour]
	if models == nil {
		models = make(map[string]*sessionBucket)
		e.buckets[hour] = models
	}
	bucket := models[model]
	if bucket == nil {
		bucket = &sessionBucket{}
		models[model] = bucket
	}
	return bucket
}

func (a *Aggregator) evictOldestSessionLocked() {
	oldestKey := ""
	var oldest time.Time
	for key, entry := range a.sessions {
		if oldestKey == "" || entry.lastSeen.Before(oldest) {
			oldestKey, oldest = key, entry.lastSeen
		}
	}
	delete(a.sessions, oldestKey)
}

func (a *Aggregator) pruneSessionsLocked(cutoff int64) {
	for key, entry := range a.sessions {
		for hour := range entry.buckets {
			if hour < cutoff {
				delete(entry.buckets, hour)
			}
		}
		if len(entry.buckets) == 0 {
			delete(a.sessions, key)
		}
	}
}

// QuerySessions returns sessions with activity in [now-window, now], most
// recently seen first, at most limit entries (limit <= 0 means all).
func (a *Aggregator) QuerySessions(window time.Duration, limit int) []SessionUsage {
	if a == nil {
		return nil
	}
	now := a.opts.Now().UTC()
	start := now.Add(-window).Truncate(time.Hour).Unix()
	end := now.Truncate(time.Hour).Unix()

	a.mu.Lock()
	defer a.mu.Unlock()
	a.pruneLocked()
	out := make([]SessionUsage, 0, len(a.sessions))
	for key, entry := range a.sessions {
		usage := SessionUsage{Key: key, Provider: entry.provider, FirstSeen: entry.firstSeen, LastSeen: entry.lastSeen, Models: make(map[string]Tokens)}
		for hour, models := range entry.buckets {
			if hour < start || hour > end {
				continue
			}
			for model, bucket := range models {
				usage.Totals.Add(bucket.Tokens)
				usage.ColdCacheWrites += bucket.ColdCacheWrites
				modelTotals := usage.Models[model]
				modelTotals.Add(bucket.Tokens)
				usage.Models[model] = modelTotals
			}
		}
		if usage.Totals.Requests == 0 {
			continue
		}
		for authIndex := range entry.authIndexes {
			usage.AuthIndexes = append(usage.AuthIndexes, authIndex)
		}
		sort.Strings(usage.AuthIndexes)
		out = append(out, usage)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].LastSeen.Equal(out[j].LastSeen) {
			return out[i].LastSeen.After(out[j].LastSeen)
		}
		return out[i].Key < out[j].Key
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// persistedSession is one entry of usage-sessions.json (a top-level array).
type persistedSession struct {
	Key         string   `json:"key"`
	Provider    string   `json:"provider,omitempty"`
	FirstSeen   int64    `json:"first_seen"`
	LastSeen    int64    `json:"last_seen"`
	AuthIndexes []string `json:"auth_indexes,omitempty"`
	Requests    int64    `json:"requests"`
	// Models lists the model names that Buckets reference by index.
	Models  []string                 `json:"models"`
	Buckets []persistedSessionBucket `json:"buckets"`
}

// persistedSessionBucket is encoded as a compact array:
// [hour, model_index, requests, input, output, cache_read, cache_creation,
// cache_creation_5m, cache_creation_1h, uncached_input, cold_cache_writes].
type persistedSessionBucket struct {
	Hour       int64
	ModelIndex int
	Bucket     sessionBucket
}

func (b persistedSessionBucket) MarshalJSON() ([]byte, error) {
	t := b.Bucket.Tokens
	return json.Marshal([]int64{b.Hour, int64(b.ModelIndex), t.Requests, t.InputTokens, t.OutputTokens, t.CacheReadTokens,
		t.CacheCreationTokens, t.CacheCreation5mTokens, t.CacheCreation1hTokens, t.UncachedInputTokens, b.Bucket.ColdCacheWrites})
}

func (b *persistedSessionBucket) UnmarshalJSON(data []byte) error {
	var values []int64
	if errUnmarshal := json.Unmarshal(data, &values); errUnmarshal != nil {
		return errUnmarshal
	}
	if len(values) < 11 {
		return fmt.Errorf("session bucket has %d fields, want 11", len(values))
	}
	b.Hour, b.ModelIndex = values[0], int(values[1])
	b.Bucket = sessionBucket{Tokens: Tokens{
		Requests: values[2], InputTokens: values[3], OutputTokens: values[4], CacheReadTokens: values[5],
		CacheCreationTokens: values[6], CacheCreation5mTokens: values[7], CacheCreation1hTokens: values[8], UncachedInputTokens: values[9],
	}, ColdCacheWrites: values[10]}
	return nil
}

func (a *Aggregator) sessionSnapshotLocked() []persistedSession {
	out := make([]persistedSession, 0, len(a.sessions))
	for key, entry := range a.sessions {
		persisted := persistedSession{Key: key, Provider: entry.provider, FirstSeen: entry.firstSeen.Unix(), LastSeen: entry.lastSeen.Unix(), Requests: entry.requests}
		for authIndex := range entry.authIndexes {
			persisted.AuthIndexes = append(persisted.AuthIndexes, authIndex)
		}
		sort.Strings(persisted.AuthIndexes)
		modelIndex := make(map[string]int)
		for hour, models := range entry.buckets {
			for model, bucket := range models {
				index, ok := modelIndex[model]
				if !ok {
					index = len(persisted.Models)
					modelIndex[model] = index
					persisted.Models = append(persisted.Models, model)
				}
				persisted.Buckets = append(persisted.Buckets, persistedSessionBucket{Hour: hour, ModelIndex: index, Bucket: *bucket})
			}
		}
		sort.Slice(persisted.Buckets, func(i, j int) bool {
			if persisted.Buckets[i].Hour != persisted.Buckets[j].Hour {
				return persisted.Buckets[i].Hour < persisted.Buckets[j].Hour
			}
			return persisted.Buckets[i].ModelIndex < persisted.Buckets[j].ModelIndex
		})
		out = append(out, persisted)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func (a *Aggregator) loadSessions(path string) error {
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		if errors.Is(errRead, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("usage stats: read %s: %w", filepath.Base(path), errRead)
	}
	var persisted []persistedSession
	if errUnmarshal := json.Unmarshal(data, &persisted); errUnmarshal != nil {
		return fmt.Errorf("usage stats: decode %s: %w", filepath.Base(path), errUnmarshal)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	cutoff := a.retentionCutoffLocked()
	for _, item := range persisted {
		key := strings.TrimSpace(item.Key)
		if key == "" {
			continue
		}
		entry := a.sessions[key]
		if entry == nil {
			entry = &sessionEntry{firstSeen: time.Unix(item.FirstSeen, 0).UTC(), lastSeen: time.Unix(item.LastSeen, 0).UTC(), authIndexes: make(map[string]struct{}), buckets: make(map[int64]map[string]*sessionBucket)}
			a.sessions[key] = entry
		}
		if entry.provider == "" {
			entry.provider = item.Provider
		}
		entry.requests += item.Requests
		for _, authIndex := range item.AuthIndexes {
			entry.authIndexes[authIndex] = struct{}{}
		}
		for _, bucket := range item.Buckets {
			if bucket.Hour < cutoff || bucket.Hour%3600 != 0 || bucket.ModelIndex < 0 || bucket.ModelIndex >= len(item.Models) {
				continue
			}
			current := entry.bucket(bucket.Hour, item.Models[bucket.ModelIndex])
			current.Add(bucket.Bucket.Tokens)
			current.ColdCacheWrites += bucket.Bucket.ColdCacheWrites
		}
		if len(entry.buckets) == 0 {
			delete(a.sessions, key)
		}
	}
	return nil
}
