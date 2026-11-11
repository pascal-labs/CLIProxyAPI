package management

import (
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/hubevents"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotahistory"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/sessionmeta"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestats"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// Data sources for the hub dashboard endpoints; tests replace them.
var (
	quotaHistorySource = quotahistory.Default
	hubEventsSource    = hubevents.Default
	sessionMetaSource  = sessionmeta.Default
	hubNow             = time.Now
)

const (
	hubMonth              = 30 * 24 * time.Hour
	runwayHistoryWindow   = 7 * 24 * time.Hour
	runwayHistoryPoints   = 200
	runwayBurnLookback    = 6 * time.Hour
	runwayBurnFallback    = 24 * time.Hour
	runwayCyclesPerWindow = 10
	sessionResolveBudget  = 3 * time.Second

	// A burn rate is only projected once the current cycle has at least
	// runwayMinCycleSamples samples spanning the window kind's minimum, so a
	// fresh start does not extrapolate a few minutes of traffic.
	runwayMinCycleSamples = 3
	runwayMinSpanFiveHour = time.Hour
	runwayMinSpanWeekly   = 3 * time.Hour

	// projection_status values on /usage/runway windows.
	runwayProjectionOK       = "ok"
	runwayProjectionLearning = "learning"
	runwayProjectionIdle     = "idle"
)

// whatIfModels are the models every session's Claude tokens are re-priced on
// for /usage/sessions what_if_usd.
var whatIfModels = []string{"claude-opus-5-5", "claude-sonnet-5-5"}

var hubWindowDurations = map[string]time.Duration{
	"1h":  time.Hour,
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
	"30d": hubMonth,
}

// parseHubWindow reads ?window= (default def) and checks it against allowed.
func parseHubWindow(c *gin.Context, def string, allowed ...string) (string, time.Duration, bool) {
	label := strings.ToLower(strings.TrimSpace(c.DefaultQuery("window", def)))
	for _, candidate := range allowed {
		if candidate == label {
			return label, hubWindowDurations[label], true
		}
	}
	c.JSON(http.StatusBadRequest, gin.H{"error": "invalid window; use " + strings.Join(allowed, ", ")})
	return "", 0, false
}

func parseHubLimit(c *gin.Context, def, max int) int {
	raw := strings.TrimSpace(c.Query("limit"))
	if raw == "" {
		return def
	}
	limit, errParse := strconv.Atoi(raw)
	if errParse != nil || limit <= 0 {
		return def
	}
	if limit > max {
		return max
	}
	return limit
}

func (h *Handler) hubConfig() config.HubConfig {
	if h == nil || h.cfg == nil {
		return config.HubConfig{}
	}
	return h.cfg.Hub
}

// hubAuths indexes the manager's credentials by auth_index and by auth ID.
func (h *Handler) hubAuths() (byIndex map[string]*coreauth.Auth, byID map[string]*coreauth.Auth) {
	byIndex = make(map[string]*coreauth.Auth)
	byID = make(map[string]*coreauth.Auth)
	if h == nil || h.authManager == nil {
		return byIndex, byID
	}
	for _, auth := range h.authManager.List() {
		if auth == nil {
			continue
		}
		if index := auth.EnsureIndex(); index != "" {
			byIndex[index] = auth
		}
		byID[auth.ID] = auth
	}
	return byIndex, byID
}

// apiValueUSD prices tokens at API rates. Cache writes whose TTL split is
// unknown are priced at the 5-minute rate.
func apiValueUSD(price config.HubModelPrice, tokens usagestats.Tokens) float64 {
	unsplit := tokens.CacheCreationTokens - tokens.CacheCreation5mTokens - tokens.CacheCreation1hTokens
	if unsplit < 0 {
		unsplit = 0
	}
	usd := float64(tokens.UncachedInputTokens)*price.Input +
		float64(tokens.OutputTokens)*price.Output +
		float64(tokens.CacheReadTokens)*price.CacheRead +
		float64(tokens.CacheCreation5mTokens+unsplit)*price.CacheWrite5m +
		float64(tokens.CacheCreation1hTokens)*price.CacheWrite1h
	return usd / 1e6
}

// cacheSavingsUSD is what the cache reads would have cost as fresh input
// minus what they cost as cache reads.
func cacheSavingsUSD(price config.HubModelPrice, tokens usagestats.Tokens) float64 {
	return float64(tokens.CacheReadTokens) * (price.Input - price.CacheRead) / 1e6
}

// noCacheValueUSD prices the same traffic as if nothing were cached: every
// prompt token (uncached input, cache reads and cache writes) at the input
// rate, plus output.
func noCacheValueUSD(price config.HubModelPrice, tokens usagestats.Tokens) float64 {
	prompt := tokens.UncachedInputTokens + tokens.CacheReadTokens + tokens.CacheCreationTokens
	return (float64(prompt)*price.Input + float64(tokens.OutputTokens)*price.Output) / 1e6
}

// isClaudeModel reports whether a model id names a Claude model (a provider
// path prefix such as "anthropic/" is ignored).
func isClaudeModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	if index := strings.LastIndex(model, "/"); index >= 0 {
		model = model[index+1:]
	}
	return strings.HasPrefix(model, "claude")
}

