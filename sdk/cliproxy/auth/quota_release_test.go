package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

// A 7d credential-quota lock (the 429's resets_at) must lift once a fresh poll shows
// usage is back (early reset), and stay while any window is still at 100%.
func TestReleaseRecoveredQuota(t *testing.T) {
	const model = "gpt-test-codex"
	setup := func(t *testing.T) (*Manager, string) {
		manager := NewManager(nil, nil, nil)
		auth := &Auth{ID: uuid.NewString() + "-codex-release", Provider: "codex", Attributes: map[string]string{"api_key": "k"}}
		reg := registry.GetGlobalRegistry()
		reg.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register: %v", err)
		}
		week := 7 * 24 * time.Hour
		manager.MarkResult(context.Background(), Result{
			AuthID: auth.ID, Provider: "codex", Model: model, Success: false,
			RetryAfter: &week, CredentialScope: true,
			Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "usage_limit_reached"},
		})
		got, _ := manager.GetByID(auth.ID)
		if blocked, _, _ := isAuthBlockedForModel(got, model, time.Now()); !blocked {
			t.Fatal("setup: auth should be blocked after the 429")
		}
		return manager, auth.ID
	}
	now := time.Now()
	win := func(key string, used float64) QuotaWindow {
		return QuotaWindow{Key: key, Kind: QuotaWindowKindWeekly, UsedPercent: used, ResetsAt: now.Add(7 * 24 * time.Hour), ObservedAt: now, Source: QuotaWindowSourcePoll}
	}

	t.Run("still at 100% keeps the lock", func(t *testing.T) {
		manager, id := setup(t)
		if manager.ReleaseRecoveredQuota(context.Background(), id, []QuotaWindow{win("seven_day", 100), win("seven_day_fable", 0)}) {
			t.Fatal("lifted a lock while a window is at 100%")
		}
		got, _ := manager.GetByID(id)
		if blocked, _, _ := isAuthBlockedForModel(got, model, time.Now()); !blocked {
			t.Fatal("auth unblocked while a window is at 100%")
		}
	})

	t.Run("usage back lifts the lock", func(t *testing.T) {
		manager, id := setup(t)
		if !manager.ReleaseRecoveredQuota(context.Background(), id, []QuotaWindow{win("seven_day", 0)}) {
			t.Fatal("did not lift the lock after usage came back")
		}
		got, _ := manager.GetByID(id)
		if blocked, _, _ := isAuthBlockedForModel(got, model, time.Now()); blocked {
			t.Fatal("auth still blocked after release")
		}
		if manager.ReleaseRecoveredQuota(context.Background(), id, []QuotaWindow{win("seven_day", 0)}) {
			t.Fatal("reported a release with no lock in place")
		}
	})

	t.Run("no windows or unknown auth does nothing", func(t *testing.T) {
		manager, id := setup(t)
		if manager.ReleaseRecoveredQuota(context.Background(), id, nil) || manager.ReleaseRecoveredQuota(context.Background(), "nope", []QuotaWindow{win("seven_day", 0)}) {
			t.Fatal("released without windows or for an unknown auth")
		}
	})
}
