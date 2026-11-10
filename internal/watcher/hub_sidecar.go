package watcher

import (
	"path/filepath"
	"strings"
)

// hubSidecarFiles are non-credential JSON files the hub keeps in the auth
// directory (internal/usagestats). They are ignored by auth-file events so their
// periodic rewrites do not trigger credential reloads.
var hubSidecarFiles = map[string]struct{}{
	"usage-stats.json": {},
}

func isHubSidecarFile(path string) bool {
	_, ok := hubSidecarFiles[strings.ToLower(filepath.Base(path))]
	return ok
}
