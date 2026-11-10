package hubevents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type manualClock struct{ now time.Time }

func (c *manualClock) Now() time.Time { return c.now }

func noSchedule(time.Duration, func()) func() bool { return func() bool { return true } }

func TestStoreQueryRetentionAndCap(t *testing.T) {
	base := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	clock := &manualClock{now: base}
	store := New(Options{Now: clock.Now, Schedule: noSchedule})
	store.Record(Event{T: base.Add(-31 * 24 * time.Hour), Kind: KindRateLimited})
	store.Record(Event{T: base.Add(-2 * time.Hour), Kind: KindNetworkError, Detail: strings.Repeat("x", 500)})
	store.Record(Event{Kind: KindHubStart})
	events := store.Query(base.Add(-24 * time.Hour))
	if len(events) != 2 || events[0].Kind != KindHubStart || events[1].Kind != KindNetworkError {
		t.Fatalf("events = %+v", events)
	}
	if len(events[1].Detail) > maxDetailLength+3 {
		t.Fatalf("detail not truncated: %d", len(events[1].Detail))
	}
	for i := 0; i < MaxEvents+MaxEvents/5; i++ {
		store.Record(Event{T: base, Kind: KindFailover})
	}
	if got := len(store.Query(time.Time{})); got != MaxEvents {
		t.Fatalf("buffer holds %d events, want %d", got, MaxEvents)
	}
}

func TestStorePersists(t *testing.T) {
	dir := t.TempDir()
	clock := &manualClock{now: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
	store := New(Options{Now: clock.Now, Schedule: noSchedule})
	if errStart := store.Start(dir); errStart != nil {
		t.Fatal(errStart)
	}
	store.Record(Event{Kind: KindCooldown, AuthID: "claude-a.json", AuthIndex: "idx", StatusCode: 429, Detail: "until x"})
	if errStop := store.Stop(); errStop != nil {
		t.Fatal(errStop)
	}
	data, errRead := os.ReadFile(filepath.Join(dir, FileName))
	if errRead != nil || !strings.HasPrefix(string(data), "[") {
		t.Fatalf("file = %q, %v", data, errRead)
	}
	reloaded := New(Options{Now: clock.Now, Schedule: noSchedule})
	if errStart := reloaded.Start(dir); errStart != nil {
		t.Fatal(errStart)
	}
	events := reloaded.Query(time.Time{})
	if len(events) != 1 || events[0].StatusCode != 429 || events[0].AuthIndex != "idx" {
		t.Fatalf("reloaded = %+v", events)
	}
}
