package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestSoonestResetConfigDefaultsAndParsing(t *testing.T) {
	var cfg Config
	if errUnmarshal := yaml.Unmarshal([]byte(`
routing:
  strategy: soonest-reset
  soonest-reset:
    min-remaining-percent: 0
    max-5h-percent: 90.5
    poll-interval: 15m
`), &cfg); errUnmarshal != nil {
		t.Fatalf("unmarshal: %v", errUnmarshal)
	}
	soonest := cfg.Routing.SoonestReset
	if soonest.EffectiveMinRemainingPercent() != 0 || soonest.EffectiveMax5hPercent() != 90.5 || soonest.EffectivePollInterval() != 15*time.Minute {
		t.Fatalf("parsed soonest-reset config = %+v", soonest)
	}

	var defaults SoonestResetConfig
	if defaults.EffectiveMinRemainingPercent() != 3 || defaults.EffectiveMax5hPercent() != 98 || defaults.EffectivePollInterval() != 10*time.Minute {
		t.Fatal("soonest-reset defaults are wrong")
	}
	for raw, want := range map[string]time.Duration{"0": 0, "0s": 0, "-5m": 0, "bogus": 10 * time.Minute, "10s": time.Minute} {
		if got := (SoonestResetConfig{PollInterval: raw}).EffectivePollInterval(); got != want {
			t.Fatalf("poll-interval %q = %s, want %s", raw, got, want)
		}
	}
}
