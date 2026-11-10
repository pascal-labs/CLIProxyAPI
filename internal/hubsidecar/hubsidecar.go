// Package hubsidecar names the non-credential JSON files the hub keeps in (or
// reads from) the auth directory, and writes them atomically. Credential
// loaders and the auth-directory watcher skip these files so their periodic
// rewrites neither trigger credential reloads nor surface as bogus
// "unknown" credentials.
package hubsidecar

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	log "github.com/sirupsen/logrus"
)

// Sidecar file names.
const (
	UsageStatsFile      = "usage-stats.json"
	UsageSessionsFile   = "usage-sessions.json"
	SessionAffinityFile = "session-affinity.json"
	QuotaWindowsFile    = "quota-windows.json"
	QuotaHistoryFile    = "quota-history.json"
	HubEventsFile       = "hub-events.json"
	// T3ThreadsFile is written by an external exporter; the hub only reads it.
	T3ThreadsFile = "t3-threads.json"
	// AlertStateFile is written by companion alerting tooling.
	AlertStateFile = "alert-state.json"
)

var names = map[string]struct{}{
	UsageStatsFile:      {},
	UsageSessionsFile:   {},
	SessionAffinityFile: {},
	QuotaWindowsFile:    {},
	QuotaHistoryFile:    {},
	HubEventsFile:       {},
	T3ThreadsFile:       {},
	AlertStateFile:      {},
}

// IsFile reports whether path (or a bare file name) is a hub sidecar file.
func IsFile(path string) bool {
	_, ok := names[strings.ToLower(filepath.Base(strings.TrimSpace(path)))]
	return ok
}

// WriteJSONAtomic encodes value and replaces path with it through a temp file
// in the same directory (write, fsync, rename), so readers never observe a
// partial file.
func WriteJSONAtomic(path string, value any) error {
	data, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return fmt.Errorf("%s: encode: %w", filepath.Base(path), errMarshal)
	}
	return WriteFileAtomic(path, data)
}

// WriteFileAtomic replaces path with data through a temp file in the same directory.
func WriteFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	if errMkdir := os.MkdirAll(dir, 0o700); errMkdir != nil {
		return fmt.Errorf("%s: create directory: %w", base, errMkdir)
	}
	temp, errTemp := os.CreateTemp(dir, "."+strings.TrimSuffix(base, filepath.Ext(base))+"-*.tmp")
	if errTemp != nil {
		return fmt.Errorf("%s: create temp file: %w", base, errTemp)
	}
	tempPath := temp.Name()
	cleanup := func() {
		if errRemove := os.Remove(tempPath); errRemove != nil && !errors.Is(errRemove, os.ErrNotExist) {
			log.WithError(errRemove).Debugf("%s: remove temp file", base)
		}
	}
	if _, errWrite := temp.Write(data); errWrite != nil {
		_ = temp.Close()
		cleanup()
		return fmt.Errorf("%s: write temp file: %w", base, errWrite)
	}
	if errSync := temp.Sync(); errSync != nil {
		_ = temp.Close()
		cleanup()
		return fmt.Errorf("%s: sync temp file: %w", base, errSync)
	}
	if errClose := temp.Close(); errClose != nil {
		cleanup()
		return fmt.Errorf("%s: close temp file: %w", base, errClose)
	}
	if errRename := os.Rename(tempPath, path); errRename != nil {
		cleanup()
		return fmt.Errorf("%s: replace: %w", base, errRename)
	}
	return nil
}

// ReadJSON decodes path into value. A missing file is not an error and
// reports found=false.
func ReadJSON(path string, value any) (found bool, err error) {
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		if errors.Is(errRead, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("%s: read: %w", filepath.Base(path), errRead)
	}
	if errUnmarshal := json.Unmarshal(data, value); errUnmarshal != nil {
		return true, fmt.Errorf("%s: decode: %w", filepath.Base(path), errUnmarshal)
	}
	return true, nil
}
