package usagestats

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

type manualClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) Set(now time.Time) {
	c.mu.Lock()
	c.now = now
	c.mu.Unlock()
}

type manualScheduler struct {
	mu      sync.Mutex
	pending []func()
	delays  []time.Duration
}

func (s *manualScheduler) Schedule(delay time.Duration, fn func()) func() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = append(s.pending, fn)
	s.delays = append(s.delays, delay)
	return func() bool { return true }
}

func (s *manualScheduler) Fire() int {
	s.mu.Lock()
	pending := s.pending
	s.pending = nil
	s.mu.Unlock()
	for _, fn := range pending {
		fn()
	}
	return len(pending)
}

func claudeRecord(at time.Time, input, cacheRead, cacheCreation, output int64) coreusage.Record {
	fiveMinute := cacheCreation
	return coreusage.Record{
		Provider:    "claude",
		AuthID:      "claude-a.json",
		AuthIndex:   "idx-claude",
		RequestedAt: at,
		Detail: coreusage.Detail{
			InputTokens:           input,
			OutputTokens:          output,
			CacheReadTokens:       cacheRead,
			CachedTokens:          cacheRead,
			CacheCreationTokens:   cacheCreation,
			CacheCreation5mTokens: &fiveMinute,
		},
	}
}

func codexRecord(at time.Time, input, cached, output int64) coreusage.Record {
	return coreusage.Record{
		Provider:    "codex",
		AuthID:      "codex-a.json",
		AuthIndex:   "idx-codex",
		RequestedAt: at,
		Detail: coreusage.Detail{
			InputTokens:     input,
			OutputTokens:    output,
			CachedTokens:    cached,
			CacheReadTokens: cached,
		},
	}
}

func usageByIndex(usage []AccountUsage) map[string]AccountUsage {
	out := make(map[string]AccountUsage, len(usage))
	for _, entry := range usage {
		out[entry.AuthIndex] = entry
	}
	return out
}

func TestTokensFromRecordNormalizesUncachedInputPerProvider(t *testing.T) {
	at := time.Date(2026, 11, 10, 9, 15, 0, 0, time.UTC)
	// Claude: input_tokens already excludes cache reads and writes.
	claude := TokensFromRecord(claudeRecord(at, 100, 900, 50, 20))
	if claude.InputTokens != 100 || claude.UncachedInputTokens != 100 || claude.CacheReadTokens != 900 || claude.CacheCreationTokens != 50 || claude.CacheCreation5mTokens != 50 {
		t.Fatalf("claude tokens = %+v", claude)
	}
	if got := claude.CacheHitRatio(); got != 0.8571 {
		t.Fatalf("claude cache hit ratio = %v, want 0.8571", got)
	}
	// Codex: input_tokens includes cached tokens.
	codex := TokensFromRecord(codexRecord(at, 1000, 800, 30))
	if codex.InputTokens != 1000 || codex.UncachedInputTokens != 200 || codex.CacheReadTokens != 800 {
		t.Fatalf("codex tokens = %+v", codex)
	}
	if got := codex.CacheHitRatio(); got != 0.8 {
		t.Fatalf("codex cache hit ratio = %v, want 0.8", got)
	}
	if (Tokens{}).CacheHitRatio() != 0 {
		t.Fatal("empty ratio should be 0")
	}
}

func TestAggregatorBucketsHourlyAndQueriesWindows(t *testing.T) {
	clock := &manualClock{now: time.Date(2026, 11, 10, 10, 20, 0, 0, time.UTC)}
	agg := New(Options{Now: clock.Now})
	ctx := context.Background()
	agg.HandleUsage(ctx, claudeRecord(time.Date(2026, 11, 10, 10, 5, 0, 0, time.UTC), 100, 900, 50, 20))
	agg.HandleUsage(ctx, claudeRecord(time.Date(2026, 11, 10, 10, 15, 0, 0, time.UTC), 10, 90, 0, 5))
	agg.HandleUsage(ctx, claudeRecord(time.Date(2026, 11, 10, 8, 59, 0, 0, time.UTC), 1, 0, 0, 1))
	agg.HandleUsage(ctx, codexRecord(time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC), 1000, 800, 30))
	agg.HandleUsage(ctx, coreusage.Record{Provider: "claude", Detail: coreusage.Detail{InputTokens: 5}})

	lastHour := usageByIndex(agg.Query(time.Hour))
	if len(lastHour) != 1 {
		t.Fatalf("1h accounts = %#v, want only the claude account", lastHour)
	}
	claude := lastHour["idx-claude"]
	if len(claude.Series) != 2 || claude.Series[0].Hour != time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC).Unix() {
		t.Fatalf("1h series = %+v, want the 09:00 and 10:00 buckets", claude.Series)
	}
	if claude.Series[0].Requests != 0 || claude.Series[1].Requests != 2 || claude.Series[1].CacheReadTokens != 990 {
		t.Fatalf("1h series = %+v", claude.Series)
	}
	if claude.Totals.Requests != 2 || claude.Totals.InputTokens != 110 || claude.AuthID != "claude-a.json" || claude.Provider != "claude" {
		t.Fatalf("1h totals = %+v", claude)
	}

	day := usageByIndex(agg.Query(24 * time.Hour))
	if len(day) != 2 || len(day["idx-claude"].Series) != 25 || day["idx-claude"].Totals.Requests != 3 {
		t.Fatalf("24h usage = %#v", day)
	}
	if codex := day["idx-codex"]; codex.Totals.UncachedInputTokens != 200 || codex.Totals.Requests != 1 {
		t.Fatalf("24h codex = %+v", codex.Totals)
	}
	if week := usageByIndex(agg.Query(7 * 24 * time.Hour)); len(week["idx-codex"].Series) != 7*24+1 {
		t.Fatalf("7d series length = %d", len(week["idx-codex"].Series))
	}
}

