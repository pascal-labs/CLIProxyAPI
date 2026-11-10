package watcher

import (
	"path/filepath"
	"testing"
)

func TestIsHubSidecarFile(t *testing.T) {
	for _, name := range []string{
		"usage-stats.json", "usage-sessions.json", "session-affinity.json", "quota-windows.json",
		"quota-history.json", "hub-events.json", "t3-threads.json", "alert-state.json", "Usage-Stats.JSON",
	} {
		if !isHubSidecarFile(filepath.Join("auths", name)) {
			t.Fatalf("%s not recognized as a sidecar file", name)
		}
	}
	if isHubSidecarFile(filepath.Join("auths", "claude-user@example.com.json")) {
		t.Fatal("credential file treated as a sidecar file")
	}
}
