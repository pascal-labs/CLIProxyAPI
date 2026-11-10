package usagestats

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func withModelSession(record coreusage.Record, model, session string) coreusage.Record {
	record.Model = model
	record.SessionID = session
	return record
}

func TestAggregatorQueryModels(t *testing.T) {
	clock := &manualClock{now: time.Date(2026, 11, 10, 10, 30, 0, 0, time.UTC)}
	agg := New(Options{Now: clock.Now})
	ctx := context.Background()
	agg.HandleUsage(ctx, withModelSession(claudeRecord(clock.Now(), 100, 900, 50, 20), "Claude-Opus-5-5", ""))
	agg.HandleUsage(ctx, withModelSession(claudeRecord(clock.Now().Add(-2*time.Hour), 10, 0, 0, 2), "claude-opus-5-5", ""))
	agg.HandleUsage(ctx, withModelSession(claudeRecord(clock.Now(), 5, 0, 0, 1), "claude-fable-5-2", ""))
	agg.HandleUsage(ctx, withModelSession(codexRecord(clock.Now(), 300, 100, 30), "", ""))

	byKey := map[string]ModelUsage{}
	for _, usage := range agg.QueryModels(24 * time.Hour) {
		byKey[usage.AuthIndex+"/"+usage.Model] = usage
	}
	if len(byKey) != 3 {
		t.Fatalf("models = %#v", byKey)
	}
	opus := byKey["idx-claude/claude-opus-5-5"]
	if opus.Totals.Requests != 2 || opus.Totals.InputTokens != 110 || opus.Totals.CacheReadTokens != 900 || opus.Provider != "claude" || opus.AuthID != "claude-a.json" {
		t.Fatalf("opus = %+v", opus)
	}
	if unknown := byKey["idx-codex/"+UnknownModel]; unknown.Totals.UncachedInputTokens != 200 {
		t.Fatalf("codex without model = %+v", unknown)
	}
	if hour := agg.QueryModels(time.Hour); len(hour) != 3 {
		t.Fatalf("1h models = %#v", hour)
	} else {
		for _, usage := range hour {
			if usage.Model == "claude-opus-5-5" && usage.Totals.Requests != 1 {
				t.Fatalf("1h opus requests = %d, want 1", usage.Totals.Requests)
			}
		}
	}
}

func TestAggregatorSessions(t *testing.T) {
	clock := &manualClock{now: time.Date(2026, 11, 10, 10, 30, 0, 0, time.UTC)}
	agg := New(Options{Now: clock.Now})
	ctx := context.Background()
	start := clock.Now().Add(-3 * time.Hour)
	// A failed attempt without tokens does not count as the session's first request.
	failed := withModelSession(claudeRecord(start.Add(-time.Second), 0, 0, 0, 0), "claude-fable-5-2", "claude:s1")
	failed.Failed = true
	agg.HandleUsage(ctx, failed)
	// The first request writes the whole cache (not cold); a later 30k write is cold.
	agg.HandleUsage(ctx, withModelSession(claudeRecord(start, 10, 0, 90_000, 5), "claude-fable-5-2", "claude:s1"))
	agg.HandleUsage(ctx, withModelSession(claudeRecord(start.Add(time.Minute), 10, 90_000, 1_000, 5), "claude-fable-5-2", "claude:s1"))
	moved := withModelSession(claudeRecord(clock.Now(), 10, 0, 30_000, 5), "claude-opus-5-5", "claude:s1")
	moved.AuthIndex = "idx-claude-2"
	agg.HandleUsage(ctx, moved)
	agg.HandleUsage(ctx, withModelSession(codexRecord(clock.Now().Add(-30*time.Hour), 100, 0, 1), "gpt-6.1-sol", "codex:old"))

	sessions := agg.QuerySessions(24*time.Hour, 0)
	if len(sessions) != 1 {
		t.Fatalf("24h sessions = %#v", sessions)
	}
	s1 := sessions[0]
	if s1.Key != "claude:s1" || s1.Totals.Requests != 4 || s1.ColdCacheWrites != 1 || s1.Provider != "claude" {
		t.Fatalf("session = %+v", s1)
	}
	if strings.Join(s1.AuthIndexes, ",") != "idx-claude,idx-claude-2" || !s1.FirstSeen.Equal(start.Add(-time.Second)) || !s1.LastSeen.Equal(clock.Now()) {
		t.Fatalf("session identity = %+v", s1)
	}
	if s1.Models["claude-fable-5-2"].Requests != 3 || s1.Models["claude-opus-5-5"].CacheCreationTokens != 30_000 {
		t.Fatalf("session models = %+v", s1.Models)
	}
	if week := agg.QuerySessions(7*24*time.Hour, 0); len(week) != 2 || week[0].Key != "claude:s1" {
		t.Fatalf("7d sessions = %#v", week)
	}
	if limited := agg.QuerySessions(7*24*time.Hour, 1); len(limited) != 1 {
		t.Fatalf("limit ignored: %d", len(limited))
	}
}

