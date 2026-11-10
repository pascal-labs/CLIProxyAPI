package cliproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/hubsidecar"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotahistory"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// hubStateSaveInterval debounces session-binding and quota-window persistence.
const hubStateSaveInterval = 30 * time.Second

// persistedQuotaWindows is one entry of quota-windows.json (a top-level array,
// like every hub sidecar, so no credential loader can mistake it for an auth).
type persistedQuotaWindows struct {
	AuthID    string                 `json:"auth_id"`
	AuthIndex string                 `json:"auth_index,omitempty"`
	Provider  string                 `json:"provider,omitempty"`
	Windows   []coreauth.QuotaWindow `json:"windows"`
}

// hubStatePersister keeps session-affinity bindings and the latest quota
// windows across restarts. Both live only in memory upstream, so a restart
// rebound every session and left soonest-reset without data until the first
// poll; this restores them before the server starts serving.
type hubStatePersister struct {
	bindingsPath string
	windowsPath  string
	manager      *coreauth.Manager
	interval     time.Duration
	// history, when set, receives every credential's windows on each save so
	// the runway view has utilization samples from polls and headers alike.
	history *quotahistory.Store

	mu           sync.Mutex
	selector     *coreauth.SessionAffinitySelector
	lastBindings []byte
	lastWindows  []byte

	lifecycleMu sync.Mutex
	cancel      context.CancelFunc
	done        chan struct{}
}

func newHubStatePersister(authDir string, manager *coreauth.Manager) *hubStatePersister {
	return &hubStatePersister{
		bindingsPath: filepath.Join(authDir, hubsidecar.SessionAffinityFile),
		windowsPath:  filepath.Join(authDir, hubsidecar.QuotaWindowsFile),
		manager:      manager,
		interval:     hubStateSaveInterval,
	}
}

func currentAffinitySelector(manager *coreauth.Manager) *coreauth.SessionAffinitySelector {
	if manager == nil {
		return nil
	}
	affinity, _ := manager.Selector().(*coreauth.SessionAffinitySelector)
	return affinity
}

// Load restores persisted quota windows (merged newest-observation-wins) and
// unexpired session bindings for credentials that are still registered.
func (p *hubStatePersister) Load() {
	if p == nil || p.manager == nil {
		return
	}
	known := make(map[string]struct{})
	for _, auth := range p.manager.List() {
		if auth != nil {
			known[auth.ID] = struct{}{}
		}
	}

	var windows []persistedQuotaWindows
	if _, errRead := hubsidecar.ReadJSON(p.windowsPath, &windows); errRead != nil {
		log.WithError(errRead).Warn("hub state: quota windows not restored")
	}
	restoredWindows := 0
	for _, entry := range windows {
		if _, ok := known[entry.AuthID]; !ok || len(entry.Windows) == 0 {
			continue
		}
		p.manager.ObserveQuotaWindows(entry.AuthID, entry.Windows)
		restoredWindows++
	}

	var bindings []coreauth.SessionBindingRecord
	if _, errRead := hubsidecar.ReadJSON(p.bindingsPath, &bindings); errRead != nil {
		log.WithError(errRead).Warn("hub state: session bindings not restored")
	}
	kept := bindings[:0]
	for _, record := range bindings {
		if _, ok := known[record.AuthID]; ok {
			kept = append(kept, record)
		}
	}
	restoredBindings := 0
	p.mu.Lock()
	p.selector = currentAffinitySelector(p.manager)
	if p.selector != nil {
		restoredBindings = p.selector.RestoreSessionBindings(kept)
	}
	p.mu.Unlock()
	log.Infof("hub state restored: quota windows for %d credential(s), %d session binding(s)", restoredWindows, restoredBindings)
}

// Start saves periodically until Stop.
func (p *hubStatePersister) Start(ctx context.Context) {
	if p == nil {
		return
	}
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	if p.cancel != nil {
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	p.cancel, p.done = cancel, done
	go func() {
		defer close(done)
		ticker := time.NewTicker(p.interval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				if errSave := p.Save(); errSave != nil {
					log.WithError(errSave).Warn("hub state: persist failed")
				}
			}
		}
	}()
}

// Stop ends the save loop and writes a final snapshot.
func (p *hubStatePersister) Stop() {
	if p == nil {
		return
	}
	p.lifecycleMu.Lock()
	cancel, done := p.cancel, p.done
	p.cancel, p.done = nil, nil
	p.lifecycleMu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	if errSave := p.Save(); errSave != nil {
		log.WithError(errSave).Warn("hub state: final persist failed")
	}
}

// Save writes bindings and windows when they changed since the last write. A
// selector replaced by a routing config reload inherits the previous
// selector's bindings first, so a reload does not scatter sessions either.
func (p *hubStatePersister) Save() error {
	if p == nil || p.manager == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	selector := currentAffinitySelector(p.manager)
	if selector != p.selector {
		if p.selector != nil && selector != nil {
			if restored := selector.RestoreSessionBindings(p.selector.SessionBindings()); restored > 0 {
				log.Infof("hub state: carried %d session binding(s) into the reloaded selector", restored)
			}
		}
		p.selector = selector
	}
	var firstErr error
	if selector != nil {
		bindings := selector.SessionBindings()
		if bindings == nil {
			bindings = []coreauth.SessionBindingRecord{}
		}
		if errWrite := writeIfChanged(p.bindingsPath, bindings, &p.lastBindings); errWrite != nil {
			firstErr = errWrite
		}
	}

	snapshot := p.manager.QuotaWindowSnapshot()
	windows := make([]persistedQuotaWindows, 0, len(snapshot))
	for authID, authWindows := range snapshot {
		entry := persistedQuotaWindows{AuthID: authID, Windows: authWindows}
		if auth, ok := p.manager.GetByID(authID); ok && auth != nil {
			entry.AuthIndex = auth.EnsureIndex()
			entry.Provider = strings.ToLower(strings.TrimSpace(auth.Provider))
		}
		if p.history != nil {
			p.history.Record(entry.AuthID, entry.AuthIndex, entry.Provider, entry.Windows)
		}
		windows = append(windows, entry)
	}
	sort.Slice(windows, func(i, j int) bool { return windows[i].AuthID < windows[j].AuthID })
	if errWrite := writeIfChanged(p.windowsPath, windows, &p.lastWindows); errWrite != nil && firstErr == nil {
		firstErr = errWrite
	}
	return firstErr
}

func writeIfChanged(path string, value any, last *[]byte) error {
	data, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return errMarshal
	}
	if bytes.Equal(data, *last) {
		return nil
	}
	if errWrite := hubsidecar.WriteFileAtomic(path, data); errWrite != nil {
		return errWrite
	}
	*last = data
	return nil
}
