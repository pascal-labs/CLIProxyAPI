package management

import (
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// quotaWindowsPayload renders structured quota windows for auth-files output as
// quota.windows. resets_at is null when unknown; stale marks a reset that
// already passed (the window describes a previous period).
func quotaWindowsPayload(windows []coreauth.QuotaWindow) []gin.H {
	if len(windows) == 0 {
		return nil
	}
	now := time.Now()
	out := make([]gin.H, 0, len(windows))
	for _, window := range windows {
		entry := gin.H{
			"key":               window.Key,
			"kind":              window.Kind,
			"used_percent":      window.UsedPercent,
			"remaining_percent": window.RemainingPercent(),
			"resets_at":         quotaWindowTime(window.ResetsAt),
			"observed_at":       quotaWindowTime(window.ObservedAt),
			"source":            window.Source,
			"stale":             !window.Fresh(now),
		}
		if window.Model != "" {
			entry["model"] = window.Model
		}
		out = append(out, entry)
	}
	return out
}

func quotaWindowTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.UTC().Format(time.RFC3339)
}
