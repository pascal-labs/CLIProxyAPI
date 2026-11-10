package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

var soonestResetTestNow = time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)

func soonestAuth(id string, windows ...QuotaWindow) *Auth {
	return &Auth{ID: id, Provider: "claude", Quota: QuotaState{Windows: windows}}
}

func weeklyWindow(used float64, resetIn time.Duration) QuotaWindow {
	return QuotaWindow{Key: QuotaWindowKeyWeekly, Kind: QuotaWindowKindWeekly, UsedPercent: used, ResetsAt: soonestResetTestNow.Add(resetIn), ObservedAt: soonestResetTestNow, Source: QuotaWindowSourcePoll}
}

func fableWindow(used float64, resetIn time.Duration) QuotaWindow {
	return QuotaWindow{Key: "seven_day_fable", Kind: QuotaWindowKindWeekly, Model: "fable", UsedPercent: used, ResetsAt: soonestResetTestNow.Add(resetIn), ObservedAt: soonestResetTestNow, Source: QuotaWindowSourcePoll}
}

func fiveHourWindow(used float64, resetIn time.Duration) QuotaWindow {
	return QuotaWindow{Key: QuotaWindowKeyFiveHour, Kind: QuotaWindowKindFiveHour, UsedPercent: used, ResetsAt: soonestResetTestNow.Add(resetIn), ObservedAt: soonestResetTestNow, Source: QuotaWindowSourceHeaders}
}

func newTestSoonestResetSelector() *SoonestResetSelector {
	selector := NewSoonestResetSelector(DefaultSoonestResetOptions())
	selector.now = func() time.Time { return soonestResetTestNow }
	return selector
}

func rankedIDs(ranked []SoonestResetRanking) []string {
	ids := make([]string, 0, len(ranked))
	for _, entry := range ranked {
		ids = append(ids, entry.Auth.ID)
	}
	return ids
}

