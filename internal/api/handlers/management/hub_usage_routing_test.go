package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestats"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func serveHubHandler(t *testing.T, handler gin.HandlerFunc, target string) (int, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, target, nil)
	handler(ctx)
	var body map[string]any
	if errUnmarshal := json.Unmarshal(recorder.Body.Bytes(), &body); errUnmarshal != nil {
		t.Fatalf("decode %s response: %v (%s)", target, errUnmarshal, recorder.Body.String())
	}
	return recorder.Code, body
}

func registerHubTestAuth(t *testing.T, manager *coreauth.Manager, auth *coreauth.Auth) *coreauth.Auth {
	t.Helper()
	registered, errRegister := manager.Register(context.Background(), auth)
	if errRegister != nil || registered == nil {
		t.Fatalf("Register(%s) = %v, %v", auth.ID, registered, errRegister)
	}
	registered.EnsureIndex()
	return registered
}

func assertKeys(t *testing.T, what string, object map[string]any, keys ...string) {
	t.Helper()
	for _, key := range keys {
		if _, ok := object[key]; !ok {
			t.Fatalf("%s missing key %q: %#v", what, key, object)
		}
	}
}

func TestGetUsageAccountsJSONShape(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	claude := registerHubTestAuth(t, manager, &coreauth.Auth{ID: "claude-a.json", FileName: "claude-a.json", Provider: "claude", Metadata: map[string]any{"access_token": "tok"}})
	registerHubTestAuth(t, manager, &coreauth.Auth{ID: "codex-idle.json", FileName: "codex-idle.json", Provider: "codex", Metadata: map[string]any{"access_token": "tok"}})
	registerHubTestAuth(t, manager, &coreauth.Auth{ID: "claude-apikey", Provider: "claude", Attributes: map[string]string{"api_key": "sk-ant"}})

	stats := usagestats.New(usagestats.Options{})
	stats.HandleUsage(context.Background(), coreusage.Record{
		Provider:    "claude",
		AuthID:      claude.ID,
		AuthIndex:   claude.Index,
		RequestedAt: time.Now(),
		Detail:      coreusage.Detail{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 900, CacheCreationTokens: 50},
	})
	original := usageStatsSource
	usageStatsSource = func() *usagestats.Aggregator { return stats }
	t.Cleanup(func() { usageStatsSource = original })

	h := NewHandlerWithoutConfigFilePath(&config.Config{}, manager)
	status, body := serveHubHandler(t, h.GetUsageAccounts, "/v8/management/usage/accounts?window=1h")
	if status != http.StatusOK {
		t.Fatalf("status = %d body = %#v", status, body)
	}
	assertKeys(t, "response", body, "window", "generated_at", "accounts", "totals")
	if body["window"] != "1h" {
		t.Fatalf("window = %v", body["window"])
	}
	if _, errParse := time.Parse(time.RFC3339, body["generated_at"].(string)); errParse != nil {
		t.Fatalf("generated_at = %v", body["generated_at"])
	}
	accounts := body["accounts"].([]any)
	if len(accounts) != 2 {
		t.Fatalf("accounts = %#v, want the active claude and the idle codex OAuth account", accounts)
	}
	active := accounts[0].(map[string]any)
	assertKeys(t, "account", active, "auth_index", "auth_id", "provider", "label", "requests", "input_tokens", "output_tokens", "cache_read_tokens", "cache_creation_tokens", "cache_hit_ratio", "series")
	if active["auth_index"] != claude.Index || active["auth_id"] != "claude-a.json" || active["provider"] != "claude" || active["label"] != "claude-a.json" {
		t.Fatalf("active account = %#v", active)
	}
	if active["requests"].(float64) != 1 || active["cache_read_tokens"].(float64) != 900 || active["cache_hit_ratio"].(float64) != 0.8571 {
		t.Fatalf("active account counters = %#v", active)
	}
	series := active["series"].([]any)
	if len(series) != 2 {
		t.Fatalf("1h series = %#v, want two hourly points", series)
	}
	point := series[len(series)-1].(map[string]any)
	assertKeys(t, "series point", point, "t", "requests", "input", "cache_read", "cache_creation")
	if point["requests"].(float64) != 1 || point["cache_read"].(float64) != 900 {
		t.Fatalf("series point = %#v", point)
	}
	idle := accounts[1].(map[string]any)
	if idle["provider"] != "codex" || idle["requests"].(float64) != 0 || len(idle["series"].([]any)) != 2 {
		t.Fatalf("idle account = %#v", idle)
	}
	totals := body["totals"].(map[string]any)
	assertKeys(t, "totals", totals, "requests", "input_tokens", "output_tokens", "cache_read_tokens", "cache_creation_tokens", "cache_hit_ratio")
	if totals["requests"].(float64) != 1 || totals["cache_hit_ratio"].(float64) != 0.8571 {
		t.Fatalf("totals = %#v", totals)
	}

	if status, _ := serveHubHandler(t, h.GetUsageAccounts, "/v8/management/usage/accounts?window=30d"); status != http.StatusBadRequest {
		t.Fatalf("invalid window status = %d", status)
	}
}

