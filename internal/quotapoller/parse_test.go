package quotapoller

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, errRead := os.ReadFile(filepath.Join("testdata", name))
	if errRead != nil {
		t.Fatalf("read fixture %s: %v", name, errRead)
	}
	return data
}

func windowsByKey(windows []coreauth.QuotaWindow) map[string]coreauth.QuotaWindow {
	out := make(map[string]coreauth.QuotaWindow, len(windows))
	for _, window := range windows {
		out[window.Key] = window
	}
	return out
}

func assertWindow(t *testing.T, windows map[string]coreauth.QuotaWindow, key, kind, model string, used float64, resetsAt time.Time) {
	t.Helper()
	window, ok := windows[key]
	if !ok {
		t.Fatalf("window %q missing; got %#v", key, windows)
	}
	if window.Kind != kind || window.Model != model || window.UsedPercent != used {
		t.Fatalf("window %q = kind %q model %q used %v; want kind %q model %q used %v", key, window.Kind, window.Model, window.UsedPercent, kind, model, used)
	}
	if !window.ResetsAt.Equal(resetsAt) {
		t.Fatalf("window %q resets_at = %s, want %s", key, window.ResetsAt, resetsAt)
	}
	if window.Source != coreauth.QuotaWindowSourcePoll {
		t.Fatalf("window %q source = %q, want poll", key, window.Source)
	}
}

func TestParseClaudeUsageModernPayload(t *testing.T) {
	now := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	windows, errParse := ParseClaudeUsage(readFixture(t, "claude_usage.json"), now)
	if errParse != nil {
		t.Fatalf("ParseClaudeUsage() error = %v", errParse)
	}
	byKey := windowsByKey(windows)
	if len(byKey) != 4 {
		t.Fatalf("windows = %#v, want five_hour, seven_day, seven_day_sonnet, seven_day_fable", byKey)
	}
	assertWindow(t, byKey, "five_hour", "5h", "", 42.5, time.Date(2026, 11, 10, 18, 0, 0, 0, time.UTC))
	assertWindow(t, byKey, "seven_day", "7d", "", 16, time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC))
	assertWindow(t, byKey, "seven_day_sonnet", "7d", "sonnet", 10, time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC))
	// The active weekly_scoped Fable limit wins over the inactive one; the
	// dollar-denominated iguana_necktie credit pool is not a Fable window.
	assertWindow(t, byKey, "seven_day_fable", "7d", "fable", 64, time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC))
	if !byKey["five_hour"].ObservedAt.Equal(now) {
		t.Fatalf("observed_at = %s, want %s", byKey["five_hour"].ObservedAt, now)
	}
}

func TestParseClaudeUsageLegacyFableField(t *testing.T) {
	now := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	windows, errParse := ParseClaudeUsage(readFixture(t, "claude_usage_legacy.json"), now)
	if errParse != nil {
		t.Fatalf("ParseClaudeUsage() error = %v", errParse)
	}
	byKey := windowsByKey(windows)
	assertWindow(t, byKey, "five_hour", "5h", "", 0, time.Time{})
	assertWindow(t, byKey, "seven_day", "7d", "", 71, time.Date(2026, 11, 12, 7, 59, 0, 0, time.UTC))
	assertWindow(t, byKey, "seven_day_opus", "7d", "opus", 55, time.Date(2026, 11, 12, 7, 59, 0, 0, time.UTC))
	assertWindow(t, byKey, "seven_day_fable", "7d", "fable", 41, time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC))
}

func TestParseCodexUsageProPayload(t *testing.T) {
	now := time.Unix(1785300000, 0).UTC()
	windows, errParse := ParseCodexUsage(readFixture(t, "codex_usage_pro.json"), now)
	if errParse != nil {
		t.Fatalf("ParseCodexUsage() error = %v", errParse)
	}
	byKey := windowsByKey(windows)
	if len(byKey) != 2 {
		t.Fatalf("windows = %#v, want seven_day and the Spark weekly window", byKey)
	}
	// A primary window with a weekly length is the weekly window (pro plans have no 5h window).
	assertWindow(t, byKey, "seven_day", "7d", "", 1, time.Unix(1785902974, 0).UTC())
	assertWindow(t, byKey, "seven_day_gpt-5.3-codex-spark", "7d", "gpt-5.3-codex-spark", 0, time.Unix(1785903197, 0).UTC())
}

func TestParseCodexUsageCamelCaseAndLimitReached(t *testing.T) {
	now := time.Unix(1785300000, 0).UTC()
	windows, errParse := ParseCodexUsage(readFixture(t, "codex_usage_plus.json"), now)
	if errParse != nil {
		t.Fatalf("ParseCodexUsage() error = %v", errParse)
	}
	byKey := windowsByKey(windows)
	if len(byKey) != 4 {
		t.Fatalf("windows = %#v, want main 5h/7d and bengalfox 5h/7d (code review ignored)", byKey)
	}
	assertWindow(t, byKey, "five_hour", "5h", "", 37, now.Add(time.Hour))
	assertWindow(t, byKey, "seven_day", "7d", "", 12.5, time.Unix(1786000000, 0).UTC())
	// limit_reached without used_percent counts as fully used, like the panel.
	assertWindow(t, byKey, "five_hour_codex-bengalfox", "5h", "codex-bengalfox", 100, now.Add(2*time.Minute))
	// Without a window length the secondary slot is the weekly window.
	assertWindow(t, byKey, "seven_day_codex-bengalfox", "7d", "codex-bengalfox", 63, time.Unix(1786000500, 0).UTC())
}

func TestParseUsageRejectsInvalidPayloads(t *testing.T) {
	for _, body := range []string{"", "null", "[]", "{not json", `"text"`} {
		if _, errParse := ParseClaudeUsage([]byte(body), time.Now()); errParse == nil {
			t.Fatalf("ParseClaudeUsage(%q) error = nil", body)
		}
		if _, errParse := ParseCodexUsage([]byte(body), time.Now()); errParse == nil {
			t.Fatalf("ParseCodexUsage(%q) error = nil", body)
		}
	}
	windows, errParse := ParseClaudeUsage([]byte(`{"five_hour":{"utilization":null,"resets_at":null}}`), time.Now())
	if errParse != nil || len(windows) != 0 {
		t.Fatalf("null utilization windows = %#v err = %v, want none", windows, errParse)
	}
}
