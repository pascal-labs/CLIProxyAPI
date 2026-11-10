package watcher

import (
	"path/filepath"
	"testing"
)

func TestIsHubSidecarFile(t *testing.T) {
	if !isHubSidecarFile(filepath.Join("auths", "usage-stats.json")) {
		t.Fatal("usage-stats.json not recognized as a sidecar file")
	}
	if isHubSidecarFile(filepath.Join("auths", "claude-user@example.com.json")) {
		t.Fatal("credential file treated as a sidecar file")
	}
}