func assertIDs(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func pickID(t *testing.T, selector Selector, model string, opts cliproxyexecutor.Options, auths []*Auth) string {
	t.Helper()
	picked, errPick := selector.Pick(context.Background(), "claude", model, opts, auths)
	if errPick != nil {
		t.Fatalf("Pick() error = %v", errPick)
	}
	return picked.ID
}

func TestSoonestResetOrdersByWeeklyReset(t *testing.T) {
	auths := []*Auth{
		soonestAuth("a", weeklyWindow(10, 72*time.Hour)),
		soonestAuth("b", weeklyWindow(80, 24*time.Hour)),
		soonestAuth("c", weeklyWindow(50, 48*time.Hour)),
	}
	ranked := RankSoonestReset(auths, "", DefaultSoonestResetOptions(), soonestResetTestNow)
	assertIDs(t, rankedIDs(ranked), "b", "c", "a")
	if got := pickID(t, newTestSoonestResetSelector(), "", cliproxyexecutor.Options{}, auths); got != "b" {
		t.Fatalf("Pick() = %q, want b (resets soonest)", got)
	}
}

func TestSoonestResetTieWithinMinutePrefersMoreRemaining(t *testing.T) {
	auths := []*Auth{
		soonestAuth("a", weeklyWindow(80, 24*time.Hour)),
		soonestAuth("b", weeklyWindow(40, 24*time.Hour+30*time.Second)),
		soonestAuth("c", weeklyWindow(40, 24*time.Hour+45*time.Second)),
		soonestAuth("d", weeklyWindow(0, 24*time.Hour+5*time.Minute)),
	}
	ranked := RankSoonestReset(auths, "", DefaultSoonestResetOptions(), soonestResetTestNow)
	// b and c tie on remaining (60%) so the auth ID decides; d resets later.
	assertIDs(t, rankedIDs(ranked), "b", "c", "a", "d")
}

func TestSoonestResetThresholdExclusionAndFallbackToAll(t *testing.T) {
	auths := []*Auth{
		soonestAuth("weekly-exhausted", weeklyWindow(98, 12*time.Hour)),
		soonestAuth("five-hour-hot", weeklyWindow(20, 18*time.Hour), fiveHourWindow(99, time.Hour)),
		soonestAuth("healthy", weeklyWindow(50, 72*time.Hour), fiveHourWindow(40, 2*time.Hour)),
	}
	ranked := RankSoonestReset(auths, "", DefaultSoonestResetOptions(), soonestResetTestNow)
	assertIDs(t, rankedIDs(ranked), "healthy", "weekly-exhausted", "five-hour-hot")
	if ranked[1].Eligible || ranked[1].SkipReason != SoonestResetSkipWeeklyBelowMin {
		t.Fatalf("weekly-exhausted = %+v", ranked[1])
	}
	if ranked[2].Eligible || ranked[2].SkipReason != SoonestResetSkipFiveHourAboveMax {
		t.Fatalf("five-hour-hot = %+v", ranked[2])
	}
	if got := pickID(t, newTestSoonestResetSelector(), "", cliproxyexecutor.Options{}, auths); got != "healthy" {
		t.Fatalf("Pick() = %q, want healthy", got)
	}

	// When every candidate violates a threshold, all of them stay eligible.
	exhausted := auths[:2]
	ranked = RankSoonestReset(exhausted, "", DefaultSoonestResetOptions(), soonestResetTestNow)
	assertIDs(t, rankedIDs(ranked), "weekly-exhausted", "five-hour-hot")
	for _, entry := range ranked {
		if !entry.Eligible || entry.SkipReason == "" {
			t.Fatalf("fallback entry = %+v, want eligible with the violation reported", entry)
		}
	}
	if got := pickID(t, newTestSoonestResetSelector(), "", cliproxyexecutor.Options{}, exhausted); got != "weekly-exhausted" {
		t.Fatalf("Pick() = %q, want soonest reset among all", got)
	}

	// Custom thresholds: a zero floor and a 100% ceiling exclude nothing.
	custom := RankSoonestReset(auths, "", SoonestResetOptions{MinRemainingPercent: 0, Max5hPercent: 100}, soonestResetTestNow)
	assertIDs(t, rankedIDs(custom), "weekly-exhausted", "five-hour-hot", "healthy")
}

func TestSoonestResetNoDataRoundRobin(t *testing.T) {
	selector := newTestSoonestResetSelector()
	auths := []*Auth{soonestAuth("c"), soonestAuth("a"), soonestAuth("b")}
	var got []string
	for i := 0; i < 4; i++ {
		got = append(got, pickID(t, selector, "", cliproxyexecutor.Options{}, auths))
	}
	assertIDs(t, got, "a", "b", "c", "a")

	// Credentials with data always come first; no-data ones only rotate among themselves.
	mixed := []*Auth{soonestAuth("a"), soonestAuth("b"), soonestAuth("data", weeklyWindow(10, 96*time.Hour))}
	for i := 0; i < 3; i++ {
		if picked := pickID(t, selector, "claude-mixed", cliproxyexecutor.Options{}, mixed); picked != "data" {
			t.Fatalf("Pick() = %q, want the credential with data", picked)
		}
	}
	ranked := RankSoonestReset(mixed, "", DefaultSoonestResetOptions(), soonestResetTestNow)
	assertIDs(t, rankedIDs(ranked), "data", "a", "b")

	// An excluded credential with data yields to rotating no-data credentials.
	excluded := []*Auth{soonestAuth("a"), soonestAuth("b"), soonestAuth("data", weeklyWindow(99, time.Hour))}
	got = got[:0]
	for i := 0; i < 3; i++ {
		got = append(got, pickID(t, selector, "claude-excluded", cliproxyexecutor.Options{}, excluded))
	}
	assertIDs(t, got, "a", "b", "a")
}

func TestSoonestResetTreatsStaleWindowsAsUnknown(t *testing.T) {
	stale := weeklyWindow(5, -time.Hour)
	auths := []*Auth{
		soonestAuth("stale", stale),
		soonestAuth("fresh", weeklyWindow(70, 96*time.Hour)),
	}
	ranked := RankSoonestReset(auths, "", DefaultSoonestResetOptions(), soonestResetTestNow)
	assertIDs(t, rankedIDs(ranked), "fresh", "stale")
	if ranked[1].HasData || ranked[1].Weekly != nil {
		t.Fatalf("stale entry = %+v, want no data", ranked[1])
	}

	// A stale exhausted 5h window no longer excludes the credential.
	recovered := soonestAuth("recovered", weeklyWindow(10, 24*time.Hour), fiveHourWindow(100, -time.Minute))
	ranked = RankSoonestReset([]*Auth{recovered, auths[1]}, "", DefaultSoonestResetOptions(), soonestResetTestNow)
	if !ranked[0].Eligible || ranked[0].Auth.ID != "recovered" || ranked[0].FiveHour != nil {
		t.Fatalf("recovered entry = %+v", ranked[0])
	}
}

func TestSoonestResetUsesModelBucket(t *testing.T) {
	auths := []*Auth{
		soonestAuth("a", weeklyWindow(10, 24*time.Hour), fableWindow(10, 120*time.Hour)),
		soonestAuth("b", weeklyWindow(10, 72*time.Hour), fableWindow(10, 48*time.Hour)),
	}
	selector := newTestSoonestResetSelector()
	if got := pickID(t, selector, "claude-fable-5", cliproxyexecutor.Options{}, auths); got != "b" {
		t.Fatalf("fable Pick() = %q, want b (fable bucket resets soonest)", got)
	}
	if got := pickID(t, selector, "claude-opus-4-6", cliproxyexecutor.Options{}, auths); got != "a" {
		t.Fatalf("opus Pick() = %q, want a (overall weekly resets soonest)", got)
	}
	ranked := RankSoonestReset(auths, "", DefaultSoonestResetOptions(), soonestResetTestNow)
	if ranked[0].Auth.ID != "a" || ranked[0].Weekly.Key != QuotaWindowKeyWeekly {
		t.Fatalf("empty model ranking = %+v, want the overall weekly window", ranked[0])
	}

	// An exhausted model bucket only excludes the credential for that model.
	auths[1].Quota.Windows[1] = fableWindow(99, 48*time.Hour)
	if got := pickID(t, selector, "claude-fable-5", cliproxyexecutor.Options{}, auths); got != "a" {
		t.Fatalf("fable Pick() = %q, want a once b's fable bucket is exhausted", got)
	}
	ranked = RankSoonestReset(auths, "claude-opus-4-6", DefaultSoonestResetOptions(), soonestResetTestNow)
	if !ranked[1].Eligible {
		t.Fatalf("opus ranking excluded b for its fable bucket: %+v", ranked[1])
	}

	// A model without its own bucket falls back to the overall weekly window,
	// and a stale model bucket falls back as well.
	auths[0].Quota.Windows[1] = fableWindow(10, -time.Hour)
	ranked = RankSoonestReset(auths[:1], "claude-fable-5", DefaultSoonestResetOptions(), soonestResetTestNow)
	if ranked[0].Weekly == nil || ranked[0].Weekly.Key != QuotaWindowKeyWeekly {
		t.Fatalf("stale fable bucket ranking = %+v, want overall weekly", ranked[0])
	}
}

func TestSoonestResetUnderSessionAffinityKeepsBoundSessions(t *testing.T) {
	auths := []*Auth{
		soonestAuth("a", weeklyWindow(10, 24*time.Hour)),
		soonestAuth("b", weeklyWindow(10, 72*time.Hour)),
	}
	selector := NewSessionAffinitySelector(newTestSoonestResetSelector())
	defer selector.Stop()

	sessionOne := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{"session-one"}}}
	if got := pickID(t, selector, "claude-opus-4-6", sessionOne, auths); got != "a" {
		t.Fatalf("first binding = %q, want a (soonest reset)", got)
	}

	// b now resets sooner than a.
	auths[1].Quota.Windows = []QuotaWindow{weeklyWindow(10, time.Hour)}
	for i := 0; i < 3; i++ {
		if got := pickID(t, selector, "claude-opus-4-6", sessionOne, auths); got != "a" {
			t.Fatalf("bound session moved to %q, want it to stay on a", got)
		}
	}
	sessionTwo := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{"session-two"}}}
	if got := pickID(t, selector, "claude-opus-4-6", sessionTwo, auths); got != "b" {
		t.Fatalf("new session = %q, want b (now resets soonest)", got)
	}

	// Failover: once a is unavailable the bound session rebinds via soonest-reset.
	if got := pickID(t, selector, "claude-opus-4-6", sessionOne, auths[1:]); got != "b" {
		t.Fatalf("failover = %q, want b", got)
	}
}

