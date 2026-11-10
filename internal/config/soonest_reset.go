package config

import (
	"strings"
	"time"
)

// SoonestResetConfig configures the soonest-reset routing strategy and the quota
// poller that feeds it with Claude/Codex usage windows.
type SoonestResetConfig struct {
	// MinRemainingPercent excludes credentials whose weekly remaining allowance is
	// below this percentage (unless every candidate would be excluded). Default: 3.
	MinRemainingPercent *float64 `yaml:"min-remaining-percent,omitempty" json:"min-remaining-percent,omitempty"`
	// Max5hPercent excludes credentials whose 5-hour utilization is at or above this
	// percentage (unless every candidate would be excluded). Default: 98.
	Max5hPercent *float64 `yaml:"max-5h-percent,omitempty" json:"max-5h-percent,omitempty"`
	// PollInterval is how often Claude/Codex OAuth usage endpoints are polled
	// (with jitter). Default: "10m". "0" disables the poller.
	PollInterval string `yaml:"poll-interval,omitempty" json:"poll-interval,omitempty"`
}

// Default soonest-reset thresholds and poll interval.
const (
	DefaultSoonestResetMinRemainingPercent = 3.0
	DefaultSoonestResetMax5hPercent        = 98.0
	DefaultSoonestResetPollInterval        = 10 * time.Minute
)

// EffectiveMinRemainingPercent returns the configured weekly floor or the default.
func (c SoonestResetConfig) EffectiveMinRemainingPercent() float64 {
	if c.MinRemainingPercent != nil {
		return *c.MinRemainingPercent
	}
	return DefaultSoonestResetMinRemainingPercent
}

// EffectiveMax5hPercent returns the configured 5-hour ceiling or the default.
func (c SoonestResetConfig) EffectiveMax5hPercent() float64 {
	if c.Max5hPercent != nil {
		return *c.Max5hPercent
	}
	return DefaultSoonestResetMax5hPercent
}

// EffectivePollInterval parses PollInterval. Empty or invalid values use the
// default; "0" (or any non-positive duration) disables polling.
func (c SoonestResetConfig) EffectivePollInterval() time.Duration {
	raw := strings.TrimSpace(c.PollInterval)
	if raw == "" {
		return DefaultSoonestResetPollInterval
	}
	if raw == "0" {
		return 0
	}
	parsed, errParse := time.ParseDuration(raw)
	if errParse != nil {
		return DefaultSoonestResetPollInterval
	}
	if parsed <= 0 {
		return 0
	}
	if parsed < time.Minute {
		parsed = time.Minute
	}
	return parsed
}