func TestAggregatorPersistsModelsAndSessions(t *testing.T) {
	dir := t.TempDir()
	clock := &manualClock{now: time.Date(2026, 11, 10, 10, 30, 0, 0, time.UTC)}
	scheduler := &manualScheduler{}
	agg := New(Options{Now: clock.Now, Schedule: scheduler.Schedule})
	if errStart := agg.Start(dir); errStart != nil {
		t.Fatal(errStart)
	}
	ctx := context.Background()
	agg.HandleUsage(ctx, withModelSession(claudeRecord(clock.Now(), 10, 0, 90_000, 5), "claude-fable-5-2", "claude:s1"))
	agg.HandleUsage(ctx, withModelSession(claudeRecord(clock.Now(), 10, 0, 30_000, 5), "claude-fable-5-2", "claude:s1"))
	if errFlush := agg.Flush(); errFlush != nil {
		t.Fatal(errFlush)
	}
	sessionsData, errRead := os.ReadFile(filepath.Join(dir, SessionsFileName))
	if errRead != nil || !strings.HasPrefix(string(sessionsData), "[") {
		t.Fatalf("sessions file = %q, %v", sessionsData, errRead)
	}

	reloaded := New(Options{Now: clock.Now, Schedule: scheduler.Schedule})
	if errStart := reloaded.Start(dir); errStart != nil {
		t.Fatal(errStart)
	}
	if models := reloaded.QueryModels(time.Hour); len(models) != 1 || models[0].Totals.CacheCreationTokens != 120_000 {
		t.Fatalf("reloaded models = %#v", models)
	}
	sessions := reloaded.QuerySessions(time.Hour, 0)
	if len(sessions) != 1 || sessions[0].Totals.Requests != 2 || sessions[0].ColdCacheWrites != 1 {
		t.Fatalf("reloaded sessions = %#v", sessions)
	}
	// The lifetime request count survives, so the next big write is still cold.
	reloaded.HandleUsage(ctx, withModelSession(claudeRecord(clock.Now(), 10, 0, 25_000, 5), "claude-fable-5-2", "claude:s1"))
	if sessions = reloaded.QuerySessions(time.Hour, 0); sessions[0].ColdCacheWrites != 2 {
		t.Fatalf("cold writes after reload = %d, want 2", sessions[0].ColdCacheWrites)
	}
}

// usage-stats.json written before per-model buckets existed must still load.
func TestAggregatorLoadsLegacyFileWithoutModels(t *testing.T) {
	dir := t.TempDir()
	clock := &manualClock{now: time.Date(2026, 11, 10, 10, 30, 0, 0, time.UTC)}
	hour := clock.Now().Truncate(time.Hour).Unix()
	legacy := `[{"auth_index":"idx-claude","auth_id":"claude-a.json","provider":"claude","buckets":[{"t":` +
		strconv.FormatInt(hour, 10) + `,"requests":3,"input_tokens":30,"output_tokens":6,"cache_read_tokens":0,"cache_creation_tokens":0,"cache_creation_5m_tokens":0,"cache_creation_1h_tokens":0,"uncached_input_tokens":30}]}]`
	if errWrite := os.WriteFile(filepath.Join(dir, FileName), []byte(legacy), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	agg := New(Options{Now: clock.Now, Schedule: (&manualScheduler{}).Schedule})
	if errStart := agg.Start(dir); errStart != nil {
		t.Fatal(errStart)
	}
	if accounts := agg.Query(time.Hour); len(accounts) != 1 || accounts[0].Totals.Requests != 3 {
		t.Fatalf("legacy accounts = %#v", accounts)
	}
	if models := agg.QueryModels(time.Hour); len(models) != 0 {
		t.Fatalf("legacy file produced model rows: %#v", models)
	}
}
