package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/hubevents"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotahistory"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/sessionmeta"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestats"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

const (
	fixtureClaudeSession = "3f2a6c1e-8b7d-4e21-9a55-0c1d2e3f4a5b"
	fixtureCodexSession  = "01a1c6f6-0664-7130-8a1b-2c3d4e5f6a7b"
)

type hubFixture struct {
	handler   *Handler
	claudeA   *coreauth.Auth
	claudeB   *coreauth.Auth
	codexA    *coreauth.Auth
	now       time.Time
	authDir   string
	sessionRt string
}

func writeFixtureFile(t *testing.T, path, content string) {
	t.Helper()
	if errMkdir := os.MkdirAll(filepath.Dir(path), 0o700); errMkdir != nil {
		t.Fatal(errMkdir)
	}
	if errWrite := os.WriteFile(path, []byte(content), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
}

func fixtureRecord(at time.Time, auth *coreauth.Auth, model, session string, input, cacheRead, cacheCreation, output int64) coreusage.Record {
	record := coreusage.Record{
		Provider:    auth.Provider,
		AuthID:      auth.ID,
		AuthIndex:   auth.Index,
		Model:       model,
		SessionID:   session,
		RequestedAt: at,
		Detail: coreusage.Detail{
			InputTokens:         input,
			OutputTokens:        output,
			CacheReadTokens:     cacheRead,
			CachedTokens:        cacheRead,
			CacheCreationTokens: cacheCreation,
		},
	}
	if auth.Provider == "claude" {
		fiveMinute := cacheCreation
		record.Detail.CacheCreation5mTokens = &fiveMinute
	}
	return record
}

func newHubFixture(t *testing.T) *hubFixture {
	t.Helper()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	root := t.TempDir()
	authDir := filepath.Join(root, "auths")
	claudeRoot := filepath.Join(root, "claude-home")
	codexRoot := filepath.Join(root, "codex-home")

	manager := coreauth.NewManager(nil, nil, nil)
	oauth := map[string]any{"access_token": "fixture"}
	claudeA := registerHubTestAuth(t, manager, &coreauth.Auth{ID: "claude-user-a.json", FileName: "claude-user-a.json", Provider: "claude", Metadata: oauth})
	claudeB := registerHubTestAuth(t, manager, &coreauth.Auth{ID: "claude-user-b.json", FileName: "claude-user-b.json", Provider: "claude", Metadata: oauth})
	codexA := registerHubTestAuth(t, manager, &coreauth.Auth{ID: "codex-user-a.json", FileName: "codex-user-a.json", Provider: "codex", Metadata: oauth})

	// Usage: a Fable session on claude A (second request rewrote 30k cache
	// tokens = one cold write), an Opus session on claude B, a Codex session.
	stats := usagestats.New(usagestats.Options{Now: clock})
	ctx := context.Background()
	claudeKey, codexKey := "claude:"+fixtureClaudeSession, "codex:"+fixtureCodexSession
	stats.HandleUsage(ctx, fixtureRecord(now.Add(-3*time.Hour), claudeA, "claude-fable-5-2", claudeKey, 10, 0, 90_000, 5))
	stats.HandleUsage(ctx, fixtureRecord(now.Add(-20*time.Minute), claudeA, "claude-fable-5-2", claudeKey, 10, 90_000, 30_000, 5))
	stats.HandleUsage(ctx, fixtureRecord(now.Add(-10*time.Minute), claudeB, "claude-opus-5-5", "claude:aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee:agent:a1", 200, 4_000, 0, 50))
	stats.HandleUsage(ctx, fixtureRecord(now.Add(-5*time.Minute), codexA, "gpt-6.1-sol", codexKey, 1_000, 400, 0, 50))

	// Quota windows and history for claude A.
	fiveReset, weekReset := now.Add(2*time.Hour), now.Add(3*24*time.Hour)
	history := quotahistory.New(quotahistory.Options{Now: clock})
	record := func(at time.Time, windows ...coreauth.QuotaWindow) {
		for i := range windows {
			windows[i].ObservedAt = at
		}
		history.Record(claudeA.ID, claudeA.Index, "claude", windows)
	}
	prevFiveReset := now.Add(-3 * time.Hour)
	record(now.Add(-5*time.Hour), coreauth.QuotaWindow{Key: "five_hour", Kind: "5h", UsedPercent: 55, ResetsAt: prevFiveReset})
	record(now.Add(-3*time.Hour-10*time.Minute), coreauth.QuotaWindow{Key: "five_hour", Kind: "5h", UsedPercent: 70, ResetsAt: prevFiveReset})
	// Weekly windows need 3h of current-cycle history before they are projected;
	// these sit on the same lines as the samples below.
	record(now.Add(-4*time.Hour),
		coreauth.QuotaWindow{Key: "seven_day", Kind: "7d", UsedPercent: 57, ResetsAt: weekReset},
		coreauth.QuotaWindow{Key: "seven_day_fable", Kind: "7d", Model: "fable", UsedPercent: 0, ResetsAt: weekReset},
	)
	for i := 0; i <= 4; i++ {
		at := now.Add(-2*time.Hour + time.Duration(i)*30*time.Minute)
		record(at,
			coreauth.QuotaWindow{Key: "five_hour", Kind: "5h", UsedPercent: 30 + 3*float64(i), ResetsAt: fiveReset},
			coreauth.QuotaWindow{Key: "seven_day", Kind: "7d", UsedPercent: 59 + 0.5*float64(i), ResetsAt: weekReset},
			coreauth.QuotaWindow{Key: "seven_day_fable", Kind: "7d", Model: "fable", UsedPercent: 40 + 10*float64(i), ResetsAt: weekReset},
		)
	}
	manager.ObserveQuotaWindows(claudeA.ID, []coreauth.QuotaWindow{
		{Key: "five_hour", Kind: "5h", UsedPercent: 42, ResetsAt: fiveReset, ObservedAt: now, Source: "poll"},
		{Key: "seven_day", Kind: "7d", UsedPercent: 61, ResetsAt: weekReset, ObservedAt: now, Source: "poll"},
		{Key: "seven_day_fable", Kind: "7d", Model: "fable", UsedPercent: 80, ResetsAt: weekReset, ObservedAt: now, Source: "poll"},
	})
	manager.ObserveQuotaWindows(codexA.ID, []coreauth.QuotaWindow{
		{Key: "seven_day", Kind: "7d", UsedPercent: 12, ResetsAt: weekReset, ObservedAt: now, Source: "headers"},
	})

	events := hubevents.New(hubevents.Options{Now: clock})
	events.Record(hubevents.Event{T: now.Add(-50 * time.Minute), Kind: hubevents.KindHubStart, Detail: "hub started"})
	events.Record(hubevents.Event{T: now.Add(-40 * time.Minute), Kind: hubevents.KindRateLimited, AuthID: claudeA.ID, Provider: "claude", Model: "claude-fable-5-2", SessionKey: claudeKey, StatusCode: 429, Detail: "quota"})
	events.Record(hubevents.Event{T: now.Add(-40 * time.Minute), Kind: hubevents.KindCooldown, AuthID: claudeA.ID, Provider: "claude", Model: "claude-fable-5-2", SessionKey: claudeKey, StatusCode: 429, Detail: "until 2026-11-10T11:25:00Z (rate_limited)"})
	events.Record(hubevents.Event{T: now.Add(-39 * time.Minute), Kind: hubevents.KindFailover, AuthID: claudeB.ID, RelatedAuthID: claudeA.ID, Provider: "claude", Model: "claude-fable-5-2", SessionKey: claudeKey})
	events.Record(hubevents.Event{T: now.Add(-5 * time.Minute), Kind: hubevents.KindNetworkError, AuthID: claudeA.ID, Provider: "claude", Model: "claude-opus-5-5", Detail: "timeout"})
	events.Record(hubevents.Event{T: now.Add(-50 * time.Hour), Kind: hubevents.KindDeadLogin, AuthID: codexA.ID, Provider: "codex", StatusCode: 401, Detail: "unauthorized"})

	// Local agent files and the T3 export.
	writeFixtureFile(t, filepath.Join(claudeRoot, "projects", "-work-hub", fixtureClaudeSession+".jsonl"),
		`{"type":"custom-title","customTitle":"Hub dashboard","sessionId":"`+fixtureClaudeSession+`"}`+"\n"+
			`{"type":"user","cwd":"/work/hub","sessionId":"`+fixtureClaudeSession+`"}`+"\n")
	writeFixtureFile(t, filepath.Join(codexRoot, "sessions", "2026", "11", "10", "rollout-2026-11-10T11-55-00-"+fixtureCodexSession+".jsonl"),
		`{"type":"session_meta","payload":{"id":"`+fixtureCodexSession+`","cwd":"/work/poller"}}`+"\n")
	writeFixtureFile(t, filepath.Join(codexRoot, "session_index.jsonl"), `{"id":"`+fixtureCodexSession+`","thread_name":"Poller backoff","updated_at":"2026-11-10T11:56:00Z"}`+"\n")
	writeFixtureFile(t, filepath.Join(authDir, "t3-threads.json"), `{"generated_at":"2026-11-10T11:59:00Z","threads":{
		"`+fixtureClaudeSession+`":{"t3_thread_id":"t3-main","title":"Dashboard work","project":"hub","parent_t3_thread_id":null,"parent_title":null,"subagent":false},
		"`+fixtureCodexSession+`":{"t3_thread_id":"t3-sub","title":"Poller fix","project":"poller","parent_t3_thread_id":"t3-main","parent_title":"Dashboard work","subagent":true}}}`)

	cfg := &config.Config{AuthDir: authDir}
	cfg.Hub.SessionRoots = []string{claudeRoot, codexRoot}
	cfg.Hub.Subscriptions = map[string]float64{"claude-user-a.json": 200, codexA.Index: 20}

	originalStats, originalHistory, originalEvents, originalMeta, originalNow := usageStatsSource, quotaHistorySource, hubEventsSource, sessionMetaSource, hubNow
	resolver := sessionmeta.NewResolver(nil)
	usageStatsSource = func() *usagestats.Aggregator { return stats }
	quotaHistorySource = func() *quotahistory.Store { return history }
	hubEventsSource = func() *hubevents.Store { return events }
	sessionMetaSource = func() *sessionmeta.Resolver { return resolver }
	hubNow = clock
	t.Cleanup(func() {
		usageStatsSource, quotaHistorySource, hubEventsSource, sessionMetaSource, hubNow = originalStats, originalHistory, originalEvents, originalMeta, originalNow
	})
	return &hubFixture{handler: NewHandlerWithoutConfigFilePath(cfg, manager), claudeA: claudeA, claudeB: claudeB, codexA: codexA, now: now, authDir: authDir, sessionRt: root}
}

// serveHubJSON runs a handler and returns the decoded body; with
// HUB_DASHBOARD_DUMP_DIR set it also writes the pretty JSON there.
func serveHubJSON(t *testing.T, handler gin.HandlerFunc, target string) map[string]any {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, target, nil)
	handler(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("%s: status %d body %s", target, recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if errUnmarshal := json.Unmarshal(recorder.Body.Bytes(), &body); errUnmarshal != nil {
		t.Fatalf("%s: %v", target, errUnmarshal)
	}
	if dir := os.Getenv("HUB_DASHBOARD_DUMP_DIR"); dir != "" {
		pretty, _ := json.MarshalIndent(body, "", "  ")
		name := strings.NewReplacer("/v8/management/", "", "/", "_", "?", "_", "=", "-", "&", "_").Replace(target) + ".json"
		_ = os.WriteFile(filepath.Join(dir, name), pretty, 0o600)
	}
	return body
}

func rows(t *testing.T, body map[string]any, key string) []map[string]any {
	t.Helper()
	raw, ok := body[key].([]any)
	if !ok {
		t.Fatalf("%s is %T, want array: %#v", key, body[key], body)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		out = append(out, item.(map[string]any))
	}
	return out
}

func TestHubDashboardUsageModels(t *testing.T) {
	fixture := newHubFixture(t)
	body := serveHubJSON(t, fixture.handler.GetUsageModels, "/v8/management/usage/models?window=24h")
	assertKeys(t, "response", body, "window", "generated_at", "rows", "unpriced_models")
	if body["window"] != "24h" || body["generated_at"] != "2026-11-10T12:00:00Z" {
		t.Fatalf("header = %v %v", body["window"], body["generated_at"])
	}
	modelRows := rows(t, body, "rows")
	if len(modelRows) != 3 {
		t.Fatalf("rows = %#v", modelRows)
	}
	byModel := map[string]map[string]any{}
	for _, row := range modelRows {
		assertKeys(t, "row", row, "auth_index", "provider", "model", "requests", "input_tokens", "uncached_input_tokens", "output_tokens",
			"cache_read_tokens", "cache_creation_5m_tokens", "cache_creation_1h_tokens", "cache_hit_ratio", "api_value_usd")
		byModel[row["model"].(string)] = row
	}
	fable := byModel["claude-fable-5-2"]
	// (20 uncached x 8 + 10 out x 40 + 90k read x 0.20 + 120k 5m writes x 10) / 1e6
	if fable["api_value_usd"] != 1.2186 || fable["requests"] != 2.0 || fable["auth_index"] != fixture.claudeA.Index || fable["cache_creation_5m_tokens"] != 120000.0 {
		t.Fatalf("fable row = %#v", fable)
	}
	// 90k cache reads x (8 input - 0.20 cache read) / 1e6.
	if fable["cache_savings_usd"] != 0.702 {
		t.Fatalf("fable cache savings = %v", fable["cache_savings_usd"])
	}
	if codex := byModel["gpt-6.1-sol"]; codex["api_value_usd"] != nil || codex["cache_savings_usd"] != nil || codex["uncached_input_tokens"] != 600.0 || codex["provider"] != "codex" {
		t.Fatalf("codex row = %#v", codex)
	}
	if unpriced := body["unpriced_models"].([]any); len(unpriced) != 1 || unpriced[0] != "gpt-6.1-sol" {
		t.Fatalf("unpriced = %#v", unpriced)
	}
	if hour := rows(t, serveHubJSON(t, fixture.handler.GetUsageModels, "/v8/management/usage/models?window=1h"), "rows"); len(hour) != 3 {
		t.Fatalf("1h rows = %d", len(hour))
	}
}

func TestHubDashboardUsageSessions(t *testing.T) {
	fixture := newHubFixture(t)
	body := serveHubJSON(t, fixture.handler.GetUsageSessions, "/v8/management/usage/sessions?window=24h&limit=10")
	assertKeys(t, "response", body, "window", "generated_at", "sessions")
	sessions := rows(t, body, "sessions")
	if len(sessions) != 3 {
		t.Fatalf("sessions = %#v", sessions)
	}
	byKey := map[string]map[string]any{}
	for _, session := range sessions {
		assertKeys(t, "session", session, "session_key", "provider", "client", "project", "cwd", "title", "auth_indexes", "first_seen", "last_seen",
			"requests", "input_tokens", "output_tokens", "cache_read_tokens", "cache_creation_tokens", "cache_hit_ratio", "cold_cache_writes", "api_value_usd",
			"t3_thread_id", "t3_title", "t3_parent_title", "subagent", "models", "what_if_usd")
		byKey[session["session_key"].(string)] = session
	}
	if sessions[0]["session_key"] != "codex:"+fixtureCodexSession {
		t.Fatalf("sessions not ordered by last_seen: %v", sessions[0]["session_key"])
	}
	claude := byKey["claude:"+fixtureClaudeSession]
	if claude["client"] != "claude-code" || claude["project"] != "hub" || claude["cwd"] != "/work/hub" || claude["title"] != "Hub dashboard" ||
		claude["cold_cache_writes"] != 1.0 || claude["requests"] != 2.0 || claude["api_value_usd"] != 1.2186 || claude["first_seen"] != "2026-11-10T09:00:00Z" {
		t.Fatalf("claude session = %#v", claude)
	}
	if claude["t3_thread_id"] != "t3-main" || claude["t3_title"] != "Dashboard work" || claude["t3_parent_title"] != nil || claude["subagent"] != false {
		t.Fatalf("claude t3 = %#v", claude)
	}
	claudeModels := rows(t, claude, "models")
	if len(claudeModels) != 1 {
		t.Fatalf("claude models = %#v", claudeModels)
	}
	assertKeys(t, "session model", claudeModels[0], "model", "requests", "input_tokens", "output_tokens", "cache_read_tokens", "cache_creation_tokens", "api_value_usd")
	if m := claudeModels[0]; m["model"] != "claude-fable-5-2" || m["requests"] != 2.0 || m["cache_read_tokens"] != 90000.0 || m["cache_creation_tokens"] != 120000.0 || m["api_value_usd"] != 1.2186 {
		t.Fatalf("claude model split = %#v", m)
	}
	// Same tokens on Opus 5.5: (20x4 + 10x20 + 90k x0.20 + 120k x5) / 1e6; on Sonnet 5.5:
	// (20x2 + 10x10 + 90k x0.10 + 120k x2.5) / 1e6.
	whatIf, ok := claude["what_if_usd"].(map[string]any)
	if !ok || len(whatIf) != 2 || whatIf["claude-opus-5-5"] != 0.6183 || whatIf["claude-sonnet-5-5"] != 0.3091 {
		t.Fatalf("claude what_if = %#v", claude["what_if_usd"])
	}
	codex := byKey["codex:"+fixtureCodexSession]
	if codex["client"] != "codex" || codex["cwd"] != "/work/poller" || codex["title"] != "Poller backoff" || codex["api_value_usd"] != nil ||
		codex["t3_parent_title"] != "Dashboard work" || codex["subagent"] != true {
		t.Fatalf("codex session = %#v", codex)
	}
	if codexModels := rows(t, codex, "models"); len(codexModels) != 1 || codexModels[0]["model"] != "gpt-6.1-sol" || codexModels[0]["api_value_usd"] != nil || codex["what_if_usd"] != nil {
		t.Fatalf("codex split = %#v what_if = %#v", codexModels, codex["what_if_usd"])
	}
	unmatched := byKey["claude:aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee:agent:a1"]
	if unmatched["project"] != nil || unmatched["title"] != nil || unmatched["t3_thread_id"] != nil || unmatched["subagent"] != nil || unmatched["native_session_id"] != "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee" {
		t.Fatalf("unmatched session = %#v", unmatched)
	}
	if limited := rows(t, serveHubJSON(t, fixture.handler.GetUsageSessions, "/v8/management/usage/sessions?window=7d&limit=1"), "sessions"); len(limited) != 1 {
		t.Fatalf("limit ignored: %d", len(limited))
	}
}

func TestHubDashboardUsageRunway(t *testing.T) {
	fixture := newHubFixture(t)
	body := serveHubJSON(t, fixture.handler.GetUsageRunway, "/v8/management/usage/runway")
	assertKeys(t, "response", body, "generated_at", "accounts")
	accounts := rows(t, body, "accounts")
	if len(accounts) != 3 {
		t.Fatalf("accounts = %#v", accounts)
	}
	var claudeA map[string]any
	for _, account := range accounts {
		assertKeys(t, "account", account, "auth_index", "provider", "label", "windows", "last_cycles")
		if account["auth_index"] == fixture.claudeA.Index {
			claudeA = account
		}
	}
	windows := rows(t, claudeA, "windows")
	if len(windows) != 3 {
		t.Fatalf("windows = %#v", windows)
	}
	byKey := map[string]map[string]any{}
	for _, window := range windows {
		assertKeys(t, "window", window, "key", "kind", "used_percent", "resets_at", "burn_rate_pct_per_hour", "projected_exhaust_at", "exhausts_before_reset",
			"projection_status", "learning_until", "history")
		byKey[window["key"].(string)] = window
	}
	five := byKey["five_hour"]
	// 30 -> 42 over two hours = 6 %/h: 100% would take ~9.7h, after the 2h reset.
	if five["kind"] != "five_hour" || five["burn_rate_pct_per_hour"] != 6.0 || five["exhausts_before_reset"] != false || five["projected_exhaust_at"] != nil ||
		five["projection_status"] != "ok" || five["learning_until"] != nil {
		t.Fatalf("five_hour = %#v", five)
	}
	fable := byKey["seven_day_fable"]
	// 0 -> 80 over four hours = 20 %/h: exhausted one hour from now, days before reset.
	if fable["kind"] != "weekly" || fable["burn_rate_pct_per_hour"] != 20.0 || fable["exhausts_before_reset"] != true || fable["projected_exhaust_at"] != "2026-11-10T13:00:00Z" ||
		fable["projection_status"] != "ok" {
		t.Fatalf("seven_day_fable = %#v", fable)
	}
	if history := fable["history"].([]any); len(history) != 6 {
		t.Fatalf("history = %#v", history)
	}
	cycles := rows(t, claudeA, "last_cycles")
	if len(cycles) != 1 || cycles[0]["window_key"] != "five_hour" || cycles[0]["final_used_percent"] != 70.0 || cycles[0]["reset_at"] != "2026-11-10T09:00:00Z" {
		t.Fatalf("last_cycles = %#v", cycles)
	}
}

func TestHubDashboardRunwayLearningAndIdle(t *testing.T) {
	fixture := newHubFixture(t)
	now := fixture.now
	history := quotaHistorySource()
	fiveReset, weekReset := now.Add(3*time.Hour), now.Add(5*24*time.Hour)
	record := func(at time.Time, windows ...coreauth.QuotaWindow) {
		for i := range windows {
			windows[i].ObservedAt = at
		}
		history.Record(fixture.claudeB.ID, fixture.claudeB.Index, "claude", windows)
	}
	// A fresh start: 20 minutes of fast-rising samples must not project anything.
	record(now.Add(-20*time.Minute), coreauth.QuotaWindow{Key: "five_hour", Kind: "5h", UsedPercent: 10, ResetsAt: fiveReset})
	record(now.Add(-10*time.Minute), coreauth.QuotaWindow{Key: "five_hour", Kind: "5h", UsedPercent: 20, ResetsAt: fiveReset})
	// Four flat hours on the weekly window: enough history, no pace.
	for _, hoursAgo := range []int{4, 2} {
		record(now.Add(-time.Duration(hoursAgo)*time.Hour), coreauth.QuotaWindow{Key: "seven_day", Kind: "7d", UsedPercent: 15, ResetsAt: weekReset})
	}
	// A weekly window first seen two hours ago: learning until 3h after that.
	record(now.Add(-2*time.Hour), coreauth.QuotaWindow{Key: "seven_day_fable", Kind: "7d", Model: "fable", UsedPercent: 5, ResetsAt: weekReset})
	fixture.handler.authManager.ObserveQuotaWindows(fixture.claudeB.ID, []coreauth.QuotaWindow{
		{Key: "five_hour", Kind: "5h", UsedPercent: 30, ResetsAt: fiveReset, ObservedAt: now, Source: "poll"},
		{Key: "seven_day", Kind: "7d", UsedPercent: 15, ResetsAt: weekReset, ObservedAt: now, Source: "poll"},
		{Key: "seven_day_fable", Kind: "7d", Model: "fable", UsedPercent: 25, ResetsAt: weekReset, ObservedAt: now, Source: "poll"},
	})

	body := serveHubJSON(t, fixture.handler.GetUsageRunway, "/v8/management/usage/runway")
	byKey := map[string]map[string]any{}
	for _, account := range rows(t, body, "accounts") {
		if account["auth_index"] != fixture.claudeB.Index {
			continue
		}
		for _, window := range rows(t, account, "windows") {
			byKey[window["key"].(string)] = window
		}
	}
	five := byKey["five_hour"]
	// Three samples over 20 minutes: ready one hour after the first.
	if five["projection_status"] != "learning" || five["learning_until"] != "2026-11-10T12:40:00Z" ||
		five["burn_rate_pct_per_hour"] != nil || five["projected_exhaust_at"] != nil || five["exhausts_before_reset"] != false {
		t.Fatalf("five_hour = %#v", five)
	}
	if fable := byKey["seven_day_fable"]; fable["projection_status"] != "learning" || fable["learning_until"] != "2026-11-10T13:00:00Z" || fable["burn_rate_pct_per_hour"] != nil {
		t.Fatalf("seven_day_fable = %#v", fable)
	}
	if weekly := byKey["seven_day"]; weekly["projection_status"] != "idle" || weekly["learning_until"] != nil || weekly["burn_rate_pct_per_hour"] != 0.0 || weekly["exhausts_before_reset"] != false {
		t.Fatalf("seven_day = %#v", weekly)
	}

	// Learning that cannot finish before the reset resumes in the next cycle.
	samples := []quotahistory.Sample{{T: now.Add(-10 * time.Minute), UsedPercent: 90, ResetsAt: now.Add(20 * time.Minute)}}
	if until := runwayLearningUntil(samples, now.Add(20*time.Minute), "five_hour", now); !until.Equal(now.Add(80 * time.Minute)) {
		t.Fatalf("learning across a reset = %v", until)
	}
	if until := runwayLearningUntil(nil, time.Time{}, "weekly", now); !until.Equal(now.Add(3 * time.Hour)) {
		t.Fatalf("learning without samples = %v", until)
	}
}

func TestHubDashboardUsageValue(t *testing.T) {
	fixture := newHubFixture(t)
	body := serveHubJSON(t, fixture.handler.GetUsageValue, "/v8/management/usage/value?window=30d")
	assertKeys(t, "response", body, "window", "generated_at", "accounts", "totals", "unpriced_models")
	accounts := rows(t, body, "accounts")
	byIndex := map[string]map[string]any{}
	for _, account := range accounts {
		assertKeys(t, "account", account, "auth_index", "provider", "label", "api_value_usd", "subscription_usd_per_month", "subscription_usd_for_window", "value_ratio",
			"cache_savings_usd", "no_cache_value_usd")
		byIndex[account["auth_index"].(string)] = account
	}
	a := byIndex[fixture.claudeA.Index]
	if a["api_value_usd"] != 1.2186 || a["subscription_usd_per_month"] != 200.0 || a["subscription_usd_for_window"] != 200.0 || a["value_ratio"] != 0.0061 {
		t.Fatalf("claude A = %#v", a)
	}
	// Without caching every prompt token is fresh input: (20 + 90k + 120k) x 8 + 10 x 40.
	if a["cache_savings_usd"] != 0.702 || a["no_cache_value_usd"] != 1.6806 {
		t.Fatalf("claude A cache = %v / %v", a["cache_savings_usd"], a["no_cache_value_usd"])
	}
	b := byIndex[fixture.claudeB.Index]
	if b["subscription_usd_per_month"] != nil || b["value_ratio"] != nil || b["api_value_usd"] == nil {
		t.Fatalf("claude B = %#v", b)
	}
	codex := byIndex[fixture.codexA.Index]
	if codex["api_value_usd"] != nil || codex["has_unpriced_usage"] != true || codex["subscription_usd_for_window"] != 20.0 || codex["value_ratio"] != nil ||
		codex["cache_savings_usd"] != nil || codex["no_cache_value_usd"] != nil {
		t.Fatalf("codex = %#v", codex)
	}
	totals := body["totals"].(map[string]any)
	assertKeys(t, "totals", totals, "api_value_usd", "subscription_usd_for_window", "value_ratio", "cache_savings_usd", "no_cache_value_usd", "has_unpriced_usage")
	// Claude B adds 4k Opus reads x 3.80 and (200 + 4k) x 4 + 50 x 20 without caching.
	if totals["subscription_usd_for_window"] != 220.0 || totals["cache_savings_usd"] != 0.7172 || totals["no_cache_value_usd"] != 1.6984 || totals["has_unpriced_usage"] != true {
		t.Fatalf("totals = %#v", totals)
	}
	week := serveHubJSON(t, fixture.handler.GetUsageValue, "/v8/management/usage/value?window=7d")
	for _, account := range rows(t, week, "accounts") {
		if account["auth_index"] == fixture.claudeA.Index && account["subscription_usd_for_window"] != 46.6667 {
			t.Fatalf("7d prorated subscription = %v", account["subscription_usd_for_window"])
		}
	}
}

func TestHubDashboardUsageEvents(t *testing.T) {
	fixture := newHubFixture(t)
	body := serveHubJSON(t, fixture.handler.GetUsageEvents, "/v8/management/usage/events?window=24h&limit=3")
	assertKeys(t, "response", body, "window", "generated_at", "events", "counts")
	events := rows(t, body, "events")
	if len(events) != 3 {
		t.Fatalf("events = %#v", events)
	}
	for _, event := range events {
		assertKeys(t, "event", event, "t", "kind", "auth_index", "provider", "model", "session_key", "status_code", "detail")
		encoded, _ := json.Marshal(event)
		if strings.Contains(string(encoded), ".json") {
			t.Fatalf("event leaks a credential file name: %s", encoded)
		}
	}
	if events[0]["kind"] != "network_error" || events[0]["auth_index"] != fixture.claudeA.Index || events[0]["status_code"] != nil {
		t.Fatalf("newest event = %#v", events[0])
	}
	if events[1]["kind"] != "failover" || events[1]["auth_index"] != fixture.claudeB.Index || events[1]["detail"] != "bound auth_index="+fixture.claudeA.Index {
		t.Fatalf("failover event = %#v", events[1])
	}
	counts := rows(t, body, "counts")
	total := 0
	for _, count := range counts {
		assertKeys(t, "count", count, "hour", "kind", "count")
		total += int(count["count"].(float64))
	}
	if total != 5 {
		t.Fatalf("counts cover %d events, want all 5 in the window (not only the limited list)", total)
	}
	week := rows(t, serveHubJSON(t, fixture.handler.GetUsageEvents, "/v8/management/usage/events?window=7d"), "events")
	if len(week) != 6 || week[5]["kind"] != "dead_login" {
		t.Fatalf("7d events = %#v", week)
	}
}

func TestHubDashboardRejectsBadWindows(t *testing.T) {
	fixture := newHubFixture(t)
	for _, tc := range []struct {
		handler gin.HandlerFunc
		target  string
	}{
		{fixture.handler.GetUsageModels, "/x?window=2h"},
		{fixture.handler.GetUsageSessions, "/x?window=1h"},
		{fixture.handler.GetUsageValue, "/x?window=1h"},
		{fixture.handler.GetUsageEvents, "/x?window=30d"},
	} {
		if status, _ := serveHubHandler(t, tc.handler, tc.target); status != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400", tc.target, status)
		}
	}
}
