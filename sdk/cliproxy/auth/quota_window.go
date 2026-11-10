package auth

import (
	"context"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// Quota window kinds, keys, and sources. Keys follow the Claude usage payload
// naming ("five_hour", "seven_day", "seven_day_<family>") for every provider
// so routing can treat Claude and Codex windows uniformly.
const (
	QuotaWindowKindFiveHour = "5h"
	QuotaWindowKindWeekly   = "7d"

	QuotaWindowKeyFiveHour = "five_hour"
	QuotaWindowKeyWeekly   = "seven_day"

	QuotaWindowSourcePoll    = "poll"
	QuotaWindowSourceHeaders = "headers"
	QuotaWindowSourceWS      = "ws"

	maxQuotaWindows = 32
	// quotaWindowRetention drops windows whose reset passed long ago so a
	// vanished model bucket cannot linger forever.
	quotaWindowRetention = 8 * 24 * time.Hour
)

// QuotaWindow is one structured provider usage window (rolling 5h or weekly),
// observed by the background usage poller, passive response headers, or Codex
// websocket rate-limit events. Windows are merged per Key: the newest
// observation wins, so a poll never clobbers a fresher header observation.
type QuotaWindow struct {
	// Key identifies the window, e.g. "five_hour", "seven_day", "seven_day_fable".
	Key string `json:"key"`
	// Kind is "5h" for short rolling windows and "7d" for weekly (or longer) windows.
	Kind string `json:"kind"`
	// Model is the optional normalized model family the window is scoped to.
	Model string `json:"model,omitempty"`
	// UsedPercent is the utilization in percent (0-100, may exceed 100 when over limit).
	UsedPercent float64 `json:"used_percent"`
	// ResetsAt is when the window resets; zero when unknown.
	ResetsAt time.Time `json:"resets_at"`
	// ObservedAt is when the observation was made.
	ObservedAt time.Time `json:"observed_at"`
	// Source is "poll", "headers", or "ws".
	Source string `json:"source"`
}

// RemainingPercent reports the remaining allowance in percent, floored at zero.
func (w QuotaWindow) RemainingPercent() float64 {
	remaining := 100 - w.UsedPercent
	if remaining < 0 {
		return 0
	}
	return remaining
}

// Fresh reports whether the window carries a reset that is still in the future.
// A window whose reset already passed describes a previous period and is stale.
func (w QuotaWindow) Fresh(now time.Time) bool {
	return !w.ResetsAt.IsZero() && w.ResetsAt.After(now)
}

func (w QuotaWindow) equal(other QuotaWindow) bool {
	return w.Key == other.Key && w.Kind == other.Kind && w.Model == other.Model &&
		w.UsedPercent == other.UsedPercent && w.ResetsAt.Equal(other.ResetsAt) &&
		w.ObservedAt.Equal(other.ObservedAt) && w.Source == other.Source
}

func cloneQuotaWindows(windows []QuotaWindow) []QuotaWindow {
	if len(windows) == 0 {
		return nil
	}
	return append([]QuotaWindow(nil), windows...)
}

// NormalizeQuotaModelFamily lowercases a model or limit name and collapses every
// character outside [a-z0-9.] into a single dash ("GPT-5.3-Codex-Spark" ->
// "gpt-5.3-codex-spark").
func NormalizeQuotaModelFamily(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "" {
		return ""
	}
	var builder strings.Builder
	builder.Grow(len(raw))
	lastDash := false
	for _, char := range raw {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '.' {
			builder.WriteRune(char)
			lastDash = false
			continue
		}
		if !lastDash {
			builder.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(builder.String(), "-.")
}

// QuotaWindowKey builds the canonical window key for a kind and optional model family.
func QuotaWindowKey(kind, family string) string {
	base := QuotaWindowKeyWeekly
	if kind == QuotaWindowKindFiveHour {
		base = QuotaWindowKeyFiveHour
	}
	if family = NormalizeQuotaModelFamily(family); family != "" {
		return base + "_" + family
	}
	return base
}

// QuotaWindowKindForSeconds classifies a window by its length. Windows shorter
// than a day are rolling short windows; anything longer (weekly, or the monthly
// team window Codex reports in the same slot) is treated as the weekly window.
// When the length is unknown the slot order decides, matching the management
// panel's legacy primary/secondary fallback.
func QuotaWindowKindForSeconds(seconds int64, secondarySlot bool) string {
	if seconds > 0 {
		if seconds < int64((24 * time.Hour).Seconds()) {
			return QuotaWindowKindFiveHour
		}
		return QuotaWindowKindWeekly
	}
	if secondarySlot {
		return QuotaWindowKindWeekly
	}
	return QuotaWindowKindFiveHour
}

// QuotaWindowMatchesModel reports whether a window's model family applies to the
// requested model (e.g. family "fable" applies to "claude-fable-5").
func QuotaWindowMatchesModel(family, model string) bool {
	family = NormalizeQuotaModelFamily(family)
	model = NormalizeQuotaModelFamily(model)
	return family != "" && model != "" && strings.Contains(model, family)
}

// MergeQuotaWindows merges incoming observations into existing windows per key.
// The newer observation wins; an incoming window without a reset keeps the
// previous reset while that reset is still ahead of the new observation.
// The input slices are never modified.
func MergeQuotaWindows(existing, incoming []QuotaWindow) ([]QuotaWindow, bool) {
	if len(incoming) == 0 {
		return existing, false
	}
	byKey := make(map[string]QuotaWindow, len(existing)+len(incoming))
	for _, window := range existing {
		if window.Key != "" {
			byKey[window.Key] = window
		}
	}
	changed := false
	for _, window := range incoming {
		window.Key = strings.TrimSpace(window.Key)
		if window.Key == "" {
			continue
		}
		current, ok := byKey[window.Key]
		if ok && window.ObservedAt.Before(current.ObservedAt) {
			continue
		}
		if ok && window.ResetsAt.IsZero() && current.ResetsAt.After(window.ObservedAt) {
			window.ResetsAt = current.ResetsAt
		}
		if ok && current.equal(window) {
			continue
		}
		byKey[window.Key] = window
		changed = true
	}
	if !changed {
		return existing, false
	}
	merged := make([]QuotaWindow, 0, len(byKey))
	newest := time.Time{}
	for _, window := range byKey {
		merged = append(merged, window)
		if window.ObservedAt.After(newest) {
			newest = window.ObservedAt
		}
	}
	// Drop windows whose reset passed long before the newest observation.
	kept := merged[:0]
	for _, window := range merged {
		if !window.ResetsAt.IsZero() && !newest.IsZero() && window.ResetsAt.Add(quotaWindowRetention).Before(newest) {
			continue
		}
		kept = append(kept, window)
	}
	merged = kept
	if len(merged) > maxQuotaWindows {
		sort.Slice(merged, func(i, j int) bool { return merged[i].ObservedAt.After(merged[j].ObservedAt) })
		merged = merged[:maxQuotaWindows]
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].Key < merged[j].Key })
	return merged, true
}

// ObserveQuotaWindows merges structured quota windows into the in-memory auth.
// It never persists the auth, never touches cooldown state, and never notifies
// auth-change watchers: windows are passive routing hints only.
func (m *Manager) ObserveQuotaWindows(authID string, windows []QuotaWindow) bool {
	if m == nil || strings.TrimSpace(authID) == "" || len(windows) == 0 {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	auth := m.auths[authID]
	if auth == nil {
		return false
	}
	merged, changed := MergeQuotaWindows(auth.Quota.Windows, windows)
	if changed {
		auth.Quota.Windows = merged
	}
	return changed
}

// carryQuotaWindows keeps runtime windows across auth replacement (file reloads,
// token refreshes), where the incoming auth is rebuilt without runtime state.
func carryQuotaWindows(existing, updated *Auth) {
	if existing == nil || updated == nil || len(existing.Quota.Windows) == 0 {
		return
	}
	if merged, changed := MergeQuotaWindows(existing.Quota.Windows, updated.Quota.Windows); changed {
		updated.Quota.Windows = merged
	} else {
		updated.Quota.Windows = cloneQuotaWindows(existing.Quota.Windows)
	}
}

// observeQuotaWindowHeaders derives structured windows from the response headers
// MarkResult already observes (Claude unified rate-limit headers, Codex x-codex-*
// headers, and Codex websocket codex.rate_limits events normalized into headers).
func observeQuotaWindowHeaders(ctx context.Context, auth *Auth, provider string, headers http.Header, now time.Time) {
	if auth == nil || len(headers) == 0 {
		return
	}
	source := QuotaWindowSourceHeaders
	if strings.EqualFold(strings.TrimSpace(provider), "codex") && cliproxyexecutor.DownstreamWebsocket(ctx) && authWebsocketsEnabled(auth) {
		source = QuotaWindowSourceWS
	}
	windows := QuotaWindowsFromHeaders(provider, headers, now, source)
	if len(windows) == 0 {
		return
	}
	if merged, changed := MergeQuotaWindows(auth.Quota.Windows, windows); changed {
		auth.Quota.Windows = merged
	}
}

// QuotaWindowsFromHeaders converts provider rate-limit headers into windows.
func QuotaWindowsFromHeaders(provider string, headers http.Header, observedAt time.Time, source string) []QuotaWindow {
	if len(headers) == 0 {
		return nil
	}
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	observedAt = observedAt.UTC().Round(0)
	lower := make(map[string]string, len(headers))
	for key, values := range headers {
		if len(values) == 0 {
			continue
		}
		lower[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(values[len(values)-1])
	}
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude":
		return claudeQuotaWindowsFromHeaders(lower, observedAt, source)
	case "codex":
		return codexQuotaWindowsFromHeaders(lower, observedAt, source)
	default:
		return nil
	}
}

// claudeQuotaWindowsFromHeaders reads anthropic-ratelimit-unified-{5h,7d,7d_oi}-*.
// Header utilization is a 0-1 fraction (0.69 = 69%), unlike the usage API which
// reports percent. 7d_oi is the Fable-scoped weekly window.
func claudeQuotaWindowsFromHeaders(headers map[string]string, observedAt time.Time, source string) []QuotaWindow {
	specs := []struct {
		prefix string
		kind   string
		family string
	}{
		{prefix: "anthropic-ratelimit-unified-5h-", kind: QuotaWindowKindFiveHour},
		{prefix: "anthropic-ratelimit-unified-7d-", kind: QuotaWindowKindWeekly},
		{prefix: "anthropic-ratelimit-unified-7d_oi-", kind: QuotaWindowKindWeekly, family: "fable"},
	}
	var windows []QuotaWindow
	for _, spec := range specs {
		rawUtilization := headers[spec.prefix+"utilization"]
		if rawUtilization == "" {
			continue
		}
		utilization, errParse := strconv.ParseFloat(rawUtilization, 64)
		if errParse != nil || math.IsNaN(utilization) || math.IsInf(utilization, 0) || utilization < 0 {
			continue
		}
		window := QuotaWindow{
			Key:         QuotaWindowKey(spec.kind, spec.family),
			Kind:        spec.kind,
			Model:       spec.family,
			UsedPercent: roundQuotaPercent(utilization * 100),
			ObservedAt:  observedAt,
			Source:      source,
		}
		if resetAt, ok := ParseQuotaResetTime(headers[spec.prefix+"reset"]); ok {
			window.ResetsAt = resetAt
		}
		windows = append(windows, window)
	}
	return windows
}

// codexQuotaWindowsFromHeaders reads x-codex-[<namespace>-]{primary,secondary}-*.
// The namespace is empty for the main limit, a short name on the HTTP path
// (x-codex-bengalfox-*), or additional-<limit> for websocket events. The
// code-review limit does not gate model traffic and is skipped.
func codexQuotaWindowsFromHeaders(headers map[string]string, observedAt time.Time, source string) []QuotaWindow {
	const prefix = "x-codex-"
	byKey := make(map[string]QuotaWindow)
	for key := range headers {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		rest := strings.TrimPrefix(key, prefix)
		for _, slot := range []string{"primary", "secondary"} {
			suffix := slot + "-used-percent"
			if !strings.HasSuffix(rest, suffix) {
				continue
			}
			namespace := strings.TrimSuffix(strings.TrimSuffix(rest, suffix), "-")
			if namespace == "code-review" {
				continue
			}
			base := prefix + slot + "-"
			family := ""
			if namespace != "" {
				base = prefix + namespace + "-" + slot + "-"
				family = NormalizeQuotaModelFamily(headers[prefix+namespace+"-limit-name"])
				if family == "" {
					family = NormalizeQuotaModelFamily(strings.TrimPrefix(namespace, "additional-"))
				}
				if family == "" {
					continue
				}
			}
			used, errUsed := strconv.ParseFloat(headers[key], 64)
			if errUsed != nil || math.IsNaN(used) || math.IsInf(used, 0) || used < 0 {
				continue
			}
			var windowSeconds int64
			if minutes, errMinutes := strconv.ParseInt(headers[base+"window-minutes"], 10, 64); errMinutes == nil && minutes > 0 {
				windowSeconds = minutes * 60
			}
			kind := QuotaWindowKindForSeconds(windowSeconds, slot == "secondary")
			window := QuotaWindow{
				Key:         QuotaWindowKey(kind, family),
				Kind:        kind,
				Model:       family,
				UsedPercent: roundQuotaPercent(used),
				ObservedAt:  observedAt,
				Source:      source,
			}
			if resetAt, ok := ParseQuotaResetTime(headers[base+"reset-at"]); ok {
				window.ResetsAt = resetAt
			} else if seconds, errSeconds := strconv.ParseFloat(headers[base+"reset-after-seconds"], 64); errSeconds == nil && seconds >= 0 && !math.IsInf(seconds, 0) {
				window.ResetsAt = observedAt.Add(time.Duration(seconds * float64(time.Second))).Round(time.Second)
			}
			byKey[window.Key] = window
		}
	}
	if len(byKey) == 0 {
		return nil
	}
	windows := make([]QuotaWindow, 0, len(byKey))
	for _, window := range byKey {
		windows = append(windows, window)
	}
	sort.Slice(windows, func(i, j int) bool { return windows[i].Key < windows[j].Key })
	return windows
}

// ParseQuotaResetTime parses a reset instant given as Unix seconds, Unix
// milliseconds, or an RFC 3339 timestamp (with up to nanosecond precision).
func ParseQuotaResetTime(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	if numeric, errParse := strconv.ParseFloat(raw, 64); errParse == nil {
		return QuotaResetTimeFromUnix(numeric)
	}
	if parsed, errParse := time.Parse(time.RFC3339Nano, raw); errParse == nil {
		return parsed.UTC(), true
	}
	return time.Time{}, false
}

// QuotaResetTimeFromUnix converts Unix seconds or milliseconds (disambiguated by
// magnitude, like the management panel) into a UTC instant.
func QuotaResetTimeFromUnix(value float64) (time.Time, bool) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
		return time.Time{}, false
	}
	if value >= 1e11 {
		value /= 1000
	}
	seconds := math.Floor(value)
	return time.Unix(int64(seconds), int64((value-seconds)*1e9)).UTC().Round(time.Millisecond), true
}

func roundQuotaPercent(value float64) float64 {
	return math.Round(value*100) / 100
}
