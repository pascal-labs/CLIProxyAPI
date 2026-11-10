package quotahistory

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type manualClock struct{ now time.Time }

func (c *manualClock) Now() time.Time { return c.now }

func weekly(used float64, reset, observed time.Time) coreauth.QuotaWindow {
	return coreauth.QuotaWindow{Key: "seven_day", Kind: coreauth.QuotaWindowKindWeekly, UsedPercent: used, ResetsAt: reset, ObservedAt: observed, Source: "poll"}
}

func TestRecordSpacingKeepsNewestObservation(t *testing.T) {
	base := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	clock := &manualClock{now: base.Add(time.Hour)}
	store := New(Options{Now: clock.Now})
	reset := base.Add(72 * time.Hour)
	for i, used := range []float64{10, 11, 12, 13, 14, 15, 16} {
		store.Record("a", "idx", "claude", []coreauth.QuotaWindow{weekly(used, reset, base.Add(time.Duration(i)*time.Minute))})
	}
	// A repeated or older observation is ignored.
	store.Record("a", "idx", "claude", []coreauth.QuotaWindow{weekly(99, reset, base.Add(2*time.Minute))})
	samples := store.Samples("a", "seven_day", base.Add(-time.Hour))
	if len(samples) != 3 {
		t.Fatalf("samples = %+v, want 3 (t=0, t=5m, newest)", samples)
	}
	if samples[1].T != base.Add(5*time.Minute) || samples[2].UsedPercent != 16 {
		t.Fatalf("samples = %+v", samples)
	}
}

func TestCyclesAndBurnRate(t *testing.T) {
	base := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	firstReset := base.Add(5 * time.Hour)
	secondReset := base.Add(10 * time.Hour)
	samples := []Sample{
		{T: base, UsedPercent: 0, ResetsAt: firstReset},
		{T: base.Add(2 * time.Hour), UsedPercent: 40, ResetsAt: firstReset},
		{T: base.Add(4 * time.Hour), UsedPercent: 72, ResetsAt: firstReset},
		{T: base.Add(6 * time.Hour), UsedPercent: 10, ResetsAt: secondReset},
		{T: base.Add(7 * time.Hour), UsedPercent: 25, ResetsAt: secondReset},
		{T: base.Add(8 * time.Hour), UsedPercent: 40, ResetsAt: secondReset},
	}
	now := base.Add(8 * time.Hour)
	cycles := Cycles(samples, now)
	if len(cycles) != 1 || !cycles[0].ResetAt.Equal(firstReset) || cycles[0].FinalUsedPercent != 72 {
		t.Fatalf("cycles = %+v", cycles)
	}
	// Only the current cycle counts: 10 -> 25 -> 40 over two hours = 15 %/h.
	rate, ok := BurnRate(samples, secondReset, now, 6*time.Hour)
	if !ok || math.Abs(rate-15) > 1e-9 {
		t.Fatalf("burn rate = %v, %v; want 15", rate, ok)
	}
	if _, ok := BurnRate(samples[:4], secondReset, now, 6*time.Hour); ok {
		t.Fatal("burn rate from a single current-cycle sample")
	}
	// A trailing cycle whose reset passed counts as finished.
	later := Cycles(samples, secondReset.Add(time.Minute))
	if len(later) != 2 || later[1].FinalUsedPercent != 40 {
		t.Fatalf("cycles after second reset = %+v", later)
	}
}

func TestDownsampleKeepsPeaks(t *testing.T) {
	base := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	var samples []Sample
	for i := 0; i < 1000; i++ {
		samples = append(samples, Sample{T: base.Add(time.Duration(i) * time.Minute), UsedPercent: float64(i % 100)})
	}
	out := Downsample(samples, 200)
	if len(out) > 200 || len(out) < 150 {
		t.Fatalf("downsampled to %d points", len(out))
	}
	peak := 0.0
	for _, sample := range out {
		peak = math.Max(peak, sample.UsedPercent)
	}
	if peak != 99 {
		t.Fatalf("peak lost: %v", peak)
	}
	if got := Downsample(samples[:10], 200); len(got) != 10 {
		t.Fatalf("short series changed: %d", len(got))
	}
}

func TestThinKeepsCycleFinalSamples(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	reset := base.Add(30 * time.Minute)
	samples := []Sample{
		{T: base, UsedPercent: 1, ResetsAt: reset},
		{T: base.Add(10 * time.Minute), UsedPercent: 5, ResetsAt: reset},
		{T: base.Add(20 * time.Minute), UsedPercent: 9, ResetsAt: reset},
		{T: base.Add(40 * time.Minute), UsedPercent: 2, ResetsAt: reset.Add(5 * time.Hour)},
		{T: base.Add(50 * time.Minute), UsedPercent: 3, ResetsAt: reset.Add(5 * time.Hour)},
	}
	out := thin(append([]Sample(nil), samples...), base.Add(-time.Hour), base.Add(24*time.Hour))
	if len(out) != 2 || out[0].UsedPercent != 9 || out[1].UsedPercent != 3 {
		t.Fatalf("thinned = %+v, want the final sample of each cycle", out)
	}
}

func TestStorePersistsAndPrunes(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	clock := &manualClock{now: base}
	store := New(Options{Now: clock.Now, Schedule: func(time.Duration, func()) func() bool { return func() bool { return true } }})
	if errStart := store.Start(dir); errStart != nil {
		t.Fatal(errStart)
	}
	store.Record("claude-a.json", "idx", "claude", []coreauth.QuotaWindow{weekly(30, base.Add(48*time.Hour), base), {Key: "five_hour", Kind: "5h", UsedPercent: 5, ResetsAt: base.Add(time.Hour), ObservedAt: base}})
	store.Record("claude-a.json", "idx", "claude", []coreauth.QuotaWindow{weekly(1, base.Add(48*time.Hour), base.Add(-40*24*time.Hour))})
	if errFlush := store.Flush(); errFlush != nil {
		t.Fatal(errFlush)
	}
	data, errRead := os.ReadFile(filepath.Join(dir, FileName))
	if errRead != nil || !strings.HasPrefix(string(data), "[") {
		t.Fatalf("file = %q, %v", data, errRead)
	}
	reloaded := New(Options{Now: clock.Now})
	if errStart := reloaded.Start(dir); errStart != nil {
		t.Fatal(errStart)
	}
	if keys := reloaded.Keys("claude-a.json"); strings.Join(keys, ",") != "five_hour,seven_day" {
		t.Fatalf("keys = %v", keys)
	}
	samples := reloaded.Samples("claude-a.json", "seven_day", time.Time{})
	if len(samples) != 1 || samples[0].UsedPercent != 30 || !samples[0].ResetsAt.Equal(base.Add(48*time.Hour)) {
		t.Fatalf("reloaded samples = %+v", samples)
	}
	clock.now = base.Add(Retention + time.Hour)
	if errFlush := reloaded.Flush(); errFlush != nil {
		t.Fatal(errFlush)
	}
	reloaded.mu.Lock()
	reloaded.pruneLocked()
	remaining := len(reloaded.series)
	reloaded.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("samples older than retention kept: %d series", remaining)
	}
}
