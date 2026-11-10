package auth

import (
	"context"
	"sort"
	"sync"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// soonestResetTieWindow treats weekly resets this close together as a tie.
const soonestResetTieWindow = time.Minute

// Soonest-reset skip reasons reported by RankSoonestReset.
const (
	SoonestResetSkipWeeklyBelowMin   = "weekly_remaining_below_min"
	SoonestResetSkipFiveHourAboveMax = "five_hour_above_max"
)

// SoonestResetOptions holds the soonest-reset exclusion thresholds.
type SoonestResetOptions struct {
	// MinRemainingPercent excludes candidates whose weekly remaining allowance is below it.
	MinRemainingPercent float64
	// Max5hPercent excludes candidates whose 5-hour utilization is at or above it.
	Max5hPercent float64
}

// DefaultSoonestResetOptions returns the default thresholds (3% weekly floor, 98% 5h ceiling).
func DefaultSoonestResetOptions() SoonestResetOptions {
	return SoonestResetOptions{
		MinRemainingPercent: internalconfig.DefaultSoonestResetMinRemainingPercent,
		Max5hPercent:        internalconfig.DefaultSoonestResetMax5hPercent,
	}
}

// SoonestResetRanking is one candidate's place in the soonest-reset order.
type SoonestResetRanking struct {
	Auth *Auth
	// Eligible is false when a threshold excluded the candidate. When every
	// candidate violates a threshold all of them stay eligible (fallback to all)
	// and SkipReason still names the violation.
	Eligible   bool
	SkipReason string
	// HasData reports a fresh weekly window; candidates without one rank after
	// those with data.
	HasData bool
	// Weekly is the relevant fresh weekly window (model-scoped when one matches
	// the requested model, otherwise the overall seven_day window), or nil.
	Weekly *QuotaWindow
	// FiveHour is the relevant fresh 5-hour window, or nil.
	FiveHour *QuotaWindow
}

// SoonestResetSelector routes to the credential whose weekly quota window resets
// soonest, so allowance that is about to expire is spent first. It is a custom
// selector and therefore takes the manager's legacy pick path; with session
// affinity enabled it is the fallback for new and failed-over bindings only.
type SoonestResetSelector struct {
	opts SoonestResetOptions
	now  func() time.Time

	mu         sync.Mutex
	lastPicked map[string]string
}

// NewSoonestResetSelector creates a soonest-reset selector.
func NewSoonestResetSelector(opts SoonestResetOptions) *SoonestResetSelector {
	return &SoonestResetSelector{opts: opts}
}

// Options returns the selector thresholds.
func (s *SoonestResetSelector) Options() SoonestResetOptions {
	if s == nil {
		return DefaultSoonestResetOptions()
	}
	return s.opts
}

// Pick implements Selector. Candidates arrive already filtered by provider,
// model support, priority, and cooldown, so they are only narrowed to the
// highest priority tier and the websocket transport preference here; the
// route model is not rechecked against unrelated alias cooldowns.
func (s *SoonestResetSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	candidates := make([]*Auth, 0, len(auths))
	for _, auth := range auths {
		if auth != nil {
			candidates = append(candidates, auth)
		}
	}
	candidates = preferCodexWebsocketAuths(ctx, provider, highestPriorityAuths(candidates))
	if len(candidates) == 0 {
		return nil, &Error{Code: "auth_not_found", Message: "no auth candidates"}
	}
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	ranked := RankSoonestReset(candidates, model, s.opts, now)
	if top := ranked[0]; top.Eligible && top.HasData {
		return top.Auth, nil
	}

	// No eligible candidate has quota data: rotate among the eligible ones.
	pool := make([]*Auth, 0, len(ranked))
	for _, entry := range ranked {
		if entry.Eligible {
			pool = append(pool, entry.Auth)
		}
	}
	sort.Slice(pool, func(i, j int) bool { return pool[i].ID < pool[j].ID })
	key := provider + ":" + canonicalModelLookupKey(model)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastPicked == nil || (len(s.lastPicked) >= 4096 && s.lastPicked[key] == "") {
		s.lastPicked = make(map[string]string)
	}
	picked := pool[successorIndex(pool, s.lastPicked[key])]
	s.lastPicked[key] = picked.ID
	return picked, nil
}

// RankSoonestReset orders candidates for the soonest-reset strategy:
//
//  1. The relevant weekly window is a fresh model-scoped 7d window whose family
//     matches model (most specific family wins), else the overall seven_day
//     window. The 5h window is chosen the same way. Windows whose reset already
//     passed are stale and treated as unknown.
//  2. Candidates with weekly remaining below MinRemainingPercent or 5h usage at
//     or above Max5hPercent are excluded, unless that would exclude everyone.
//  3. Eligible candidates with data sort by weekly reset ascending; resets
//     within a minute tie and prefer more remaining allowance, then auth ID.
//  4. Eligible candidates without data follow (by ID; Pick rotates them),
//     then excluded candidates in the same order.
func RankSoonestReset(auths []*Auth, model string, opts SoonestResetOptions, now time.Time) []SoonestResetRanking {
	ranked := make([]SoonestResetRanking, 0, len(auths))
	eligibleCount := 0
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		entry := SoonestResetRanking{Auth: auth, Eligible: true}
		entry.Weekly, entry.FiveHour = RelevantQuotaWindows(auth.Quota.Windows, model, now)
		entry.HasData = entry.Weekly != nil
		switch {
		case entry.Weekly != nil && entry.Weekly.RemainingPercent() < opts.MinRemainingPercent:
			entry.Eligible = false
			entry.SkipReason = SoonestResetSkipWeeklyBelowMin
		case entry.FiveHour != nil && entry.FiveHour.UsedPercent >= opts.Max5hPercent:
			entry.Eligible = false
			entry.SkipReason = SoonestResetSkipFiveHourAboveMax
		}
		if entry.Eligible {
			eligibleCount++
		}
		ranked = append(ranked, entry)
	}
	if eligibleCount == 0 {
		for i := range ranked {
			ranked[i].Eligible = true
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		a, b := ranked[i], ranked[j]
		if a.Eligible != b.Eligible {
			return a.Eligible
		}
		if a.HasData != b.HasData {
			return a.HasData
		}
		if a.HasData && !a.Weekly.ResetsAt.Equal(b.Weekly.ResetsAt) {
			return a.Weekly.ResetsAt.Before(b.Weekly.ResetsAt)
		}
		return a.Auth.ID < b.Auth.ID
	})
	breakSoonestResetTies(ranked)
	return ranked
}