func roundUSD(value float64) float64 { return math.Round(value*10000) / 10000 }

func roundRatio(value float64) float64 { return math.Round(value*10000) / 10000 }

func float64PtrRounded(value float64, round func(float64) float64) *float64 {
	rounded := round(value)
	return &rounded
}

func stringPtrOrNil(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}

func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// GET /usage/models

type usageModelRow struct {
	AuthIndex             string   `json:"auth_index"`
	Provider              string   `json:"provider"`
	Label                 string   `json:"label"`
	Model                 string   `json:"model"`
	Requests              int64    `json:"requests"`
	InputTokens           int64    `json:"input_tokens"`
	UncachedInputTokens   int64    `json:"uncached_input_tokens"`
	OutputTokens          int64    `json:"output_tokens"`
	CacheReadTokens       int64    `json:"cache_read_tokens"`
	CacheCreationTokens   int64    `json:"cache_creation_tokens"`
	CacheCreation5mTokens int64    `json:"cache_creation_5m_tokens"`
	CacheCreation1hTokens int64    `json:"cache_creation_1h_tokens"`
	CacheHitRatio         float64  `json:"cache_hit_ratio"`
	APIValueUSD           *float64 `json:"api_value_usd"`
	CacheSavingsUSD       *float64 `json:"cache_savings_usd"`
	PriceKey              *string  `json:"price_key"`
}

type usageModelsResponse struct {
	Window         string          `json:"window"`
	GeneratedAt    string          `json:"generated_at"`
	Rows           []usageModelRow `json:"rows"`
	UnpricedModels []string        `json:"unpriced_models"`
}

// GetUsageModels reports usage per credential and model with its API-equivalent
// value and cache savings (cache reads priced as fresh input minus their cache
// price); both are null for unpriced models.
//
// Endpoint: GET /v8/management/usage/models?window=1h|24h|7d|30d (also under /v0/management).
func (h *Handler) GetUsageModels(c *gin.Context) {
	windowLabel, window, ok := parseHubWindow(c, "24h", "1h", "24h", "7d", "30d")
	if !ok {
		return
	}
	now := hubNow().UTC()
	var usage []usagestats.ModelUsage
	if stats := usageStatsSource(); stats != nil {
		usage = stats.QueryModels(window)
	}
	byIndex, _ := h.hubAuths()
	prices := h.hubConfig().PriceTable()
	unpriced := make(map[string]struct{})
	rows := make([]usageModelRow, 0, len(usage))
	for _, entry := range usage {
		row := usageModelRow{
			AuthIndex:             entry.AuthIndex,
			Provider:              entry.Provider,
			Label:                 usageAccountLabel(byIndex[entry.AuthIndex], entry.AuthID),
			Model:                 entry.Model,
			Requests:              entry.Totals.Requests,
			InputTokens:           entry.Totals.InputTokens,
			UncachedInputTokens:   entry.Totals.UncachedInputTokens,
			OutputTokens:          entry.Totals.OutputTokens,
			CacheReadTokens:       entry.Totals.CacheReadTokens,
			CacheCreationTokens:   entry.Totals.CacheCreationTokens,
			CacheCreation5mTokens: entry.Totals.CacheCreation5mTokens,
			CacheCreation1hTokens: entry.Totals.CacheCreation1hTokens,
			CacheHitRatio:         entry.Totals.CacheHitRatio(),
		}
		if auth := byIndex[entry.AuthIndex]; auth != nil {
			row.Provider = strings.ToLower(strings.TrimSpace(auth.Provider))
		}
		if price, key, priced := prices.Lookup(entry.Model); priced {
			row.APIValueUSD = float64PtrRounded(apiValueUSD(price, entry.Totals), roundUSD)
			row.CacheSavingsUSD = float64PtrRounded(cacheSavingsUSD(price, entry.Totals), roundUSD)
			row.PriceKey = stringPtrOrNil(key)
		} else {
			unpriced[entry.Model] = struct{}{}
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Provider != rows[j].Provider {
			return rows[i].Provider < rows[j].Provider
		}
		if rows[i].Label != rows[j].Label {
			return rows[i].Label < rows[j].Label
		}
		if rows[i].AuthIndex != rows[j].AuthIndex {
			return rows[i].AuthIndex < rows[j].AuthIndex
		}
		return rows[i].Model < rows[j].Model
	})
	c.JSON(http.StatusOK, usageModelsResponse{Window: windowLabel, GeneratedAt: now.Format(time.RFC3339), Rows: rows, UnpricedModels: sortedKeys(unpriced)})
}

