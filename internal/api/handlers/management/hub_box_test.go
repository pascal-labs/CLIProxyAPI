package management

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func boxHandler(t *testing.T, now time.Time) (*Handler, string) {
	t.Helper()
	authDir := t.TempDir()
	original := hubNow
	hubNow = func() time.Time { return now }
	t.Cleanup(func() { hubNow = original })
	return NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil), authDir
}

func TestBoxHealthMissingExport(t *testing.T) {
	h, _ := boxHandler(t, time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC))
	body := serveHubJSON(t, h.GetBoxHealth, "/v8/management/box/health")
	if body["available"] != false || body["reason"] != "no export yet" {
		t.Fatalf("missing export: %#v", body)
	}
}

func TestBoxHealthServesExportWithAge(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	h, dir := boxHandler(t, now)
	doc := `{"schema":1,"generated_at":"2026-11-11T11:59:30Z","cpu":{"cores":16,"util_pct":12.5},"units":[{"name":"x.service","state":"failed"}]}`
	if err := os.WriteFile(filepath.Join(dir, "box-health.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	body := serveHubJSON(t, h.GetBoxHealth, "/v8/management/box/health")
	if body["available"] != true || body["age_seconds"] != float64(30) || body["stale"] != false {
		t.Fatalf("fresh export: %#v", body)
	}
	box, ok := body["box"].(map[string]any)
	if !ok || box["cpu"].(map[string]any)["util_pct"] != 12.5 {
		t.Fatalf("box document not passed through: %#v", body["box"])
	}

	hubNow = func() time.Time { return now.Add(10 * time.Minute) }
	if body = serveHubJSON(t, h.GetBoxHealth, "/v8/management/box/health"); body["stale"] != true {
		t.Fatalf("old export should be stale: %#v", body)
	}
}

func TestBoxHealthRejectsBadExport(t *testing.T) {
	h, dir := boxHandler(t, time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC))
	if err := os.WriteFile(filepath.Join(dir, "box-health.json"), []byte(`{"generated_at":`), 0o600); err != nil {
		t.Fatal(err)
	}
	body := serveHubJSON(t, h.GetBoxHealth, "/v8/management/box/health")
	if body["available"] != false || body["reason"] != "export unreadable" {
		t.Fatalf("bad export: %#v", body)
	}
}