// breakSoonestResetTies reorders runs of data-bearing candidates whose weekly
// resets fall within soonestResetTieWindow of the run's first reset, preferring
// more remaining allowance and then auth ID. Runs never cross the
// eligible/ineligible boundary.
func breakSoonestResetTies(ranked []SoonestResetRanking) {
	for start := 0; start < len(ranked); {
		if !ranked[start].HasData {
			start++
			continue
		}
		end := start + 1
		for end < len(ranked) && ranked[end].HasData && ranked[end].Eligible == ranked[start].Eligible &&
			ranked[end].Weekly.ResetsAt.Sub(ranked[start].Weekly.ResetsAt) <= soonestResetTieWindow {
			end++
		}
		if end-start > 1 {
			run := ranked[start:end]
			sort.SliceStable(run, func(i, j int) bool {
				ri, rj := run[i].Weekly.RemainingPercent(), run[j].Weekly.RemainingPercent()
				if ri != rj {
					return ri > rj
				}
				return run[i].Auth.ID < run[j].Auth.ID
			})
		}
		start = end
	}
}

// RelevantQuotaWindows picks the fresh weekly and 5-hour windows that apply to
// model (model-scoped first, then the overall window). Results are copies.
func RelevantQuotaWindows(windows []QuotaWindow, model string, now time.Time) (weekly, fiveHour *QuotaWindow) {
	return relevantQuotaWindow(windows, QuotaWindowKindWeekly, QuotaWindowKeyWeekly, model, now),
		relevantQuotaWindow(windows, QuotaWindowKindFiveHour, QuotaWindowKeyFiveHour, model, now)
}

func relevantQuotaWindow(windows []QuotaWindow, kind, overallKey, model string, now time.Time) *QuotaWindow {
	var scoped, overall *QuotaWindow
	for i := range windows {
		window := &windows[i]
		if window.Kind != kind || !window.Fresh(now) {
			continue
		}
		if window.Key == overallKey {
			overall = window
			continue
		}
		if window.Model == "" || !QuotaWindowMatchesModel(window.Model, model) {
			continue
		}
		if scoped == nil || len(window.Model) > len(scoped.Model) || (len(window.Model) == len(scoped.Model) && window.Key < scoped.Key) {
			scoped = window
		}
	}
	chosen := scoped
	if chosen == nil {
		chosen = overall
	}
	if chosen == nil {
		return nil
	}
	copyWindow := *chosen
	return &copyWindow
}