// ---------------------------------------------------------------------------
// GET /usage/sessions

type usageSessionRow struct {
	SessionKey          string   `json:"session_key"`
	NativeSessionID     *string  `json:"native_session_id"`
	Provider            string   `json:"provider"`
	Client              string   `json:"client"`
	Project             *string  `json:"project"`
	Cwd                 *string  `json:"cwd"`
	Title               *string  `json:"title"`
	T3ThreadID          *string  `json:"t3_thread_id"`
	T3Title             *string  `json:"t3_title"`
	T3ParentTitle       *string  `json:"t3_parent_title"`
	Subagent            *bool    `json:"subagent"`
	AuthIndexes         []string `json:"auth_indexes"`
	FirstSeen           string   `json:"first_seen"`
	LastSeen            string   `json:"last_seen"`
	Requests            int64    `json:"requests"`
	InputTokens         int64    `json:"input_tokens"`
	OutputTokens        int64    `json:"output_tokens"`
	CacheReadTokens     int64    `json:"cache_read_tokens"`
	CacheCreationTokens int64    `json:"cache_creation_tokens"`
	CacheHitRatio       float64  `json:"cache_hit_ratio"`
	ColdCacheWrites     int64    `json:"cold_cache_writes"`
	APIValueUSD         *float64 `json:"api_value_usd"`
	// Models splits the session's usage by model.
	Models []usageSessionModel `json:"models"`
	// WhatIfUSD re-prices the session's Claude tokens on each of whatIfModels
	// at the configured prices; null when the session has no Claude usage.
	WhatIfUSD map[string]float64 `json:"what_if_usd"`
}

type usageSessionModel struct {
	Model               string   `json:"model"`
	Requests            int64    `json:"requests"`
	InputTokens         int64    `json:"input_tokens"`
	OutputTokens        int64    `json:"output_tokens"`
	CacheReadTokens     int64    `json:"cache_read_tokens"`
	CacheCreationTokens int64    `json:"cache_creation_tokens"`
	APIValueUSD         *float64 `json:"api_value_usd"`
}

// sessionModelSplit returns the session's per-model rows (highest value
// first, unpriced last, then most tokens), its total API value (priced is
// false when no model had a price) and the what-if prices of its Claude tokens.
func sessionModelSplit(models map[string]usagestats.Tokens, prices config.HubPriceTable) (rows []usageSessionModel, value float64, priced bool, whatIf map[string]float64) {
	rows = make([]usageSessionModel, 0, len(models))
	totals := make(map[string]float64, len(whatIfModels))
	claude := false
	for model, tokens := range models {
		row := usageSessionModel{
			Model:               model,
			Requests:            tokens.Requests,
			InputTokens:         tokens.InputTokens,
			OutputTokens:        tokens.OutputTokens,
			CacheReadTokens:     tokens.CacheReadTokens,
			CacheCreationTokens: tokens.CacheCreationTokens,
		}
		if price, _, ok := prices.Lookup(model); ok {
			modelValue := apiValueUSD(price, tokens)
			row.APIValueUSD = float64PtrRounded(modelValue, roundUSD)
			value += modelValue
			priced = true
		}
		if isClaudeModel(model) {
			claude = true
			for _, target := range whatIfModels {
				if price, _, ok := prices.Lookup(target); ok {
					totals[target] += apiValueUSD(price, tokens)
				}
			}
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		left, right := rows[i], rows[j]
		if (left.APIValueUSD == nil) != (right.APIValueUSD == nil) {
			return left.APIValueUSD != nil
		}
		if left.APIValueUSD != nil && *left.APIValueUSD != *right.APIValueUSD {
			return *left.APIValueUSD > *right.APIValueUSD
		}
		leftTokens := left.InputTokens + left.OutputTokens + left.CacheReadTokens + left.CacheCreationTokens
		rightTokens := right.InputTokens + right.OutputTokens + right.CacheReadTokens + right.CacheCreationTokens
		if leftTokens != rightTokens {
			return leftTokens > rightTokens
		}
		return left.Model < right.Model
	})
	if claude && len(totals) > 0 {
		whatIf = make(map[string]float64, len(totals))
		for target, usd := range totals {
			whatIf[target] = roundUSD(usd)
		}
	}
	return rows, value, priced, whatIf
}

