package auth

import (
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestSoonestResetOrderListsSkippedCredentialsLast(t *testing.T) {
	now := soonestResetTestNow
	cooling := soonestAuth("cooling", weeklyWindow(10, time.Hour))
	cooling.Unavailable = true
	cooling.Status = StatusError
	cooling.NextRetryAfter = now.Add(time.Hour)
	lowPriority := soonestAuth("low", weeklyWindow(10, 2*time.Hour))
	lowPriority.Attributes = map[string]string{"priority": "-1"}
	disabled := soonestAuth("off", weeklyWindow(10, 3*time.Hour))
	disabled.Disabled = true
	auths := []*Auth{
		disabled,
		cooling,
		lowPriority,
		soonestAuth("late", weeklyWindow(10, 96*time.Hour)),
		soonestAuth("soon", weeklyWindow(10, 24*time.Hour)),
	}

	order := SoonestResetOrder(auths, "", DefaultSoonestResetOptions(), now)
	assertIDs(t, rankedIDs(order), "soon", "late", "low", "cooling", "off")
	wantReasons := []string{"", "", SoonestResetSkipLowerPriority, SoonestResetSkipUnavailable, SoonestResetSkipDisabled}
	for i, entry := range order {
		if entry.SkipReason != wantReasons[i] || entry.Eligible != (wantReasons[i] == "") {
			t.Fatalf("entry %d (%s) = eligible %v reason %q, want reason %q", i, entry.Auth.ID, entry.Eligible, entry.SkipReason, wantReasons[i])
		}
		if entry.Weekly == nil {
			t.Fatalf("entry %s has no weekly window", entry.Auth.ID)
		}
	}

	// The order view agrees with what the selector picks for the same pool.
	picked := pickID(t, newTestSoonestResetSelector(), "", cliproxyexecutor.Options{}, []*Auth{auths[3], auths[4]})
	if picked != order[0].Auth.ID {
		t.Fatalf("selector picked %q, order view ranks %q first", picked, order[0].Auth.ID)
	}
}
