package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func quotaWindowMap(windows []QuotaWindow) map[string]QuotaWindow {
	out := make(map[string]QuotaWindow, len(windows))
	for _, window := range windows {
		out[window.Key] = window
	}
	return out
}

func TestMergeQuotaWindowsNewerObservationWinsPerKey(t *testing.T) {
	t0 := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	existing := []QuotaWindow{
		{Key: "seven_day", Kind: "7d", UsedPercent: 40, ResetsAt: t0.Add(48 * time.Hour), ObservedAt: t0, Source: QuotaWindowSourcePoll},
		{Key: "five_hour", Kind: "5h", UsedPercent: 10, ResetsAt: t0.Add(time.Hour), ObservedAt: t0.Add(time.Minute), Source: QuotaWindowSourceHeaders},
	}
	incoming := []QuotaWindow{
		// Older than the header observation: ignored.
		{Key: "five_hour", Kind: "5h", UsedPercent: 5, ResetsAt: t0.Add(time.Hour), ObservedAt: t0, Source: QuotaWindowSourcePoll},
		// Newer without a reset: keeps the previous future reset.
		{Key: "seven_day", Kind: "7d", UsedPercent: 45, ObservedAt: t0.Add(2 * time.Minute), Source: QuotaWindowSourceHeaders},
		// New key: added.
		{Key: "seven_day_fable", Kind: "7d", Model: "fable", UsedPercent: 64, ResetsAt: t0.Add(24 * time.Hour), ObservedAt: t0, Source: QuotaWindowSourcePoll},
	}
	existingCopy := cloneQuotaWindows(existing)

	merged, changed := MergeQuotaWindows(existing, incoming)
	if !changed {
		t.Fatal("MergeQuotaWindows() changed = false")
	}
	byKey := quotaWindowMap(merged)
	if got := byKey["five_hour"]; got.UsedPercent != 10 || got.Source != QuotaWindowSourceHeaders {
		t.Fatalf("five_hour = %#v, want the newer header observation kept", got)
	}
	if got := byKey["seven_day"]; got.UsedPercent != 45 || !got.ResetsAt.Equal(t0.Add(48*time.Hour)) {
		t.Fatalf("seven_day = %#v, want newer percent with carried reset", got)
	}
	if got := byKey["seven_day_fable"]; got.UsedPercent != 64 {
		t.Fatalf("seven_day_fable = %#v", got)
	}
	if len(merged) != 3 || merged[0].Key != "five_hour" || merged[2].Key != "seven_day_fable" {
		t.Fatalf("merged order = %#v, want sorted by key", merged)
	}
	for i := range existing {
		if !existing[i].equal(existingCopy[i]) {
			t.Fatal("MergeQuotaWindows mutated its input")
		}
	}
	if _, changedAgain := MergeQuotaWindows(merged, incoming); changedAgain {
		t.Fatal("re-merging identical observations reported a change")
	}
}

func TestQuotaWindowsFromClaudeUnifiedHeaders(t *testing.T) {
	now := time.Unix(1785300000, 0).UTC()
	headers := http.Header{
		"Anthropic-Ratelimit-Unified-5h-Utilization":    []string{"0.23"},
		"Anthropic-Ratelimit-Unified-5h-Reset":          []string{"1785310000"},
		"Anthropic-Ratelimit-Unified-7d-Utilization":    []string{"0.73"},
		"Anthropic-Ratelimit-Unified-7d-Reset":          []string{"1785700000"},
		"Anthropic-Ratelimit-Unified-7d_oi-Utilization": []string{"1.02"},
		"Anthropic-Ratelimit-Unified-7d_oi-Reset":       []string{"1785500000"},
		"Anthropic-Ratelimit-Unified-Status":            []string{"allowed"},
	}
	byKey := quotaWindowMap(QuotaWindowsFromHeaders("claude", headers, now, QuotaWindowSourceHeaders))
	if len(byKey) != 3 {
		t.Fatalf("windows = %#v", byKey)
	}
	if got := byKey["five_hour"]; got.Kind != "5h" || got.UsedPercent != 23 || !got.ResetsAt.Equal(time.Unix(1785310000, 0)) {
		t.Fatalf("five_hour = %#v", got)
	}
	if got := byKey["seven_day"]; got.Kind != "7d" || got.UsedPercent != 73 || got.Model != "" {
		t.Fatalf("seven_day = %#v", got)
	}
	if got := byKey["seven_day_fable"]; got.Model != "fable" || got.UsedPercent != 102 || got.RemainingPercent() != 0 || got.Source != QuotaWindowSourceHeaders {
		t.Fatalf("seven_day_fable = %#v", got)
	}
}

