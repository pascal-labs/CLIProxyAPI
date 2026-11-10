package cliproxy

import (
	"context"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestSoonestResetRoutingSelector(t *testing.T) {
	minRemaining, maxFiveHour := 5.0, 90.0
	state := normalizedRoutingRuntimeState(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			Strategy: "Soonest-Reset",
			SoonestReset: internalconfig.SoonestResetConfig{
				MinRemainingPercent: &minRemaining,
				Max5hPercent:        &maxFiveHour,
			},
		},
	})
	if state.strategy != "soonest-reset" {
		t.Fatalf("strategy = %q, want soonest-reset", state.strategy)
	}
	selector, ok := newRoutingSelector(state).(*coreauth.SoonestResetSelector)
	if !ok {
		t.Fatalf("selector type = %T, want *auth.SoonestResetSelector", newRoutingSelector(state))
	}
	if got := selector.Options(); got.MinRemainingPercent != 5 || got.Max5hPercent != 90 {
		t.Fatalf("selector options = %+v", got)
	}

	defaults := normalizedRoutingRuntimeState(&internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: "soonest-reset"}})
	if defaults.soonestReset != coreauth.DefaultSoonestResetOptions() {
		t.Fatalf("default options = %+v", defaults.soonestReset)
	}
	// Thresholds only matter for soonest-reset, so other strategies ignore them.
	other := normalizedRoutingRuntimeState(&internalconfig.Config{Routing: internalconfig.RoutingConfig{
		Strategy:     "round-robin",
		SoonestReset: internalconfig.SoonestResetConfig{MinRemainingPercent: &minRemaining},
	}})
	if other.soonestReset != (coreauth.SoonestResetOptions{}) {
		t.Fatalf("round-robin state carried soonest-reset options: %+v", other.soonestReset)
	}
}

func TestSoonestResetRoutingSelectorWithSessionAffinity(t *testing.T) {
	state := normalizedRoutingRuntimeState(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{Strategy: "soonest-reset", SessionAffinity: true},
	})
	affinity, ok := newRoutingSelector(state).(*coreauth.SessionAffinitySelector)
	if !ok {
		t.Fatalf("selector type = %T, want *auth.SessionAffinitySelector", newRoutingSelector(state))
	}
	defer affinity.Stop()

	now := time.Now()
	weekly := func(resetIn time.Duration) coreauth.QuotaState {
		return coreauth.QuotaState{Windows: []coreauth.QuotaWindow{{Key: "seven_day", Kind: "7d", UsedPercent: 10, ResetsAt: now.Add(resetIn), ObservedAt: now}}}
	}
	auths := []*coreauth.Auth{
		{ID: "a", Provider: "claude", Quota: weekly(72 * time.Hour)},
		{ID: "b", Provider: "claude", Quota: weekly(12 * time.Hour)},
	}
	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{"wired-session"}}}
	picked, errPick := affinity.Pick(context.Background(), "claude", "claude-opus-4-6", opts, auths)
	if errPick != nil || picked == nil || picked.ID != "b" {
		t.Fatalf("Pick() = %v, %v; want b from the soonest-reset fallback", picked, errPick)
	}
}