type usageSessionsResponse struct {
	Window      string            `json:"window"`
	GeneratedAt string            `json:"generated_at"`
	Sessions    []usageSessionRow `json:"sessions"`
}

// GetUsageSessions reports usage per agent session (the session-affinity
// identity, e.g. "claude:<uuid>" or "codex:<uuid>"), most recent first, with
// the session's working directory, title and T3 Code thread resolved from
// local files. Token totals cover the window; first_seen/last_seen span the
// retained history. models splits the window's usage by model, and
// what_if_usd re-prices the session's Claude tokens on Opus 5.5 and Sonnet
// 5.5 (API-price estimates; null without Claude usage).
//
// Endpoint: GET /v8/management/usage/sessions?window=24h|7d|30d&limit=N (also under /v0/management).
func (h *Handler) GetUsageSessions(c *gin.Context) {
	windowLabel, window, ok := parseHubWindow(c, "24h", "24h", "7d", "30d")
	if !ok {
		return
	}
	limit := parseHubLimit(c, 100, 1000)
	now := hubNow().UTC()
	var sessions []usagestats.SessionUsage
	if stats := usageStatsSource(); stats != nil {
		sessions = stats.QuerySessions(window, limit)
	}
	resolver := sessionMetaSource()
	if resolver != nil {
		authDir := ""
		if h != nil && h.cfg != nil && h.cfg.AuthDir != "" {
			if resolved, errResolve := util.ResolveAuthDir(h.cfg.AuthDir); errResolve == nil {
				authDir = resolved
			}
		}
		resolver.Configure(h.hubConfig().SessionRoots, authDir)
	}
	deadline := time.Now().Add(sessionResolveBudget)
	prices := h.hubConfig().PriceTable()
	rows := make([]usageSessionRow, 0, len(sessions))
	for _, session := range sessions {
		row := usageSessionRow{
			SessionKey:          session.Key,
			Provider:            session.Provider,
			AuthIndexes:         session.AuthIndexes,
			FirstSeen:           session.FirstSeen.UTC().Format(time.RFC3339),
			LastSeen:            session.LastSeen.UTC().Format(time.RFC3339),
			Requests:            session.Totals.Requests,
			InputTokens:         session.Totals.InputTokens,
			OutputTokens:        session.Totals.OutputTokens,
			CacheReadTokens:     session.Totals.CacheReadTokens,
			CacheCreationTokens: session.Totals.CacheCreationTokens,
			CacheHitRatio:       session.Totals.CacheHitRatio(),
			ColdCacheWrites:     session.ColdCacheWrites,
		}
		if row.AuthIndexes == nil {
			row.AuthIndexes = []string{}
		}
		var info sessionmeta.Info
		if resolver != nil {
			info = resolver.Resolve(session.Key, deadline)
		} else {
			info.Client, info.NativeID = sessionmeta.NativeID(session.Key)
		}
		row.Client = info.Client
		row.NativeSessionID = stringPtrOrNil(info.NativeID)
		row.Project = stringPtrOrNil(info.Project)
		row.Cwd = stringPtrOrNil(info.Cwd)
		row.Title = stringPtrOrNil(info.Title)
		row.T3ThreadID = stringPtrOrNil(info.T3ThreadID)
		row.T3Title = stringPtrOrNil(info.T3Title)
		row.T3ParentTitle = stringPtrOrNil(info.T3ParentTitle)
		row.Subagent = info.Subagent
		models, value, priced, whatIf := sessionModelSplit(session.Models, prices)
		row.Models = models
		row.WhatIfUSD = whatIf
		if priced {
			row.APIValueUSD = float64PtrRounded(value, roundUSD)
		}
		rows = append(rows, row)
	}
	c.JSON(http.StatusOK, usageSessionsResponse{Window: windowLabel, GeneratedAt: now.Format(time.RFC3339), Sessions: rows})
}

// ---------------------------------------------------------------------------
// GET /usage/runway

type runwayPoint struct {
	T           string  `json:"t"`
	UsedPercent float64 `json:"used_percent"`
}

type runwayWindow struct {
	Key                 string        `json:"key"`
	Kind                string        `json:"kind"`
	Model               *string       `json:"model"`
	UsedPercent         float64       `json:"used_percent"`
	ResetsAt            *string       `json:"resets_at"`
	ObservedAt          *string       `json:"observed_at"`
	BurnRatePctPerHour  *float64      `json:"burn_rate_pct_per_hour"`
	ProjectedExhaustAt  *string       `json:"projected_exhaust_at"`
	ExhaustsBeforeReset bool          `json:"exhausts_before_reset"`
	ProjectionStatus    string        `json:"projection_status"`
	LearningUntil       *string       `json:"learning_until"`
	History             []runwayPoint `json:"history"`
}