func TestGetRoutingOrderJSONShape(t *testing.T) {
	now := time.Now().UTC()
	weekly := func(key, model string, used float64, resetIn time.Duration) coreauth.QuotaWindow {
		return coreauth.QuotaWindow{Key: key, Kind: "7d", Model: model, UsedPercent: used, ResetsAt: now.Add(resetIn), ObservedAt: now, Source: "poll"}
	}
	manager := coreauth.NewManager(nil, nil, nil)
	registerHubTestAuth(t, manager, &coreauth.Auth{ID: "claude-late.json", Provider: "claude", Metadata: map[string]any{"access_token": "tok"},
		Quota: coreauth.QuotaState{Windows: []coreauth.QuotaWindow{weekly("seven_day", "", 20, 96*time.Hour), weekly("seven_day_fable", "fable", 10, 12*time.Hour),
			{Key: "five_hour", Kind: "5h", UsedPercent: 15, ResetsAt: now.Add(2 * time.Hour), ObservedAt: now, Source: "headers"}}}})
	registerHubTestAuth(t, manager, &coreauth.Auth{ID: "claude-soon.json", Provider: "claude", Metadata: map[string]any{"access_token": "tok"},
		Quota: coreauth.QuotaState{Windows: []coreauth.QuotaWindow{weekly("seven_day", "", 42, 24*time.Hour), weekly("seven_day_fable", "fable", 10, 72*time.Hour)}}})
	registerHubTestAuth(t, manager, &coreauth.Auth{ID: "claude-nodata.json", Provider: "claude", Metadata: map[string]any{"access_token": "tok"}})
	registerHubTestAuth(t, manager, &coreauth.Auth{ID: "claude-off.json", Provider: "claude", Disabled: true, Status: coreauth.StatusDisabled, Metadata: map[string]any{"access_token": "tok"}})

	h := NewHandlerWithoutConfigFilePath(&config.Config{Routing: config.RoutingConfig{Strategy: "soonest-reset"}}, manager)
	status, body := serveHubHandler(t, h.GetRoutingOrder, "/v8/management/routing/order")
	if status != http.StatusOK {
		t.Fatalf("status = %d body = %#v", status, body)
	}
	assertKeys(t, "response", body, "strategy", "generated_at", "providers")
	if body["strategy"] != "soonest-reset" {
		t.Fatalf("strategy = %v", body["strategy"])
	}
	providers := body["providers"].(map[string]any)
	if codex, ok := providers["codex"].([]any); !ok || len(codex) != 0 {
		t.Fatalf("codex = %#v, want an empty list", providers["codex"])
	}
	claude := providers["claude"].([]any)
	gotOrder := make([]string, 0, len(claude))
	for _, raw := range claude {
		gotOrder = append(gotOrder, raw.(map[string]any)["auth_id"].(string))
	}
	wantOrder := []string{"claude-soon.json", "claude-late.json", "claude-nodata.json", "claude-off.json"}
	for i := range wantOrder {
		if gotOrder[i] != wantOrder[i] {
			t.Fatalf("order = %v, want %v", gotOrder, wantOrder)
		}
	}
	first := claude[0].(map[string]any)
	assertKeys(t, "entry", first, "auth_index", "auth_id", "label", "rank", "eligible", "skip_reason", "weekly_key", "weekly_used_percent", "weekly_resets_at", "five_hour_used_percent", "five_hour_resets_at", "disabled")
	if first["rank"].(float64) != 1 || first["eligible"] != true || first["weekly_key"] != "seven_day" || first["weekly_used_percent"].(float64) != 42 || first["five_hour_used_percent"] != nil || first["five_hour_resets_at"] != nil {
		t.Fatalf("first entry = %#v", first)
	}
	if _, errParse := time.Parse(time.RFC3339, first["weekly_resets_at"].(string)); errParse != nil {
		t.Fatalf("weekly_resets_at = %v", first["weekly_resets_at"])
	}
	noData := claude[2].(map[string]any)
	if noData["eligible"] != true || noData["weekly_resets_at"] != nil || noData["has_data"] != false {
		t.Fatalf("no-data entry = %#v", noData)
	}
	disabled := claude[3].(map[string]any)
	if disabled["disabled"] != true || disabled["eligible"] != false || disabled["skip_reason"] != "disabled" {
		t.Fatalf("disabled entry = %#v", disabled)
	}

	// A Fable model ranks by the Fable weekly bucket instead.
	_, body = serveHubHandler(t, h.GetRoutingOrder, "/v8/management/routing/order?model=claude-fable-5")
	first = body["providers"].(map[string]any)["claude"].([]any)[0].(map[string]any)
	if first["auth_id"] != "claude-late.json" || first["weekly_key"] != "seven_day_fable" || first["five_hour_used_percent"].(float64) != 15 {
		t.Fatalf("fable first entry = %#v", first)
	}
	if body["model"] != "claude-fable-5" {
		t.Fatalf("model = %v", body["model"])
	}
}
