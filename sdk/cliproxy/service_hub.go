package cliproxy

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/hubevents"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotahistory"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotapoller"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestats"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// hubRuntime owns the hub patch-set background workers. It lives in its own
// file so upstream service files only carry one-line start/stop hooks.
type hubRuntime struct {
	mu      sync.Mutex
	poller  *quotapoller.Poller
	usage   *usagestats.Aggregator
	state   *hubStatePersister
	history *quotahistory.Store
	events  *hubevents.Store
}

// startHubRuntime restores persisted session bindings and quota windows, then
// starts the event log, the quota history, the Claude/Codex quota poller and
// the usage aggregator. Everything persists to sidecar files in the auth
// directory (see internal/hubsidecar). It runs only in local (non-Home) mode,
// after the auth manager has loaded credentials and before the server starts
// serving.
func (s *Service) startHubRuntime(ctx context.Context) {
	if s == nil || s.coreManager == nil {
		return
	}
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	authDir := ""
	if cfg := s.currentConfig(); cfg != nil && cfg.AuthDir != "" {
		resolved, errResolve := util.ResolveAuthDir(cfg.AuthDir)
		if errResolve != nil {
			log.WithError(errResolve).Warn("hub state and usage stats disabled: cannot resolve auth directory")
		} else {
			authDir = resolved
		}
	}
	if s.hub.events == nil && authDir != "" {
		s.hub.events = hubevents.Default()
		if errStart := s.hub.events.Start(authDir); errStart != nil {
			log.WithError(errStart).Warn("hub events: previous events not loaded")
		}
		events := s.hub.events
		coreauth.SetHubEventSink(func(event coreauth.HubEvent) {
			events.Record(hubevents.Event{
				T: event.Time, Kind: event.Kind, AuthID: event.AuthID, AuthIndex: event.AuthIndex,
				Provider: event.Provider, Model: event.Model, SessionKey: event.SessionKey,
				StatusCode: event.StatusCode, Detail: event.Detail, RelatedAuthID: event.RelatedAuthID,
			})
		})
		events.Record(hubevents.Event{Kind: hubevents.KindHubStart, Detail: "hub started"})
	}
	if s.hub.history == nil && authDir != "" {
		s.hub.history = quotahistory.Default()
		if errStart := s.hub.history.Start(authDir); errStart != nil {
			log.WithError(errStart).Warn("quota history: previous samples not loaded")
		}
	}
	if s.hub.state == nil && authDir != "" {
		s.hub.state = newHubStatePersister(authDir, s.coreManager)
		s.hub.state.history = s.hub.history
		s.hub.state.Load()
		s.hub.state.Start(ctx)
	}
	if s.hub.poller == nil {
		s.hub.poller = quotapoller.New(quotapoller.Options{
			Manager:  s.coreManager,
			Interval: s.quotaPollInterval,
			Transport: func(auth *coreauth.Auth) http.RoundTripper {
				return management.APICallTransportForAuth(s.currentConfig(), auth)
			},
			Token: management.APICallTokenForAuth,
		})
		s.hub.poller.Start(ctx)
		log.Infof("quota poller started (interval=%s)", s.quotaPollInterval())
	}
	if s.hub.usage == nil && authDir != "" {
		s.hub.usage = usagestats.Default()
		if errStart := s.hub.usage.Start(authDir); errStart != nil {
			log.WithError(errStart).Warn("usage stats: previous data not loaded; starting empty")
		}
	}
}

// stopHubRuntime stops the poller and flushes usage statistics, session
// bindings, quota windows and history, and events. It is safe to call more
// than once.
func (s *Service) stopHubRuntime() {
	if s == nil {
		return
	}
	s.hub.mu.Lock()
	poller, usage, state, history, events := s.hub.poller, s.hub.usage, s.hub.state, s.hub.history, s.hub.events
	s.hub.poller, s.hub.usage, s.hub.state, s.hub.history, s.hub.events = nil, nil, nil, nil, nil
	s.hub.mu.Unlock()
	if poller != nil {
		poller.Stop()
	}
	if usage != nil {
		if errStop := usage.Stop(); errStop != nil {
			log.WithError(errStop).Warn("usage stats: final persist failed")
		}
	}
	if state != nil {
		state.Stop()
	}
	if history != nil {
		if errStop := history.Stop(); errStop != nil {
			log.WithError(errStop).Warn("quota history: final persist failed")
		}
	}
	if events != nil {
		coreauth.SetHubEventSink(nil)
		if errStop := events.Stop(); errStop != nil {
			log.WithError(errStop).Warn("hub events: final persist failed")
		}
	}
}

func (s *Service) quotaPollInterval() time.Duration {
	cfg := s.currentConfig()
	if cfg == nil {
		return internalconfig.DefaultSoonestResetPollInterval
	}
	return cfg.Routing.SoonestReset.EffectivePollInterval()
}
