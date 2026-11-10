package quotapoller

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/MoeForge/moego/ojson"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

var errInvalidUsagePayload = errors.New("invalid usage payload")

// claudeNamedWindows mirrors CLAUDE_USAGE_WINDOW_KEYS in the management panel
// (src/utils/quota/constants.ts). "five_hour" is the rolling window; every other
// key on the payload is a weekly window. iguana_necktie is the legacy Fable
// field unless it is dollar-denominated (then it is a credit pool, not a window).
var claudeNamedWindows = []struct {
	key    string
	kind   string
	family string
	window string
}{
	{key: "five_hour", kind: coreauth.QuotaWindowKindFiveHour},
	{key: "seven_day", kind: coreauth.QuotaWindowKindWeekly},
	{key: "seven_day_oauth_apps", kind: coreauth.QuotaWindowKindWeekly, window: "seven_day_oauth_apps"},
	{key: "seven_day_cowork", kind: coreauth.QuotaWindowKindWeekly, window: "seven_day_cowork"},
	{key: "seven_day_opus", kind: coreauth.QuotaWindowKindWeekly, family: "opus"},
	{key: "seven_day_sonnet", kind: coreauth.QuotaWindowKindWeekly, family: "sonnet"},
	{key: "iguana_necktie", kind: coreauth.QuotaWindowKindWeekly, family: "fable"},
}

// ParseClaudeUsage converts a GET /api/oauth/usage payload into quota windows.
//
// Field mapping (see management-center src/features/quota/providers/claude/data.ts):
//   - five_hour / seven_day / seven_day_{opus,sonnet,oauth_apps,cowork}:
//     {utilization: percent, resets_at: RFC 3339}
//   - iguana_necktie: legacy Fable weekly window, skipped when it carries
//     limit_dollars/used_dollars/remaining_dollars (cloud session credits)
//   - limits[]: {kind, percent, resets_at, is_active, scope.model.display_name};
//     "weekly_scoped" with a model is a model-scoped weekly window (Fable) and
//     overrides the legacy field, preferring the active entry; "session" and
//     "weekly_all" fill five_hour / seven_day when the named fields are absent.
//   - any other seven_day_<name> object is kept as a weekly window for <name>.
func ParseClaudeUsage(body []byte, now time.Time) ([]coreauth.QuotaWindow, error) {
	root, errRoot := parseUsageRoot(body)
	if errRoot != nil {
		return nil, errRoot
	}
	observedAt := now.UTC().Round(0)
	byKey := make(map[string]coreauth.QuotaWindow)
	named := make(map[string]struct{}, len(claudeNamedWindows))
	for _, spec := range claudeNamedWindows {
		named[spec.key] = struct{}{}
		node := root.Get(spec.key)
		if !node.IsObject() {
			continue
		}
		if spec.key == "iguana_necktie" && isDollarDenominatedClaudeWindow(node) {
			continue
		}
		key := spec.window
		if key == "" {
			key = coreauth.QuotaWindowKey(spec.kind, spec.family)
		}
		if window, ok := claudeWindowFromNode(node, key, spec.kind, spec.family, observedAt); ok {
			byKey[key] = window
		}
	}
	root.ForEach(func(keyNode, value ojson.Result) bool {
		rawKey := keyNode.String()
		if _, isNamed := named[rawKey]; isNamed || !strings.HasPrefix(rawKey, "seven_day_") || !value.IsObject() {
			return true
		}
		family := coreauth.NormalizeQuotaModelFamily(strings.TrimPrefix(rawKey, "seven_day_"))
		if family == "" || isDollarDenominatedClaudeWindow(value) {
			return true
		}
		key := coreauth.QuotaWindowKey(coreauth.QuotaWindowKindWeekly, family)
		if _, exists := byKey[key]; exists {
			return true
		}
		if window, ok := claudeWindowFromNode(value, key, coreauth.QuotaWindowKindWeekly, family, observedAt); ok {
			byKey[key] = window
		}
		return true
	})
	applyClaudeLimits(byKey, root.Get("limits"), observedAt)
	return sortedWindows(byKey), nil
}

func claudeWindowFromNode(node ojson.Result, key, kind, family string, observedAt time.Time) (coreauth.QuotaWindow, bool) {
	used, ok := numberValue(node.Get("utilization"))
	if !ok {
		return coreauth.QuotaWindow{}, false
	}
	window := coreauth.QuotaWindow{
		Key:         key,
		Kind:        kind,
		Model:       family,
		UsedPercent: used,
		ObservedAt:  observedAt,
		Source:      coreauth.QuotaWindowSourcePoll,
	}
	if resetAt, okReset := resetValue(node.Get("resets_at")); okReset {
		window.ResetsAt = resetAt
	}
	return window, true
}

