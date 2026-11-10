package management

import (
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestats"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// usageStatsSource returns the in-process usage aggregator; tests replace it.
var usageStatsSource = usagestats.Default

var usageAccountWindows = map[string]time.Duration{
	"1h":  time.Hour,
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
}

type usageSeriesPoint struct {
	T             string `json:"t"`
	Requests      int64  `json:"requests"`
	Input         int64  `json:"input"`
	Output        int64  `json:"output"`
	CacheRead     int64  `json:"cache_read"`
	CacheCreation int64  `json:"cache_creation"`
}

type usageTotals struct {
	Requests              int64   `json:"requests"`
	InputTokens           int64   `json:"input_tokens"`
	OutputTokens          int64   `json:"output_tokens"`
	CacheReadTokens       int64   `json:"cache_read_tokens"`
	CacheCreationTokens   int64   `json:"cache_creation_tokens"`
	CacheCreation5mTokens int64   `json:"cache_creation_5m_tokens"`
	CacheCreation1hTokens int64   `json:"cache_creation_1h_tokens"`
	UncachedInputTokens   int64   `json:"uncached_input_tokens"`
	CacheHitRatio         float64 `json:"cache_hit_ratio"`
}

func newUsageTotals(tokens usagestats.Tokens) usageTotals {
	return usageTotals{
		Requests:              tokens.Requests,
		InputTokens:           tokens.InputTokens,
		OutputTokens:          tokens.OutputTokens,
		CacheReadTokens:       tokens.CacheReadTokens,
		CacheCreationTokens:   tokens.CacheCreationTokens,
		CacheCreation5mTokens: tokens.CacheCreation5mTokens,
		CacheCreation1hTokens: tokens.CacheCreation1hTokens,
		UncachedInputTokens:   tokens.UncachedInputTokens,
		CacheHitRatio:         tokens.CacheHitRatio(),
	}
}

type usageAccountEntry struct {
	AuthIndex string `json:"auth_index"`
	AuthID    string `json:"auth_id"`
	Provider  string `json:"provider"`
	Label     string `json:"label"`
	usageTotals
	Series []usageSeriesPoint `json:"series"`
}

type usageAccountsResponse struct {
	Window      string              `json:"window"`
	GeneratedAt string              `json:"generated_at"`
	Accounts    []usageAccountEntry `json:"accounts"`
	Totals      usageTotals         `json:"totals"`
}

// GetUsageAccounts reports hourly per-credential usage.
//
// Endpoint: GET /v8/management/usage/accounts?window=1h|24h|7d (also under /v0/management).
//
// input_tokens is the provider-reported value (Claude excludes cached tokens,
// Codex includes them); uncached_input_tokens is normalized across providers and
// cache_hit_ratio = cache_read / (cache_read + cache_creation + uncached_input).
// The series has one zero-filled point per hour overlapping the window. Idle
// OAuth credentials are listed with zero usage.
func (h *Handler) GetUsageAccounts(c *gin.Context) {
	windowLabel := strings.ToLower(strings.TrimSpace(c.DefaultQuery("window", "24h")))
	window, ok := usageAccountWindows[windowLabel]
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid window; use 1h, 24h or 7d"})
		return
	}
	now := time.Now().UTC()
	var usage []usagestats.AccountUsage
	if stats := usageStatsSource(); stats != nil {
		usage = stats.Query(window)
	}

	authsByIndex := make(map[string]*coreauth.Auth)
	if h != nil && h.authManager != nil {
		for _, auth := range h.authManager.List() {
			if auth == nil {
				continue
			}
			if index := auth.EnsureIndex(); index != "" {
				authsByIndex[index] = auth
			}
		}
	}

	seriesHours := seriesHourStarts(now, window)
	accounts := make([]usageAccountEntry, 0, len(usage)+len(authsByIndex))
	var totals usagestats.Tokens
	seen := make(map[string]struct{}, len(usage))
	for _, entry := range usage {
		seen[entry.AuthIndex] = struct{}{}
		account := usageAccountEntry{
			AuthIndex:   entry.AuthIndex,
			AuthID:      entry.AuthID,
			Provider:    entry.Provider,
			usageTotals: newUsageTotals(entry.Totals),
			Series:      make([]usageSeriesPoint, 0, len(entry.Series)),
		}
		if auth := authsByIndex[entry.AuthIndex]; auth != nil {
			account.AuthID = auth.ID
			account.Provider = strings.ToLower(strings.TrimSpace(auth.Provider))
		}
		account.Label = usageAccountLabel(authsByIndex[entry.AuthIndex], account.AuthID)
		for _, bucket := range entry.Series {
			account.Series = append(account.Series, usageSeriesPoint{
				T:             time.Unix(bucket.Hour, 0).UTC().Format(time.RFC3339),
				Requests:      bucket.Requests,
				Input:         bucket.InputTokens,
				Output:        bucket.OutputTokens,
				CacheRead:     bucket.CacheReadTokens,
				CacheCreation: bucket.CacheCreationTokens,
			})
		}
		totals.Add(entry.Totals)
		accounts = append(accounts, account)
	}
	for index, auth := range authsByIndex {
		if _, ok := seen[index]; ok || auth.AuthKind() != coreauth.AuthKindOAuth {
			continue
		}
		account := usageAccountEntry{
			AuthIndex: index,
			AuthID:    auth.ID,
			Provider:  strings.ToLower(strings.TrimSpace(auth.Provider)),
			Label:     usageAccountLabel(auth, auth.ID),
			Series:    make([]usageSeriesPoint, 0, len(seriesHours)),
		}
		for _, hour := range seriesHours {
			account.Series = append(account.Series, usageSeriesPoint{T: hour})
		}
		accounts = append(accounts, account)
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
	c.JSON(http.StatusOK, usageAccountsResponse{
		Window:      windowLabel,
		GeneratedAt: now.Format(time.RFC3339),
		Accounts:    accounts,
		Totals:      newUsageTotals(totals),
	})
}

func seriesHourStarts(now time.Time, window time.Duration) []string {
	start := now.Add(-window).Truncate(time.Hour)
	end := now.Truncate(time.Hour)
	var hours []string
	for hour := start; !hour.After(end); hour = hour.Add(time.Hour) {
		hours = append(hours, hour.Format(time.RFC3339))
	}
	return hours
}

func usageAccountLabel(auth *coreauth.Auth, authID string) string {
	if auth != nil {
		if name := strings.TrimSpace(auth.FileName); name != "" {
			return filepath.Base(name)
		}
		return auth.ID
	}
	if authID == "" {
		return ""
	}
	return filepath.Base(authID)
}

type routingOrderEntry struct {
	AuthIndex           string   `json:"auth_index"`
	AuthID              string   `json:"auth_id"`
	Label               string   `json:"label"`
	Rank                int      `json:"rank"`
	Eligible            bool     `json:"eligible"`
	SkipReason          string   `json:"skip_reason"`
	HasData             bool     `json:"has_data"`
	WeeklyKey           string   `json:"weekly_key"`
	WeeklyUsedPercent   *float64 `json:"weekly_used_percent"`
	WeeklyResetsAt      *string  `json:"weekly_resets_at"`
	FiveHourUsedPercent *float64 `json:"five_hour_used_percent"`
	FiveHourResetsAt    *string  `json:"five_hour_resets_at"`
	Disabled            bool     `json:"disabled"`
}

type routingOrderResponse struct {
	Strategy    string                         `json:"strategy"`
	Model       string                         `json:"model"`
	GeneratedAt string                         `json:"generated_at"`
	Providers   map[string][]routingOrderEntry `json:"providers"`
}

// GetRoutingOrder shows the order soonest-reset would assign new sessions,
// computed with the selector's own ranking (coreauth.RankSoonestReset via
// SoonestResetOrder). ?model= picks model-scoped weekly windows (e.g. a Fable
// model uses seven_day_fable); without it the overall seven_day window is used.
// Rank 1 is the next pick; credentials without data rotate round-robin at
// runtime, so their relative order here is by ID.
//
// Endpoint: GET /v8/management/routing/order[?model=] (also under /v0/management).
func (h *Handler) GetRoutingOrder(c *gin.Context) {
	model := strings.TrimSpace(c.Query("model"))
	strategy := "round-robin"
	opts := coreauth.DefaultSoonestResetOptions()
	if h != nil && h.cfg != nil {
		if normalized, ok := normalizeRoutingStrategy(h.cfg.Routing.Strategy); ok {
			strategy = normalized
		} else {
			strategy = strings.TrimSpace(h.cfg.Routing.Strategy)
		}
		opts = coreauth.SoonestResetOptions{
			MinRemainingPercent: h.cfg.Routing.SoonestReset.EffectiveMinRemainingPercent(),
			Max5hPercent:        h.cfg.Routing.SoonestReset.EffectiveMax5hPercent(),
		}
	}
	grouped := map[string][]*coreauth.Auth{"claude": nil, "codex": nil}
	if h != nil && h.authManager != nil {
		for _, auth := range h.authManager.List() {
			if auth == nil {
				continue
			}
			provider := strings.ToLower(strings.TrimSpace(auth.Provider))
			if _, tracked := grouped[provider]; tracked {
				grouped[provider] = append(grouped[provider], auth)
			}
		}
	}
	now := time.Now()
	providers := make(map[string][]routingOrderEntry, len(grouped))
	for provider, auths := range grouped {
		order := coreauth.SoonestResetOrder(auths, model, opts, now)
		entries := make([]routingOrderEntry, 0, len(order))
		for i, ranking := range order {
			auth := ranking.Auth
			entry := routingOrderEntry{
				AuthIndex:  auth.EnsureIndex(),
				AuthID:     auth.ID,
				Label:      usageAccountLabel(auth, auth.ID),
				Rank:       i + 1,
				Eligible:   ranking.Eligible,
				SkipReason: ranking.SkipReason,
				HasData:    ranking.HasData,
				Disabled:   auth.Disabled || auth.Status == coreauth.StatusDisabled,
			}
			if ranking.Weekly != nil {
				entry.WeeklyKey = ranking.Weekly.Key
				entry.WeeklyUsedPercent = float64Ptr(ranking.Weekly.UsedPercent)
				entry.WeeklyResetsAt = rfc3339Ptr(ranking.Weekly.ResetsAt)
			}
			if ranking.FiveHour != nil {
				entry.FiveHourUsedPercent = float64Ptr(ranking.FiveHour.UsedPercent)
				entry.FiveHourResetsAt = rfc3339Ptr(ranking.FiveHour.ResetsAt)
			}
			entries = append(entries, entry)
		}
		providers[provider] = entries
	}
	c.JSON(http.StatusOK, routingOrderResponse{
		Strategy:    strategy,
		Model:       model,
		GeneratedAt: now.UTC().Format(time.RFC3339),
		Providers:   providers,
	})
}

func float64Ptr(value float64) *float64 { return &value }

func rfc3339Ptr(value time.Time) *string {
	if value.IsZero() {
		return nil
	}
	formatted := value.UTC().Format(time.RFC3339)
	return &formatted
}
