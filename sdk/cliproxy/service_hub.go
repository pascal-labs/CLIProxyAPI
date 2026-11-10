package cliproxy

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotapoller"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// hubRuntime owns the hub patch-set background workers. It lives in its own
// file so upstream service files only carry one-line start/stop hooks.
type hubRuntime struct {
	mu     sync.Mutex
	poller *quotapoller.Poller
}

// startHubRuntime starts the Claude/Codex quota poller. It runs only in local
// (non-Home) mode, after the auth manager has loaded credentials.
func (s *Service) startHubRuntime(ctx context.Context) {
	if s == nil || s.coreManager == nil {
		return
	}
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
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
}

// stopHubRuntime stops the hub workers. It is safe to call more than once.
func (s *Service) stopHubRuntime() {
	if s == nil {
		return
	}
	s.hub.mu.Lock()
	poller := s.hub.poller
	s.hub.poller = nil
	s.hub.mu.Unlock()
	if poller != nil {
		poller.Stop()
	}
}

func (s *Service) quotaPollInterval() time.Duration {
	cfg := s.currentConfig()
	if cfg == nil {
		return internalconfig.DefaultSoonestResetPollInterval
	}
	return cfg.Routing.SoonestReset.EffectivePollInterval()
}