type runwayCycle struct {
	WindowKey        string  `json:"window_key"`
	ResetAt          string  `json:"reset_at"`
	FinalUsedPercent float64 `json:"final_used_percent"`
}

type runwayAccount struct {
	AuthIndex  string         `json:"auth_index"`
	Provider   string         `json:"provider"`
	Label      string         `json:"label"`
	Windows    []runwayWindow `json:"windows"`
	LastCycles []runwayCycle  `json:"last_cycles"`
}

type runwayResponse struct {
	GeneratedAt string          `json:"generated_at"`
	Accounts    []runwayAccount `json:"accounts"`
}

func runwayKind(kind string) string {
	if kind == coreauth.QuotaWindowKindFiveHour {
		return "five_hour"
	}
	return "weekly"
}

// runwayLearningUntil returns zero when the cycle ending at resetsAt has
// enough samples to project a burn rate, else an estimate of when it will:
// the first sample of the cycle plus the kind's minimum span (now when there
// is none yet), no earlier than the missing samples can arrive. A cycle that
// resets before then starts learning again, so the estimate moves past the
// reset.
func runwayLearningUntil(samples []quotahistory.Sample, resetsAt time.Time, kind string, now time.Time) time.Time {
	minSpan := runwayMinSpanWeekly
	if kind == "five_hour" {
		minSpan = runwayMinSpanFiveHour
	}
	count, first, last := quotahistory.CycleCoverage(samples, resetsAt)
	if count >= runwayMinCycleSamples && last.Sub(first) >= minSpan {
		return time.Time{}
	}
	start := first
	if count == 0 {
		start = now
	}
	ready := start.Add(minSpan)
	if byCount := now.Add(time.Duration(runwayMinCycleSamples-count) * quotahistory.MinSpacing); count < runwayMinCycleSamples && byCount.After(ready) {
		ready = byCount
	}
	if !ready.After(now) {
		ready = now.Add(quotahistory.MinSpacing)
	}
	if !resetsAt.IsZero() && ready.After(resetsAt) {
		ready = resetsAt.Add(minSpan)
	}
	return ready.UTC().Truncate(time.Second)
}

// GetUsageRunway reports, per Claude/Codex OAuth credential, each current
// quota window with its burn rate (least squares over the current cycle's
// samples in the last 6h, else 24h), the projected exhaustion time when it
// falls before the reset, a 7-day history (at most 200 points), and the final
// utilization of recently finished cycles (100 minus it is allowance that
// went unused).
//
// The burn rate and projection are only computed once the current cycle has
// at least 3 samples spanning 1h (five_hour) or 3h (weekly); until then
// projection_status is "learning" with learning_until set, the rate and
// projection are null and exhausts_before_reset is false (unless the window
// is already at 100%). "idle" means enough history but no upward pace.
//
// Endpoint: GET /v8/management/usage/runway (also under /v0/management).
func (h *Handler) GetUsageRunway(c *gin.Context) {
	now := hubNow().UTC()
	history := quotaHistorySource()
	accounts := make([]runwayAccount, 0)
	if h != nil && h.authManager != nil {
		for _, auth := range h.authManager.List() {
			if auth == nil || auth.AuthKind() != coreauth.AuthKindOAuth {
				continue
			}
			provider := strings.ToLower(strings.TrimSpace(auth.Provider))
			if provider != "claude" && provider != "codex" {
				continue
			}
			accounts = append(accounts, buildRunwayAccount(auth, provider, history, now))
		}
	}
	sort.Slice(accounts, func(i, j int) bool {
		if accounts[i].Provider != accounts[j].Provider {
			return accounts[i].Provider < accounts[j].Provider
		}
		if accounts[i].Label != accounts[j].Label {
			return accounts[i].Label < accounts[j].Label
		}
		return accounts[i].AuthIndex < accounts[j].AuthIndex
	})
	c.JSON(http.StatusOK, runwayResponse{GeneratedAt: now.Format(time.RFC3339), Accounts: accounts})
}

