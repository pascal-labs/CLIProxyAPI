package auth

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Hub sidecar files are JSON objects or arrays that live next to credentials;
// an object without "type" would otherwise load as an "unknown" credential.
func TestFileTokenStoreListSkipsHubSidecarFiles(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"t3-threads.json":       `{"generated_at":"2026-01-01T00:00:00Z","threads":{}}`,
		"alert-state.json":      `{}`,
		"usage-stats.json":      `[]`,
		"claude-user-a.json":    `{"type":"claude","email":"user-a@example.com","access_token":"x"}`,
		"hub-events.json":       `[]`,
		"quota-windows.json":    `[]`,
		"session-affinity.json": `[]`,
	}
	for name, content := range files {
		if errWrite := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); errWrite != nil {
			t.Fatal(errWrite)
		}
	}
	store := NewFileTokenStore()
	store.SetBaseDir(dir)
	auths, errList := store.List(context.Background())
	if errList != nil {
		t.Fatalf("List: %v", errList)
	}
	if len(auths) != 1 || auths[0].Provider != "claude" {
		names := make([]string, 0, len(auths))
		for _, auth := range auths {
			names = append(names, auth.ID+"/"+auth.Provider)
		}
		t.Fatalf("List() = %v, want only the claude credential", names)
	}
}
