package hubsidecar

import "testing"

func TestIsFile(t *testing.T) {
	for name, want := range map[string]bool{
		"usage-stats.json":            true,
		"/x/y/Box-Health.json":        true,
		"t3-threads.json":             true,
		"t3-threads.laptop.json":      true,
		"t3-threads..json":            false,
		"t3-threads.json.tmp":         false,
		"claude-abc-user.json":        false,
		"box-health.json.bak":         false,
		"codex-1234-user-promax.json": false,
	} {
		if got := IsFile(name); got != want {
			t.Errorf("IsFile(%q) = %v, want %v", name, got, want)
		}
	}
}
