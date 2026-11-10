package cliproxy

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/hubsidecar"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func newHubStateTestManager(t *testing.T) *coreauth.Manager {
	t.Helper()
	state := normalizedRoutingRuntimeState(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{Strategy: "soonest-reset", SessionAffinity: true},
	})
	selector := newRoutingSelector(state)
	manager := coreauth.NewManager(nil, selector, nil)
	t.Cleanup(func() {
		if affinity, ok := manager.Selector().(*coreauth.SessionAffinitySelector); ok {
			affinity.Stop()
		}
	})
	for _, id := range []string{"claude-a.json", "claude-b.json"} {
		if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{ID: id, FileName: id, Provider: "claude", Status: coreauth.StatusActive}); errRegister != nil {
			t.Fatal(errRegister)
		}
	}
	return manager
}

func pickForSession(t *testing.T, manager *coreauth.Manager, session string) string {
	t.Helper()
	affinity := manager.Selector().(*coreauth.SessionAffinitySelector)
	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{session}}}
	picked, errPick := affinity.Pick(context.Background(), "claude", "claude-opus-5-5", opts, manager.List())
	if errPick != nil || picked == nil {
		t.Fatalf("Pick(%s) = %v, %v", session, picked, errPick)
	}
	return picked.ID
}

func weeklyWindow(resetIn time.Duration) []coreauth.QuotaWindow {
	now := time.Now().UTC()
	return []coreauth.QuotaWindow{{Key: coreauth.QuotaWindowKeyWeekly, Kind: coreauth.QuotaWindowKindWeekly, UsedPercent: 20, ResetsAt: now.Add(resetIn), ObservedAt: now, Source: coreauth.QuotaWindowSourcePoll}}
}

func TestHubStateSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	before := newHubStateTestManager(t)
	before.ObserveQuotaWindows("claude-a.json", weeklyWindow(72*time.Hour))
	// Only a has data, so the session binds to a.
	if got := pickForSession(t, before, "long-session"); got != "claude-a.json" {
		t.Fatalf("initial binding = %s, want claude-a.json", got)
	}
	// b now resets sooner: a new session (or a lost binding) would go to b.
	before.ObserveQuotaWindows("claude-b.json", weeklyWindow(12*time.Hour))
	if errSave := newHubStatePersister(dir, before).Save(); errSave != nil {
		t.Fatalf("save: %v", errSave)
	}
	for _, name := range []string{hubsidecar.SessionAffinityFile, hubsidecar.QuotaWindowsFile} {
		data, errRead := os.ReadFile(filepath.Join(dir, name))
		if errRead != nil || !strings.HasPrefix(string(data), "[") {
			t.Fatalf("%s = %q, %v; want a JSON array", name, data, errRead)
		}
		if !hubsidecar.IsFile(name) {
			t.Fatalf("%s is not ignored by credential loaders", name)
		}
	}

	after := newHubStateTestManager(t)
	persister := newHubStatePersister(dir, after)
	persister.Load()
	if got := len(after.QuotaWindowSnapshot()); got != 2 {
		t.Fatalf("restored windows for %d credentials, want 2", got)
	}
	if got := pickForSession(t, after, "long-session"); got != "claude-a.json" {
		t.Fatalf("binding after restart = %s, want claude-a.json", got)
	}
	// Restored windows let soonest-reset rank immediately instead of rotating.
	if got := pickForSession(t, after, "new-session"); got != "claude-b.json" {
		t.Fatalf("new session after restart = %s, want claude-b.json (soonest reset)", got)
	}

	// A routing reload replaces the selector; the next save carries bindings over.
	state := normalizedRoutingRuntimeState(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{Strategy: "soonest-reset", SessionAffinity: true, SessionAffinityTTL: "2h"},
	})
	after.SetSelector(newRoutingSelector(state))
	if errSave := persister.Save(); errSave != nil {
		t.Fatalf("save after reload: %v", errSave)
	}
	if got := pickForSession(t, after, "long-session"); got != "claude-a.json" {
		t.Fatalf("binding after selector reload = %s, want claude-a.json", got)
	}
}

func TestHubStateLoadSkipsUnknownCredentialsAndBadFiles(t *testing.T) {
	dir := t.TempDir()
	if errWrite := os.WriteFile(filepath.Join(dir, hubsidecar.QuotaWindowsFile), []byte("{not json"), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	expired := []coreauth.SessionBindingRecord{
		{AuthID: "claude-gone.json", ExpiresAt: time.Now().Add(time.Hour), Aliases: []string{"mixed::header:x::claude-opus-5-5"}},
		{AuthID: "claude-a.json", ExpiresAt: time.Now().Add(-time.Hour), Aliases: []string{"mixed::header:y::claude-opus-5-5"}},
	}
	if errWrite := hubsidecar.WriteJSONAtomic(filepath.Join(dir, hubsidecar.SessionAffinityFile), expired); errWrite != nil {
		t.Fatal(errWrite)
	}
	manager := newHubStateTestManager(t)
	newHubStatePersister(dir, manager).Load()
	if bindings := manager.Selector().(*coreauth.SessionAffinitySelector).SessionBindings(); len(bindings) != 0 {
		t.Fatalf("restored bindings = %#v, want none (unknown credential, expired)", bindings)
	}
}