func buildRunwayAccount(auth *coreauth.Auth, provider string, history *quotahistory.Store, now time.Time) runwayAccount {
	account := runwayAccount{
		AuthIndex:  auth.EnsureIndex(),
		Provider:   provider,
		Label:      usageAccountLabel(auth, auth.ID),
		Windows:    make([]runwayWindow, 0, len(auth.Quota.Windows)),
		LastCycles: make([]runwayCycle, 0),
	}
	since := now.Add(-quotahistory.Retention)
	for _, window := range auth.Quota.Windows {
		if !window.Fresh(now) {
			continue
		}
		samples := history.Samples(auth.ID, window.Key, since)
		current := quotahistory.Sample{T: window.ObservedAt.UTC(), UsedPercent: window.UsedPercent, ResetsAt: window.ResetsAt.UTC()}
		if n := len(samples); !current.T.IsZero() && (n == 0 || current.T.After(samples[n-1].T)) {
			samples = append(samples, current)
		}
		entry := runwayWindow{
			Key:         window.Key,
			Kind:        runwayKind(window.Kind),
			Model:       stringPtrOrNil(window.Model),
			UsedPercent: window.UsedPercent,
			ResetsAt:    rfc3339Ptr(window.ResetsAt),
			ObservedAt:  rfc3339Ptr(window.ObservedAt),
			History:     make([]runwayPoint, 0),
		}
		learningUntil := runwayLearningUntil(samples, window.ResetsAt.UTC(), entry.Kind, now)
		rate, ok := 0.0, false
		if learningUntil.IsZero() {
			rate, ok = quotahistory.BurnRate(samples, window.ResetsAt, now, runwayBurnLookback)
			if !ok {
				rate, ok = quotahistory.BurnRate(samples, window.ResetsAt, now, runwayBurnFallback)
			}
		}
		switch {
		case !learningUntil.IsZero():
			entry.ProjectionStatus = runwayProjectionLearning
			entry.LearningUntil = rfc3339Ptr(learningUntil)
		case ok && rate > 0:
			entry.ProjectionStatus = runwayProjectionOK
		default:
			entry.ProjectionStatus = runwayProjectionIdle
		}
		if ok {
			entry.BurnRatePctPerHour = float64PtrRounded(rate, quotahistory.RoundPercent)
		}
		switch {
		case window.UsedPercent >= 100:
			// Already exhausted: an observation, not a projection.
			entry.ExhaustsBeforeReset = true
			entry.ProjectedExhaustAt = rfc3339Ptr(window.ObservedAt)
		case ok && rate > 0:
			base := window.ObservedAt
			if base.IsZero() {
				base = now
			}
			exhaustAt := base.Add(time.Duration((100 - window.UsedPercent) / rate * float64(time.Hour)))
			if exhaustAt.Before(window.ResetsAt) {
				entry.ExhaustsBeforeReset = true
				entry.ProjectedExhaustAt = rfc3339Ptr(exhaustAt)
			}
		}
		var recent []quotahistory.Sample
		for _, sample := range samples {
			if !sample.T.Before(now.Add(-runwayHistoryWindow)) {
				recent = append(recent, sample)
			}
		}
		for _, sample := range quotahistory.Downsample(recent, runwayHistoryPoints) {
			entry.History = append(entry.History, runwayPoint{T: sample.T.UTC().Format(time.RFC3339), UsedPercent: sample.UsedPercent})
		}
		account.Windows = append(account.Windows, entry)
	}
	sort.Slice(account.Windows, func(i, j int) bool { return account.Windows[i].Key < account.Windows[j].Key })

	for _, key := range history.Keys(auth.ID) {
		cycles := quotahistory.Cycles(history.Samples(auth.ID, key, since), now)
		if len(cycles) > runwayCyclesPerWindow {
			cycles = cycles[len(cycles)-runwayCyclesPerWindow:]
		}
		for _, cycle := range cycles {
			account.LastCycles = append(account.LastCycles, runwayCycle{WindowKey: key, ResetAt: cycle.ResetAt.UTC().Format(time.RFC3339), FinalUsedPercent: cycle.FinalUsedPercent})
		}
	}
	sort.SliceStable(account.LastCycles, func(i, j int) bool {
		if account.LastCycles[i].ResetAt != account.LastCycles[j].ResetAt {
			return account.LastCycles[i].ResetAt > account.LastCycles[j].ResetAt
		}
		return account.LastCycles[i].WindowKey < account.LastCycles[j].WindowKey
	})
	return account
}

// ---------------------------------------------------------------------------
// GET /usage/value

type usageValueAccount struct {
	AuthIndex                string   `json:"auth_index"`
	Provider                 string   `json:"provider"`
	Label                    string   `json:"label"`
	Requests                 int64    `json:"requests"`
	APIValueUSD              *float64 `json:"api_value_usd"`
	HasUnpricedUsage         bool     `json:"has_unpriced_usage"`
	CacheSavingsUSD          *float64 `json:"cache_savings_usd"`
	NoCacheValueUSD          *float64 `json:"no_cache_value_usd"`
	SubscriptionUSDPerMonth  *float64 `json:"subscription_usd_per_month"`
	SubscriptionUSDForWindow *float64 `json:"subscription_usd_for_window"`
	ValueRatio               *float64 `json:"value_ratio"`
}

