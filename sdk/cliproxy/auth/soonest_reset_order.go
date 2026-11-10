package auth

import (
	"sort"
	"time"
)

// Additional skip reasons reported by SoonestResetOrder for credentials the
// manager would never hand to the selector.
const (
	SoonestResetSkipDisabled      = "disabled"
	SoonestResetSkipUnavailable   = "unavailable"
	SoonestResetSkipLowerPriority = "lower_priority"
)

// SoonestResetOrder ranks a whole provider pool the way a new request would see
// it: credentials that are enabled, not cooling down for model, and in the
// highest available priority tier are ranked with RankSoonestReset (the same
// function the selector uses). Lower-priority, cooled-down, and disabled
// credentials follow, ineligible, with their skip reason. Window fields are
// filled for every entry.
func SoonestResetOrder(auths []*Auth, model string, opts SoonestResetOptions, now time.Time) []SoonestResetRanking {
	var available, unavailable, disabled []*Auth
	for _, auth := range auths {
		switch {
		case auth == nil:
		case auth.Disabled || auth.Status == StatusDisabled:
			disabled = append(disabled, auth)
		case !IsAuthAvailableForModel(auth, model, now):
			unavailable = append(unavailable, auth)
		default:
			available = append(available, auth)
		}
	}
	sort.Slice(available, func(i, j int) bool { return available[i].ID < available[j].ID })
	topTier := highestPriorityAuths(available)
	inTopTier := make(map[string]struct{}, len(topTier))
	for _, auth := range topTier {
		inTopTier[auth.ID] = struct{}{}
	}
	var lower []*Auth
	for _, auth := range available {
		if _, ok := inTopTier[auth.ID]; !ok {
			lower = append(lower, auth)
		}
	}

	order := RankSoonestReset(topTier, model, opts, now)
	for _, entry := range RankSoonestReset(lower, model, opts, now) {
		entry.Eligible = false
		entry.SkipReason = SoonestResetSkipLowerPriority
		order = append(order, entry)
	}
	for _, group := range []struct {
		auths  []*Auth
		reason string
	}{{unavailable, SoonestResetSkipUnavailable}, {disabled, SoonestResetSkipDisabled}} {
		sort.Slice(group.auths, func(i, j int) bool { return group.auths[i].ID < group.auths[j].ID })
		for _, auth := range group.auths {
			entry := SoonestResetRanking{Auth: auth, SkipReason: group.reason}
			entry.Weekly, entry.FiveHour = RelevantQuotaWindows(auth.Quota.Windows, model, now)
			entry.HasData = entry.Weekly != nil
			order = append(order, entry)
		}
	}
	return order
}
