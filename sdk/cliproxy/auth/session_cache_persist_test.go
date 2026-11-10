package auth

import (
	"context"
	"testing"
	"time"
)

func TestSessionCacheSnapshotRestore(t *testing.T) {
	source := NewSessionCache(time.Hour)
	t.Cleanup(source.Stop)
	source.setSessionAliases("auth-a", "claude", "claude:s1", "mixed::claude:s1::opus", "mixed::claude:s1::fable")
	source.setSessionAliases("auth-b", "codex", "codex:s2", "mixed::codex:s2::gpt")

	records := source.Snapshot()
	if len(records) != 2 {
		t.Fatalf("snapshot = %#v, want 2 bindings", records)
	}
	expired := SessionBindingRecord{AuthID: "auth-c", ExpiresAt: time.Now().Add(-time.Minute), Aliases: []string{"mixed::claude:old::opus"}}
	records = append(records, expired)

	target := NewSessionCache(time.Hour)
	t.Cleanup(target.Stop)
	// A binding made after restart must win over the persisted one.
	target.SetAliases("auth-live", "mixed::codex:s2::gpt")
	if restored := target.Restore(records); restored != 1 {
		t.Fatalf("restored = %d, want 1 (one live conflict, one expired)", restored)
	}
	for _, key := range []string{"mixed::claude:s1::opus", "mixed::claude:s1::fable"} {
		if authID, ok := target.Get(key); !ok || authID != "auth-a" {
			t.Fatalf("Get(%s) = %q, %v; want auth-a", key, authID, ok)
		}
	}
	if authID, _ := target.Get("mixed::codex:s2::gpt"); authID != "auth-live" {
		t.Fatalf("live binding replaced by persisted one: %q", authID)
	}
	if _, ok := target.Get("mixed::claude:old::opus"); ok {
		t.Fatal("expired binding restored")
	}
	if got := target.latestSessionAuth("claude:s1", map[string]struct{}{"claude": {}}); got != "auth-a" {
		t.Fatalf("restored binding lost its selection order: %q", got)
	}
	// New selections must sequence after restored ones.
	target.setSessionAliases("auth-z", "claude", "claude:s1", "mixed::claude:s1::haiku")
	if got := target.latestSessionAuth("claude:s1", map[string]struct{}{"claude": {}}); got != "auth-z" {
		t.Fatalf("new selection did not supersede restored one: %q", got)
	}
}

func TestSessionCacheRestoreCapsExpiryAtTTL(t *testing.T) {
	cache := NewSessionCache(time.Minute)
	t.Cleanup(cache.Stop)
	cache.Restore([]SessionBindingRecord{{AuthID: "a", ExpiresAt: time.Now().Add(24 * time.Hour), Aliases: []string{"k"}}})
	records := cache.Snapshot()
	if len(records) != 1 || records[0].ExpiresAt.After(time.Now().Add(time.Minute+time.Second)) {
		t.Fatalf("expiry not capped at TTL: %#v", records)
	}
}

func TestManagerQuotaWindowSnapshotIsACopy(t *testing.T) {
	m := NewManager(nil, nil, nil)
	if _, errRegister := m.Register(context.Background(), &Auth{ID: "a", Provider: "claude"}); errRegister != nil {
		t.Fatal(errRegister)
	}
	now := time.Now().UTC()
	m.ObserveQuotaWindows("a", []QuotaWindow{{Key: QuotaWindowKeyWeekly, Kind: QuotaWindowKindWeekly, UsedPercent: 40, ResetsAt: now.Add(time.Hour), ObservedAt: now, Source: QuotaWindowSourcePoll}})
	snapshot := m.QuotaWindowSnapshot()
	if len(snapshot["a"]) != 1 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	snapshot["a"][0].UsedPercent = 99
	if again := m.QuotaWindowSnapshot(); again["a"][0].UsedPercent != 40 {
		t.Fatal("snapshot aliases manager state")
	}
}