func TestSoonestResetThroughManagerLegacyPickPath(t *testing.T) {
	ctx := context.Background()
	selector := NewSoonestResetSelector(DefaultSoonestResetOptions())
	if isBuiltInSelector(selector) {
		t.Fatal("soonest-reset must stay on the legacy (custom selector) pick path")
	}
	manager := NewManager(nil, selector, nil)
	manager.RegisterExecutor(schedulerProviderTestExecutor{provider: "claude"})
	now := time.Now()
	for _, auth := range []*Auth{
		{ID: "late", Provider: "claude", Quota: QuotaState{Windows: []QuotaWindow{{Key: "seven_day", Kind: "7d", UsedPercent: 10, ResetsAt: now.Add(96 * time.Hour), ObservedAt: now}}}},
		{ID: "soon", Provider: "claude", Quota: QuotaState{Windows: []QuotaWindow{{Key: "seven_day", Kind: "7d", UsedPercent: 10, ResetsAt: now.Add(12 * time.Hour), ObservedAt: now}}}},
	} {
		if _, errRegister := manager.Register(ctx, auth); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, errRegister)
		}
	}
	picked, _, errPick := manager.pickNext(ctx, "claude", "", cliproxyexecutor.Options{}, nil)
	if errPick != nil || picked == nil || picked.ID != "soon" {
		t.Fatalf("pickNext() = %v, %v; want soon", picked, errPick)
	}
	// A cooled-down credential is filtered by the manager before the selector runs.
	if _, errUpdate := manager.Update(ctx, &Auth{ID: "soon", Provider: "claude", Unavailable: true, NextRetryAfter: now.Add(time.Hour), Status: StatusError,
		Quota: QuotaState{Windows: []QuotaWindow{{Key: "seven_day", Kind: "7d", UsedPercent: 10, ResetsAt: now.Add(12 * time.Hour), ObservedAt: now}}}}); errUpdate != nil {
		t.Fatalf("Update() error = %v", errUpdate)
	}
	picked, _, errPick = manager.pickNext(ctx, "claude", "", cliproxyexecutor.Options{}, nil)
	if errPick != nil || picked == nil || picked.ID != "late" {
		t.Fatalf("pickNext() after cooldown = %v, %v; want late", picked, errPick)
	}
}