func TestQuotaWindowsFromCodexHeadersAndWebsocketEvents(t *testing.T) {
	now := time.Unix(1785300000, 0).UTC()
	headers := http.Header{
		"X-Codex-Primary-Used-Percent":                                  []string{"12"},
		"X-Codex-Primary-Window-Minutes":                                []string{"300"},
		"X-Codex-Primary-Reset-After-Seconds":                           []string{"600"},
		"X-Codex-Secondary-Used-Percent":                                []string{"40"},
		"X-Codex-Secondary-Window-Minutes":                              []string{"10080"},
		"X-Codex-Secondary-Reset-At":                                    []string{"1785800000"},
		"X-Codex-Bengalfox-Limit-Name":                                  []string{"GPT-5.3-Codex-Spark"},
		"X-Codex-Bengalfox-Secondary-Used-Percent":                      []string{"35"},
		"X-Codex-Additional-Gpt-5.3-Codex-Spark-Limit-Name":             []string{"GPT-5.3-Codex-Spark"},
		"X-Codex-Additional-Gpt-5.3-Codex-Spark-Primary-Used-Percent":   []string{"3"},
		"X-Codex-Additional-Gpt-5.3-Codex-Spark-Primary-Window-Minutes": []string{"300"},
		"X-Codex-Code-Review-Primary-Used-Percent":                      []string{"4"},
	}
	byKey := quotaWindowMap(QuotaWindowsFromHeaders("codex", headers, now, QuotaWindowSourceWS))
	if got := byKey["five_hour"]; got.UsedPercent != 12 || !got.ResetsAt.Equal(now.Add(10*time.Minute)) || got.Source != QuotaWindowSourceWS {
		t.Fatalf("five_hour = %#v", got)
	}
	if got := byKey["seven_day"]; got.UsedPercent != 40 || !got.ResetsAt.Equal(time.Unix(1785800000, 0)) {
		t.Fatalf("seven_day = %#v", got)
	}
	if got := byKey["seven_day_gpt-5.3-codex-spark"]; got.UsedPercent != 35 || got.Model != "gpt-5.3-codex-spark" {
		t.Fatalf("http additional window = %#v", got)
	}
	if got := byKey["five_hour_gpt-5.3-codex-spark"]; got.UsedPercent != 3 {
		t.Fatalf("websocket additional window = %#v", got)
	}
	for key := range byKey {
		if key == "five_hour_code-review" || key == "seven_day_code-review" {
			t.Fatalf("code review limit produced window %q", key)
		}
	}
	if len(QuotaWindowsFromHeaders("gemini", headers, now, QuotaWindowSourceHeaders)) != 0 {
		t.Fatal("unsupported provider produced windows")
	}
}

func TestMarkResultMergesHeaderWindowsWithPolledWindows(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth, errRegister := manager.Register(context.Background(), &Auth{ID: "claude-window-auth", Provider: "claude"})
	if errRegister != nil || auth == nil {
		t.Fatalf("Register() auth=%#v err=%v", auth, errRegister)
	}
	polledAt := time.Now().Add(-time.Minute).UTC()
	manager.ObserveQuotaWindows(auth.ID, []QuotaWindow{
		{Key: "seven_day", Kind: "7d", UsedPercent: 30, ResetsAt: polledAt.Add(72 * time.Hour), ObservedAt: polledAt, Source: QuotaWindowSourcePoll},
		{Key: "seven_day_fable", Kind: "7d", Model: "fable", UsedPercent: 50, ResetsAt: polledAt.Add(24 * time.Hour), ObservedAt: polledAt, Source: QuotaWindowSourcePoll},
	})

	reset := time.Now().Add(96 * time.Hour).Unix()
	ctx := internallogging.WithResponseHeadersHolder(context.Background())
	internallogging.SetResponseHeaders(ctx, http.Header{
		"Anthropic-Ratelimit-Unified-7d-Utilization": []string{"0.35"},
		"Anthropic-Ratelimit-Unified-7d-Reset":       []string{time.Unix(reset, 0).UTC().Format(time.RFC3339)},
	})
	manager.MarkResult(ctx, Result{AuthID: auth.ID, Provider: "claude", Model: "claude-fable-5", Success: true})

	updated, _ := manager.GetByID(auth.ID)
	byKey := quotaWindowMap(updated.Quota.Windows)
	if got := byKey["seven_day"]; got.UsedPercent != 35 || got.Source != QuotaWindowSourceHeaders || got.ResetsAt.Unix() != reset {
		t.Fatalf("seven_day = %#v, want header observation", got)
	}
	if got := byKey["seven_day_fable"]; got.UsedPercent != 50 || got.Source != QuotaWindowSourcePoll {
		t.Fatalf("seven_day_fable = %#v, want polled window untouched", got)
	}
}