func TestAggregatorRetention(t *testing.T) {
	clock := &manualClock{now: time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)}
	agg := New(Options{Now: clock.Now})
	agg.HandleUsage(context.Background(), claudeRecord(clock.Now().Add(-15*24*time.Hour), 1, 0, 0, 1))
	if len(agg.Query(30*24*time.Hour)) != 0 {
		t.Fatal("record older than the retention window was kept")
	}
	agg.HandleUsage(context.Background(), claudeRecord(clock.Now().Add(-13*24*time.Hour), 1, 0, 0, 1))
	if len(agg.Query(30*24*time.Hour)) != 1 {
		t.Fatal("record inside the retention window was dropped")
	}
	clock.Set(clock.Now().Add(2 * 24 * time.Hour))
	if len(agg.Query(30*24*time.Hour)) != 0 {
		t.Fatal("bucket was not pruned once it aged past retention")
	}
}

func TestAggregatorPersistsWithDebounceAndReloads(t *testing.T) {
	dir := t.TempDir()
	clock := &manualClock{now: time.Date(2026, 11, 10, 10, 20, 0, 0, time.UTC)}
	scheduler := &manualScheduler{}
	agg := New(Options{Now: clock.Now, Schedule: scheduler.Schedule})
	if errStart := agg.Start(dir); errStart != nil {
		t.Fatalf("Start() error = %v", errStart)
	}
	path := filepath.Join(dir, FileName)

	agg.HandleUsage(context.Background(), claudeRecord(clock.Now(), 100, 900, 50, 20))
	agg.HandleUsage(context.Background(), codexRecord(clock.Now(), 1000, 800, 30))
	if _, errStat := os.Stat(path); !os.IsNotExist(errStat) {
		t.Fatalf("file written before the debounce fired: %v", errStat)
	}
	if len(scheduler.delays) != 1 || scheduler.delays[0] != 30*time.Second {
		t.Fatalf("scheduled flushes = %v, want one 30s debounce", scheduler.delays)
	}
	if fired := scheduler.Fire(); fired != 1 {
		t.Fatalf("fired %d flushes", fired)
	}
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read persisted file: %v", errRead)
	}
	// The file sits in the auth directory, so it must never parse as a credential object.
	var asObject map[string]any
	if json.Unmarshal(data, &asObject) == nil {
		t.Fatalf("persisted file is a JSON object; credential loaders would pick it up: %s", data)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, ".usage-stats-*.tmp")); len(matches) != 0 {
		t.Fatalf("temp files left behind: %v", matches)
	}

	// A later record schedules a new debounce; Stop flushes it synchronously.
	agg.HandleUsage(context.Background(), claudeRecord(clock.Now(), 1, 0, 0, 1))
	if errStop := agg.Stop(); errStop != nil {
		t.Fatalf("Stop() error = %v", errStop)
	}

	reloaded := New(Options{Now: clock.Now, Schedule: (&manualScheduler{}).Schedule})
	if errStart := reloaded.Start(dir); errStart != nil {
		t.Fatalf("reload Start() error = %v", errStart)
	}
	defer func() { _ = reloaded.Stop() }()
	byIndex := usageByIndex(reloaded.Query(24 * time.Hour))
	if got := byIndex["idx-claude"].Totals; got.Requests != 2 || got.CacheReadTokens != 900 || got.UncachedInputTokens != 101 || got.CacheCreation5mTokens != 50 {
		t.Fatalf("reloaded claude totals = %+v", got)
	}
	if got := byIndex["idx-codex"]; got.Totals.UncachedInputTokens != 200 || got.Provider != "codex" || got.AuthID != "codex-a.json" {
		t.Fatalf("reloaded codex usage = %+v", got)
	}
}

func TestAggregatorStartRejectsCorruptFileButKeepsRunning(t *testing.T) {
	dir := t.TempDir()
	if errWrite := os.WriteFile(filepath.Join(dir, FileName), []byte("{broken"), 0o600); errWrite != nil {
		t.Fatalf("write corrupt file: %v", errWrite)
	}
	agg := New(Options{Schedule: (&manualScheduler{}).Schedule})
	if errStart := agg.Start(dir); errStart == nil {
		t.Fatal("Start() accepted a corrupt file")
	}
	agg.HandleUsage(context.Background(), claudeRecord(time.Now(), 1, 0, 0, 1))
	if errStop := agg.Stop(); errStop != nil {
		t.Fatalf("Stop() error = %v", errStop)
	}
	data, errRead := os.ReadFile(filepath.Join(dir, FileName))
	if errRead != nil || len(data) == 0 || data[0] != '[' {
		t.Fatalf("corrupt file was not replaced: %q %v", data, errRead)
	}
}

func TestStartRegistersAlongsideUsageQueuePlugin(t *testing.T) {
	agg := New(Options{Schedule: (&manualScheduler{}).Schedule})
	if errStart := agg.Start(t.TempDir()); errStart != nil {
		t.Fatalf("Start() error = %v", errStart)
	}
	defer func() { _ = agg.Stop() }()

	record := claudeRecord(time.Now(), 7, 0, 0, 3)
	record.AuthIndex = "idx-published"
	coreusage.PublishRecord(context.Background(), record)

	// Delivery is asynchronous; wait for the dispatcher instead of sleeping a fixed time.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if usage := usageByIndex(agg.Query(time.Hour)); usage["idx-published"].Totals.Requests == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("published usage record never reached the aggregator")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
