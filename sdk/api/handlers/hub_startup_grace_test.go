package handlers

import (
	"testing"
	"time"
)

// fakeStartupClock drives waitForStartupProviders without wall-clock sleeps.
func fakeStartupClock(t *testing.T, sinceStart, window, maxWait time.Duration) *time.Time {
	t.Helper()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	prev := struct {
		start        time.Time
		window, wait time.Duration
		poll         time.Duration
		nowFn        func() time.Time
		sleepFn      func(time.Duration)
	}{hubProcessStart, hubStartupGraceWindow, hubStartupMaxWait, hubStartupPoll, hubNow, hubSleep}
	hubProcessStart = now.Add(-sinceStart)
	hubStartupGraceWindow, hubStartupMaxWait, hubStartupPoll = window, maxWait, 100*time.Millisecond
	hubNow = func() time.Time { return now }
	hubSleep = func(d time.Duration) { now = now.Add(d) }
	t.Cleanup(func() {
		hubProcessStart, hubStartupGraceWindow, hubStartupMaxWait = prev.start, prev.window, prev.wait
		hubStartupPoll, hubNow, hubSleep = prev.poll, prev.nowFn, prev.sleepFn
	})
	return &now
}

func TestWaitForStartupProvidersWaitsForRegistryDuringStartup(t *testing.T) {
	fakeStartupClock(t, time.Second, 90*time.Second, 15*time.Second)
	calls := 0
	got := waitForStartupProviders(func() []string {
		calls++
		if calls < 5 {
			return nil
		}
		return []string{"codex"}
	})
	if len(got) != 1 || got[0] != "codex" || calls != 5 {
		t.Fatalf("providers = %v after %d lookups, want [codex] after 5", got, calls)
	}
}

func TestWaitForStartupProvidersReturnsAtOnceAfterStartup(t *testing.T) {
	fakeStartupClock(t, time.Hour, 90*time.Second, 15*time.Second)
	calls := 0
	if got := waitForStartupProviders(func() []string { calls++; return []string{"codex"} }); got != nil || calls != 0 {
		t.Fatalf("providers = %v with %d lookups after the startup window, want nil and no lookups", got, calls)
	}
}

func TestWaitForStartupProvidersGivesUpForUnknownModel(t *testing.T) {
	now := fakeStartupClock(t, time.Second, 90*time.Second, 15*time.Second)
	begin := *now
	if got := waitForStartupProviders(func() []string { return nil }); got != nil {
		t.Fatalf("providers = %v, want nil", got)
	}
	if waited := now.Sub(begin); waited < 15*time.Second || waited > 15*time.Second+100*time.Millisecond {
		t.Fatalf("waited %s, want the 15s cap", waited)
	}
}

func TestWaitForStartupProvidersStopsAtEndOfStartupWindow(t *testing.T) {
	now := fakeStartupClock(t, 85*time.Second, 90*time.Second, 15*time.Second)
	begin := *now
	waitForStartupProviders(func() []string { return nil })
	if waited := now.Sub(begin); waited > 5*time.Second+100*time.Millisecond {
		t.Fatalf("waited %s, want at most the 5s left in the startup window", waited)
	}
}
