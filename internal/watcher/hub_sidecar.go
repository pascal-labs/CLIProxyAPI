package watcher

import "github.com/router-for-me/CLIProxyAPI/v8/internal/hubsidecar"

// isHubSidecarFile reports non-credential JSON files the hub keeps in the auth
// directory (see internal/hubsidecar). They are ignored by auth-file events so
// their periodic rewrites do not trigger credential reloads.
func isHubSidecarFile(path string) bool {
	return hubsidecar.IsFile(path)
}