func isDollarDenominatedClaudeWindow(node ojson.Result) bool {
	for _, field := range []string{"limit_dollars", "used_dollars", "remaining_dollars"} {
		if _, ok := numberValue(node.Get(field)); ok {
			return true
		}
	}
	return false
}

func applyClaudeLimits(byKey map[string]coreauth.QuotaWindow, limits ojson.Result, observedAt time.Time) {
	if !limits.IsArray() {
		return
	}
	type candidate struct {
		window coreauth.QuotaWindow
		active bool
	}
	scoped := make(map[string]candidate)
	var session, weeklyAll *candidate
	for _, limit := range limits.Array() {
		if !limit.IsObject() {
			continue
		}
		used, ok := numberValue(limit.Get("percent"))
		if !ok {
			continue
		}
		active := limit.Get("is_active").Type == ojson.True
		window := coreauth.QuotaWindow{UsedPercent: used, ObservedAt: observedAt, Source: coreauth.QuotaWindowSourcePoll}
		if resetAt, okReset := resetValue(limit.Get("resets_at")); okReset {
			window.ResetsAt = resetAt
		}
		switch strings.ToLower(strings.TrimSpace(limit.Get("kind").String())) {
		case "weekly_scoped":
			family := ""
			if fields := strings.Fields(limit.Get("scope.model.display_name").String()); len(fields) > 0 {
				family = coreauth.NormalizeQuotaModelFamily(fields[0])
			}
			if family == "" {
				family = coreauth.NormalizeQuotaModelFamily(limit.Get("scope.model.id").String())
			}
			if family == "" {
				continue
			}
			window.Kind = coreauth.QuotaWindowKindWeekly
			window.Model = family
			window.Key = coreauth.QuotaWindowKey(window.Kind, family)
			if existing, exists := scoped[window.Key]; exists && (existing.active || !active) {
				continue
			}
			scoped[window.Key] = candidate{window: window, active: active}
		case "session":
			window.Kind = coreauth.QuotaWindowKindFiveHour
			window.Key = coreauth.QuotaWindowKeyFiveHour
			if session == nil || (!session.active && active) {
				session = &candidate{window: window, active: active}
			}
		case "weekly_all":
			window.Kind = coreauth.QuotaWindowKindWeekly
			window.Key = coreauth.QuotaWindowKeyWeekly
			if weeklyAll == nil || (!weeklyAll.active && active) {
				weeklyAll = &candidate{window: window, active: active}
			}
		}
	}
	for key, entry := range scoped {
		byKey[key] = entry.window
	}
	for _, fallback := range []*candidate{session, weeklyAll} {
		if fallback == nil {
			continue
		}
		if _, exists := byKey[fallback.window.Key]; !exists {
			byKey[fallback.window.Key] = fallback.window
		}
	}
}

// ParseCodexUsage converts a GET /backend-api/wham/usage payload into quota windows.
//
// Field mapping (see management-center src/features/quota/providers/codex/data.ts):
//   - rate_limit.{primary_window,secondary_window}: {used_percent,
//     limit_window_seconds, reset_at (Unix seconds), reset_after_seconds}
//     classified by limit_window_seconds (< 1 day -> five_hour, otherwise
//     seven_day, which also covers the monthly team window); without a length
//     the primary slot is the 5h window and the secondary slot the weekly one.
//   - additional_rate_limits[]: {limit_name | metered_feature, rate_limit{...}}
//     produce model-scoped windows keyed seven_day_<limit> / five_hour_<limit>.
//   - a missing used_percent with limit_reached=true or allowed=false and a
//     known reset counts as 100% used, like the panel.
//
// The code_review_rate_limit does not gate model traffic and is ignored.
func ParseCodexUsage(body []byte, now time.Time) ([]coreauth.QuotaWindow, error) {
	root, errRoot := parseUsageRoot(body)
	if errRoot != nil {
		return nil, errRoot
	}
	observedAt := now.UTC().Round(0)
	byKey := make(map[string]coreauth.QuotaWindow)
	addCodexRateLimitWindows(byKey, firstResult(root, "rate_limit", "rateLimit"), "", observedAt)
	additional := firstResult(root, "additional_rate_limits", "additionalRateLimits")
	if additional.IsArray() {
		for index, item := range additional.Array() {
			rateInfo := firstResult(item, "rate_limit", "rateLimit")
			if !rateInfo.IsObject() {
				continue
			}
			name := stringValue(firstResult(item, "limit_name", "limitName"))
			if name == "" {
				name = stringValue(firstResult(item, "metered_feature", "meteredFeature"))
			}
			if name == "" {
				name = fmt.Sprintf("additional-%d", index+1)
			}
			family := coreauth.NormalizeQuotaModelFamily(name)
			if family == "" {
				continue
			}
			addCodexRateLimitWindows(byKey, rateInfo, family, observedAt)
		}
	}
	return sortedWindows(byKey), nil
}