type usageValueTotals struct {
	APIValueUSD              float64  `json:"api_value_usd"`
	CacheSavingsUSD          float64  `json:"cache_savings_usd"`
	NoCacheValueUSD          float64  `json:"no_cache_value_usd"`
	HasUnpricedUsage         bool     `json:"has_unpriced_usage"`
	SubscriptionUSDForWindow float64  `json:"subscription_usd_for_window"`
	ValueRatio               *float64 `json:"value_ratio"`
}

type usageValueResponse struct {
	Window         string              `json:"window"`
	GeneratedAt    string              `json:"generated_at"`
	Accounts       []usageValueAccount `json:"accounts"`
	Totals         usageValueTotals    `json:"totals"`
	UnpricedModels []string            `json:"unpriced_models"`
}

// GetUsageValue compares each credential's API-equivalent value with its
// subscription cost prorated to the window (monthly cost x window / 30 days).
// api_value_usd is null when the credential's usage is entirely unpriced;
// value_ratio is null without a configured subscription or priced usage.
// cache_savings_usd (cache reads priced as fresh input minus their cache
// price) and no_cache_value_usd (the same traffic with every prompt token at
// the input rate) cover priced usage only, like api_value_usd; totals carry
// has_unpriced_usage when any credential's value leaves usage out.
//
// Endpoint: GET /v8/management/usage/value?window=24h|7d|30d (also under /v0/management).
func (h *Handler) GetUsageValue(c *gin.Context) {
	windowLabel, window, ok := parseHubWindow(c, "7d", "24h", "7d", "30d")
	if !ok {
		return
	}
	now := hubNow().UTC()
	var usage []usagestats.ModelUsage
	if stats := usageStatsSource(); stats != nil {
		usage = stats.QueryModels(window)
	}
	byIndex, _ := h.hubAuths()
	hub := h.hubConfig()
	prices := hub.PriceTable()
	unpriced := make(map[string]struct{})

	type accumulator struct {
		account usageValueAccount
		authID  string
		value   float64
		savings float64
		noCache float64
		priced  bool
	}
	accounts := make(map[string]*accumulator)
	get := func(authIndex, authID, provider string) *accumulator {
		entry := accounts[authIndex]
		if entry == nil {
			entry = &accumulator{account: usageValueAccount{AuthIndex: authIndex, Provider: provider}, authID: authID}
			accounts[authIndex] = entry
		}
		return entry
	}
	for _, row := range usage {
		entry := get(row.AuthIndex, row.AuthID, row.Provider)
		entry.account.Requests += row.Totals.Requests
		if price, _, priced := prices.Lookup(row.Model); priced {
			entry.value += apiValueUSD(price, row.Totals)
			entry.savings += cacheSavingsUSD(price, row.Totals)
			entry.noCache += noCacheValueUSD(price, row.Totals)
			entry.priced = true
		} else {
			entry.account.HasUnpricedUsage = true
			unpriced[row.Model] = struct{}{}
		}
	}
	for index, auth := range byIndex {
		provider := strings.ToLower(strings.TrimSpace(auth.Provider))
		if auth.AuthKind() == coreauth.AuthKindOAuth && (provider == "claude" || provider == "codex") {
			get(index, auth.ID, provider)
		}
	}

	var totals usageValueTotals
	out := make([]usageValueAccount, 0, len(accounts))
	for index, entry := range accounts {
		account := entry.account
		auth := byIndex[index]
		if auth != nil {
			account.Provider = strings.ToLower(strings.TrimSpace(auth.Provider))
		}
		account.Label = usageAccountLabel(auth, entry.authID)
		if entry.priced || account.Requests == 0 {
			account.APIValueUSD = float64PtrRounded(entry.value, roundUSD)
			account.CacheSavingsUSD = float64PtrRounded(entry.savings, roundUSD)
			account.NoCacheValueUSD = float64PtrRounded(entry.noCache, roundUSD)
			totals.APIValueUSD += entry.value
			totals.CacheSavingsUSD += entry.savings
			totals.NoCacheValueUSD += entry.noCache
		}
		totals.HasUnpricedUsage = totals.HasUnpricedUsage || account.HasUnpricedUsage
		fileName := entry.authID
		if auth != nil && strings.TrimSpace(auth.FileName) != "" {
			fileName = auth.FileName
		}
		if monthly, configured := hub.SubscriptionFor(fileName, index); configured {
			forWindow := monthly * float64(window) / float64(hubMonth)
			account.SubscriptionUSDPerMonth = float64PtrRounded(monthly, roundUSD)
			account.SubscriptionUSDForWindow = float64PtrRounded(forWindow, roundUSD)
			totals.SubscriptionUSDForWindow += forWindow
			if account.APIValueUSD != nil && forWindow > 0 {
				account.ValueRatio = float64PtrRounded(entry.value/forWindow, roundRatio)
			}
		}
		out = append(out, account)
	}
	if totals.SubscriptionUSDForWindow > 0 {
		totals.ValueRatio = float64PtrRounded(totals.APIValueUSD/totals.SubscriptionUSDForWindow, roundRatio)
	}
	totals.APIValueUSD = roundUSD(totals.APIValueUSD)
	totals.CacheSavingsUSD = roundUSD(totals.CacheSavingsUSD)
	totals.NoCacheValueUSD = roundUSD(totals.NoCacheValueUSD)
	totals.SubscriptionUSDForWindow = roundUSD(totals.SubscriptionUSDForWindow)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		if out[i].Label != out[j].Label {
			return out[i].Label < out[j].Label
		}
		return out[i].AuthIndex < out[j].AuthIndex
	})
	c.JSON(http.StatusOK, usageValueResponse{Window: windowLabel, GeneratedAt: now.Format(time.RFC3339), Accounts: out, Totals: totals, UnpricedModels: sortedKeys(unpriced)})
}