func TestMarkResultTagsCodexWebsocketWindows(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth, errRegister := manager.Register(context.Background(), &Auth{
		ID:         "codex-ws-auth",
		Provider:   "codex",
		Attributes: map[string]string{"websockets": "true"},
	})
	if errRegister != nil || auth == nil {
		t.Fatalf("Register() auth=%#v err=%v", auth, errRegister)
	}
	ctx := cliproxyexecutor.WithDownstreamWebsocket(internallogging.WithResponseHeadersHolder(context.Background()))
	internallogging.MergeResponseHeaders(ctx, http.Header{
		"X-Codex-Secondary-Used-Percent":        []string{"22"},
		"X-Codex-Secondary-Window-Minutes":      []string{"10080"},
		"X-Codex-Secondary-Reset-After-Seconds": []string{"86400"},
	})
	manager.MarkResult(ctx, Result{AuthID: auth.ID, Provider: "codex", Model: "gpt-5.3-codex", Success: true})

	updated, _ := manager.GetByID(auth.ID)
	if got := quotaWindowMap(updated.Quota.Windows)["seven_day"]; got.UsedPercent != 22 || got.Source != QuotaWindowSourceWS {
		t.Fatalf("seven_day = %#v, want websocket-sourced window", got)
	}
}

func TestQuotaWindowsSurviveAuthUpdate(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth, errRegister := manager.Register(context.Background(), &Auth{
		ID:       "claude-update-auth",
		Provider: "claude",
		Metadata: map[string]any{"access_token": "old"},
	})
	if errRegister != nil || auth == nil {
		t.Fatalf("Register() auth=%#v err=%v", auth, errRegister)
	}
	observedAt := time.Now().UTC()
	manager.ObserveQuotaWindows(auth.ID, []QuotaWindow{
		{Key: "seven_day", Kind: "7d", UsedPercent: 30, ResetsAt: observedAt.Add(72 * time.Hour), ObservedAt: observedAt, Source: QuotaWindowSourcePoll},
	})

	// A token refresh or file reload rebuilds the auth without runtime state.
	if _, errUpdate := manager.Update(context.Background(), &Auth{
		ID:       auth.ID,
		Provider: "claude",
		Metadata: map[string]any{"access_token": "new"},
	}); errUpdate != nil {
		t.Fatalf("Update() error = %v", errUpdate)
	}
	updated, _ := manager.GetByID(auth.ID)
	if got := quotaWindowMap(updated.Quota.Windows)["seven_day"]; got.UsedPercent != 30 {
		t.Fatalf("windows after update = %#v, want carried over", updated.Quota.Windows)
	}

	clone := updated.Clone()
	clone.Quota.Windows[0].UsedPercent = 99
	if again, _ := manager.GetByID(auth.ID); again.Quota.Windows[0].UsedPercent != 30 {
		t.Fatal("Auth.Clone shares the windows slice")
	}
}

func TestQuotaWindowHelpers(t *testing.T) {
	if got := NormalizeQuotaModelFamily(" GPT-5.3 Codex_Spark "); got != "gpt-5.3-codex-spark" {
		t.Fatalf("NormalizeQuotaModelFamily() = %q", got)
	}
	if !QuotaWindowMatchesModel("fable", "claude-fable-5-20261001") || QuotaWindowMatchesModel("fable", "claude-opus-4-6") {
		t.Fatal("QuotaWindowMatchesModel() family matching is wrong")
	}
	if QuotaWindowKey(QuotaWindowKindFiveHour, "") != "five_hour" || QuotaWindowKey(QuotaWindowKindWeekly, "Fable") != "seven_day_fable" {
		t.Fatal("QuotaWindowKey() is wrong")
	}
	if QuotaWindowKindForSeconds(18000, true) != "5h" || QuotaWindowKindForSeconds(30*24*3600, false) != "7d" || QuotaWindowKindForSeconds(0, true) != "7d" {
		t.Fatal("QuotaWindowKindForSeconds() is wrong")
	}
	if at, ok := ParseQuotaResetTime("1785902974000"); !ok || at.Unix() != 1785902974 {
		t.Fatalf("ParseQuotaResetTime(ms) = %s %v", at, ok)
	}
	if at, ok := ParseQuotaResetTime("2026-07-27T10:00:00.000000+00:00"); !ok || !at.Equal(time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("ParseQuotaResetTime(iso) = %s %v", at, ok)
	}
}