func addCodexRateLimitWindows(byKey map[string]coreauth.QuotaWindow, info ojson.Result, family string, observedAt time.Time) {
	if !info.IsObject() {
		return
	}
	limitReached := firstResult(info, "limit_reached", "limitReached").Type == ojson.True
	notAllowed := firstResult(info, "allowed").Type == ojson.False
	slots := []struct {
		node      ojson.Result
		secondary bool
	}{
		{node: firstResult(info, "primary_window", "primaryWindow")},
		{node: firstResult(info, "secondary_window", "secondaryWindow"), secondary: true},
	}
	for _, slot := range slots {
		if !slot.node.IsObject() {
			continue
		}
		seconds, _ := numberValue(firstResult(slot.node, "limit_window_seconds", "limitWindowSeconds"))
		kind := coreauth.QuotaWindowKindForSeconds(int64(seconds), slot.secondary)
		resetAt, hasReset := resetValue(firstResult(slot.node, "reset_at", "resetAt"))
		if !hasReset {
			if after, ok := numberValue(firstResult(slot.node, "reset_after_seconds", "resetAfterSeconds")); ok && after > 0 {
				resetAt = observedAt.Add(time.Duration(after * float64(time.Second))).Round(time.Second)
				hasReset = true
			}
		}
		used, ok := numberValue(firstResult(slot.node, "used_percent", "usedPercent"))
		if !ok {
			if !(limitReached || notAllowed) || !hasReset {
				continue
			}
			used = 100
		}
		key := coreauth.QuotaWindowKey(kind, family)
		if _, exists := byKey[key]; exists {
			continue
		}
		window := coreauth.QuotaWindow{
			Key:         key,
			Kind:        kind,
			Model:       family,
			UsedPercent: used,
			ObservedAt:  observedAt,
			Source:      coreauth.QuotaWindowSourcePoll,
		}
		if hasReset {
			window.ResetsAt = resetAt
		}
		byKey[key] = window
	}
}

func parseUsageRoot(body []byte) (ojson.Result, error) {
	if len(body) == 0 || !ojson.ValidBytes(body) {
		return ojson.Result{}, errInvalidUsagePayload
	}
	root := ojson.ParseBytes(body)
	if !root.IsObject() {
		return ojson.Result{}, errInvalidUsagePayload
	}
	return root, nil
}

func sortedWindows(byKey map[string]coreauth.QuotaWindow) []coreauth.QuotaWindow {
	if len(byKey) == 0 {
		return nil
	}
	windows := make([]coreauth.QuotaWindow, 0, len(byKey))
	for _, window := range byKey {
		windows = append(windows, window)
	}
	sort.Slice(windows, func(i, j int) bool { return windows[i].Key < windows[j].Key })
	return windows
}

func firstResult(object ojson.Result, keys ...string) ojson.Result {
	for _, key := range keys {
		value := object.Get(key)
		if value.Exists() && value.Type != ojson.Null {
			return value
		}
	}
	return ojson.Result{}
}

// numberValue mirrors the panel's normalizeNumberValue: finite numbers and
// numeric strings are accepted; null, empty, and other types are not.
func numberValue(value ojson.Result) (float64, bool) {
	var parsed float64
	switch value.Type {
	case ojson.Number:
		parsed = value.Float()
	case ojson.String:
		trimmed := strings.TrimSpace(value.String())
		if trimmed == "" {
			return 0, false
		}
		number, errParse := strconv.ParseFloat(trimmed, 64)
		if errParse != nil {
			return 0, false
		}
		parsed = number
	default:
		return 0, false
	}
	if math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, false
	}
	return parsed, true
}

func stringValue(value ojson.Result) string {
	if value.Type != ojson.String {
		return ""
	}
	return strings.TrimSpace(value.String())
}

// resetValue mirrors the panel's resolveResetMs: ISO timestamps first, then
// Unix seconds or milliseconds.
func resetValue(value ojson.Result) (time.Time, bool) {
	switch value.Type {
	case ojson.String:
		return coreauth.ParseQuotaResetTime(value.String())
	case ojson.Number:
		return coreauth.QuotaResetTimeFromUnix(value.Float())
	default:
		return time.Time{}, false
	}
}