// ---------------------------------------------------------------------------
// GET /usage/events

type usageEventRow struct {
	T          string  `json:"t"`
	Kind       string  `json:"kind"`
	AuthIndex  *string `json:"auth_index"`
	Provider   *string `json:"provider"`
	Model      *string `json:"model"`
	SessionKey *string `json:"session_key"`
	StatusCode *int    `json:"status_code"`
	Detail     *string `json:"detail"`
}

type usageEventCount struct {
	Hour  string `json:"hour"`
	Kind  string `json:"kind"`
	Count int    `json:"count"`
}

type usageEventsResponse struct {
	Window      string            `json:"window"`
	GeneratedAt string            `json:"generated_at"`
	Events      []usageEventRow   `json:"events"`
	Counts      []usageEventCount `json:"counts"`
}

// GetUsageEvents lists recent routing and credential-health events, newest
// first (at most limit), plus hourly counts per kind over the whole window.
// Credentials appear only as auth_index; details never contain bodies.
//
// Endpoint: GET /v8/management/usage/events?window=24h|7d&limit=N (also under /v0/management).
func (h *Handler) GetUsageEvents(c *gin.Context) {
	windowLabel, window, ok := parseHubWindow(c, "24h", "24h", "7d")
	if !ok {
		return
	}
	limit := parseHubLimit(c, 200, hubevents.MaxEvents)
	now := hubNow().UTC()
	var events []hubevents.Event
	if store := hubEventsSource(); store != nil {
		events = store.Query(now.Add(-window))
	}
	_, byID := h.hubAuths()
	indexFor := func(authID, fallback string) string {
		if auth := byID[authID]; auth != nil {
			if index := auth.EnsureIndex(); index != "" {
				return index
			}
		}
		return fallback
	}
	counts := make(map[[2]string]int)
	rows := make([]usageEventRow, 0, minInt(limit, len(events)))
	for _, event := range events {
		hour := event.T.UTC().Truncate(time.Hour).Format(time.RFC3339)
		counts[[2]string{hour, event.Kind}]++
		if len(rows) >= limit {
			continue
		}
		row := usageEventRow{
			T:          event.T.UTC().Format(time.RFC3339),
			Kind:       event.Kind,
			AuthIndex:  stringPtrOrNil(indexFor(event.AuthID, event.AuthIndex)),
			Provider:   stringPtrOrNil(event.Provider),
			Model:      stringPtrOrNil(event.Model),
			SessionKey: stringPtrOrNil(event.SessionKey),
			Detail:     stringPtrOrNil(event.Detail),
		}
		if event.StatusCode != 0 {
			status := event.StatusCode
			row.StatusCode = &status
		}
		if event.RelatedAuthID != "" {
			bound := "bound auth_index=" + indexFor(event.RelatedAuthID, "unknown")
			row.Detail = &bound
		}
		rows = append(rows, row)
	}
	countRows := make([]usageEventCount, 0, len(counts))
	for key, count := range counts {
		countRows = append(countRows, usageEventCount{Hour: key[0], Kind: key[1], Count: count})
	}
	sort.Slice(countRows, func(i, j int) bool {
		if countRows[i].Hour != countRows[j].Hour {
			return countRows[i].Hour < countRows[j].Hour
		}
		return countRows[i].Kind < countRows[j].Kind
	})
	c.JSON(http.StatusOK, usageEventsResponse{Window: windowLabel, GeneratedAt: now.Format(time.RFC3339), Events: rows, Counts: countRows})
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
