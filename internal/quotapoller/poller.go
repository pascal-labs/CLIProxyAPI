// Package quotapoller periodically fetches Claude and Codex OAuth usage windows
// so idle credentials carry reset data for quota-aware routing. Passive response
// headers only cover credentials that recently served traffic.
package quotapoller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	codexauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// Usage endpoints, as used by the management panel (src/utils/quota/constants.ts).
const (
	DefaultClaudeUsageURL = "https://api.anthropic.com/api/oauth/usage"
	DefaultCodexUsageURL  = "https://chatgpt.com/backend-api/wham/usage"
)

const (
	defaultTick         = 15 * time.Second
	defaultInitialDelay = 10 * time.Second
	defaultJitter       = 0.1
	// requestTimeout mirrors the management api-call timeout: this poll is the
	// server-side equivalent of the panel's api-call quota request.
	requestTimeout       = 60 * time.Second
	maxResponseBytes     = 1 << 20
	minRepollAfterChange = 30 * time.Second
	maxBackoff           = 6 * time.Hour
)

// Request headers copied from the management panel (CLAUDE_REQUEST_HEADERS plus
// the Accept header data.ts adds, and CODEX_REQUEST_HEADERS). Authorization is
// "Bearer $TOKEN$" there and is filled in from the credential here.
var (
	claudeRequestHeaders = [][2]string{
		{"User-Agent", "claude-cli/2.1.280 (external, cli)"},
		{"Content-Type", "application/json"},
		{"anthropic-beta", "oauth-2025-04-20"},
		{"Accept", "application/json"},
	}
	codexRequestHeaders = [][2]string{
		{"Content-Type", "application/json"},
		{"User-Agent", "codex-tui/0.149.1 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.149.1)"},
	}
)

var errNoToken = errors.New("credential has no access token")

// AuthManager is the subset of *coreauth.Manager the poller needs.
type AuthManager interface {
	List() []*coreauth.Auth
	ObserveQuotaWindows(authID string, windows []coreauth.QuotaWindow) bool
}

// Options configures a Poller.
type Options struct {
	Manager AuthManager
	// Interval returns the current poll interval; it is re-read every tick so
	// config hot reloads apply. A non-positive value pauses polling.
	Interval func() time.Duration
	// Transport returns the round tripper for a credential (per-auth proxy etc.).
	// Defaults to a clone of http.DefaultTransport.
	Transport func(auth *coreauth.Auth) http.RoundTripper
	// Token returns the bearer token for a credential. Defaults to the
	// metadata access_token.
	Token func(auth *coreauth.Auth) string
	// Now overrides the clock in tests.
	Now func() time.Time
	// Tick is the reconcile cadence that discovers new/updated credentials.
	Tick time.Duration
	// InitialDelay postpones the first startup poll so the auth manager's
	// auto-refresh can renew expired tokens first (default 10s, negative = none).
	InitialDelay time.Duration
	// Jitter is the +/- fraction applied to the interval (default 0.1, negative disables).
	Jitter float64

	ClaudeURL string
	CodexURL  string
}

type authPollState struct {
	nextPoll          time.Time
	lastPoll          time.Time
	credentialVersion uint64
	registrationEpoch uint64
	failures          int
}

// Poller fetches usage windows for every enabled Claude/Codex OAuth credential
// at startup, shortly after a credential is added or its token changes, and
// then every interval (with jitter).
type Poller struct {
	opts Options

	mu     sync.Mutex
	states map[string]*authPollState

	lifecycleMu sync.Mutex
	cancel      context.CancelFunc
	done        chan struct{}
}

// New constructs a Poller.
func New(opts Options) *Poller {
	if opts.Tick <= 0 {
		opts.Tick = defaultTick
	}
	if opts.InitialDelay == 0 {
		opts.InitialDelay = defaultInitialDelay
	}
	if opts.Jitter == 0 {
		opts.Jitter = defaultJitter
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.ClaudeURL == "" {
		opts.ClaudeURL = DefaultClaudeUsageURL
	}
	if opts.CodexURL == "" {
		opts.CodexURL = DefaultCodexUsageURL
	}
	return &Poller{opts: opts, states: make(map[string]*authPollState)}
}

// Start launches the background loop. Calling Start twice is a no-op.
func (p *Poller) Start(ctx context.Context) {
	if p == nil || p.opts.Manager == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	if p.cancel != nil {
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	p.done = make(chan struct{})
	go p.run(runCtx, p.done)
}

// Stop cancels the loop and waits for an in-flight poll to finish.
func (p *Poller) Stop() {
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
}

func (p *Poller) run(ctx context.Context, done chan struct{}) {
	defer close(done)
	if p.opts.InitialDelay > 0 {
		timer := time.NewTimer(p.opts.InitialDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
	p.RunOnce(ctx)
	ticker := time.NewTicker(p.opts.Tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.RunOnce(ctx)
		}
	}
}

func (p *Poller) interval() time.Duration {
	if p.opts.Interval == nil {
		return 10 * time.Minute
	}
	return p.opts.Interval()
}

// RunOnce reconciles the credential list and polls every credential that is due.
func (p *Poller) RunOnce(ctx context.Context) {
	if p == nil || p.opts.Manager == nil {
		return
	}
	interval := p.interval()
	if interval <= 0 {
		return
	}
	due := p.reconcile(p.opts.Manager.List(), p.opts.Now())
	for _, auth := range due {
		if ctx.Err() != nil {
			return
		}
		p.pollAndRecord(ctx, auth, interval)
	}
}

// reconcile tracks eligible credentials and returns the ones due for a poll.
// New credentials are due immediately; a token refresh or re-registration makes
// a credential due again (rate limited by minRepollAfterChange).
func (p *Poller) reconcile(auths []*coreauth.Auth, now time.Time) []*coreauth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := make(map[string]struct{}, len(auths))
	var due []*coreauth.Auth
	for _, auth := range auths {
		if pollProvider(auth) == "" {
			continue
		}
		seen[auth.ID] = struct{}{}
		state := p.states[auth.ID]
		if state == nil {
			state = &authPollState{nextPoll: now, credentialVersion: auth.CredentialVersion, registrationEpoch: auth.RegistrationEpoch}
			p.states[auth.ID] = state
		} else if state.credentialVersion != auth.CredentialVersion || state.registrationEpoch != auth.RegistrationEpoch {
			state.credentialVersion = auth.CredentialVersion
			state.registrationEpoch = auth.RegistrationEpoch
			state.failures = 0
			earliest := state.lastPoll.Add(minRepollAfterChange)
			if earliest.Before(now) {
				earliest = now
			}
			if earliest.Before(state.nextPoll) {
				state.nextPoll = earliest
			}
		}
		if !state.nextPoll.After(now) {
			due = append(due, auth)
		}
	}
	for id := range p.states {
		if _, ok := seen[id]; !ok {
			delete(p.states, id)
		}
	}
	sort.Slice(due, func(i, j int) bool { return due[i].ID < due[j].ID })
	return due
}

func (p *Poller) pollAndRecord(ctx context.Context, auth *coreauth.Auth, interval time.Duration) {
	windows, status, retryAfter, errPoll := p.Poll(ctx, auth)
	if ctx.Err() != nil {
		return
	}
	now := p.opts.Now()
	provider := pollProvider(auth)
	entry := log.WithFields(log.Fields{"provider": provider, "auth_id": auth.ID})

	var next time.Duration
	failures := 0
	switch {
	case errPoll == nil:
		if len(windows) > 0 {
			p.opts.Manager.ObserveQuotaWindows(auth.ID, windows)
		}
		entry.Debugf("quota poll: recorded %d window(s)", len(windows))
		next = p.jittered(interval)
	case status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusTooManyRequests:
		// Usage endpoint refusals never mark the credential bad; back off quietly.
		p.mu.Lock()
		if state := p.states[auth.ID]; state != nil {
			failures = state.failures + 1
		} else {
			failures = 1
		}
		p.mu.Unlock()
		next = backoffFor(interval, failures)
		if retryAfter > next {
			next = retryAfter
		}
		if next > maxBackoff {
			next = maxBackoff
		}
		entry.Debugf("quota poll: usage endpoint returned %d, backing off %s", status, next)
	default:
		entry.Debugf("quota poll failed: %v", errPoll)
		next = p.jittered(interval)
	}

	p.mu.Lock()
	state := p.states[auth.ID]
	if state == nil {
		state = &authPollState{credentialVersion: auth.CredentialVersion, registrationEpoch: auth.RegistrationEpoch}
		p.states[auth.ID] = state
	}
	state.lastPoll = now
	state.failures = failures
	state.nextPoll = now.Add(next)
	p.mu.Unlock()
}

func backoffFor(interval time.Duration, failures int) time.Duration {
	backoff := interval
	for i := 0; i < failures && backoff < maxBackoff; i++ {
		backoff *= 2
	}
	if backoff > maxBackoff {
		backoff = maxBackoff
	}
	return backoff
}

func (p *Poller) jittered(interval time.Duration) time.Duration {
	if p.opts.Jitter <= 0 {
		return interval
	}
	factor := 1 + (rand.Float64()*2-1)*p.opts.Jitter
	return time.Duration(float64(interval) * factor)
}

// Poll fetches and parses one credential's usage windows. It returns the HTTP
// status (0 for transport errors) and any Retry-After hint.
func (p *Poller) Poll(ctx context.Context, auth *coreauth.Auth) ([]coreauth.QuotaWindow, int, time.Duration, error) {
	provider := pollProvider(auth)
	if provider == "" {
		return nil, 0, 0, fmt.Errorf("quota poll: unsupported credential")
	}
	token := p.token(auth)
	if token == "" {
		return nil, 0, 0, errNoToken
	}
	if ctx == nil {
		ctx = context.Background()
	}
	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	targetURL, headers := p.opts.ClaudeURL, claudeRequestHeaders
	if provider == "codex" {
		targetURL, headers = p.opts.CodexURL, codexRequestHeaders
	}
	req, errReq := http.NewRequestWithContext(reqCtx, http.MethodGet, targetURL, nil)
	if errReq != nil {
		return nil, 0, 0, fmt.Errorf("quota poll: build request: %w", errReq)
	}
	for _, header := range headers {
		req.Header.Set(header[0], header[1])
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if provider == "codex" {
		if accountID := codexAccountID(auth); accountID != "" {
			req.Header.Set("Chatgpt-Account-Id", accountID)
		}
	}

	transport := p.transport(auth)
	defer closeIdleConnections(transport)
	resp, errDo := (&http.Client{Transport: transport}).Do(req)
	if errDo != nil {
		return nil, 0, 0, fmt.Errorf("quota poll: request failed: %w", errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debugf("quota poll: close response body: %v", errClose)
		}
	}()
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, retryAfterDuration(resp.Header.Get("Retry-After"), p.opts.Now()), fmt.Errorf("quota poll: usage endpoint status %d", resp.StatusCode)
	}
	if errRead != nil {
		return nil, resp.StatusCode, 0, fmt.Errorf("quota poll: read response: %w", errRead)
	}
	now := p.opts.Now()
	var (
		windows  []coreauth.QuotaWindow
		errParse error
	)
	if provider == "codex" {
		windows, errParse = ParseCodexUsage(body, now)
	} else {
		windows, errParse = ParseClaudeUsage(body, now)
	}
	if errParse != nil {
		return nil, resp.StatusCode, 0, fmt.Errorf("quota poll: parse %s usage: %w", provider, errParse)
	}
	return windows, resp.StatusCode, 0, nil
}

func (p *Poller) token(auth *coreauth.Auth) string {
	if p.opts.Token != nil {
		return strings.TrimSpace(p.opts.Token(auth))
	}
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	token, _ := auth.Metadata["access_token"].(string)
	return strings.TrimSpace(token)
}

func (p *Poller) transport(auth *coreauth.Auth) http.RoundTripper {
	if p.opts.Transport != nil {
		if transport := p.opts.Transport(auth); transport != nil {
			return transport
		}
	}
	if transport, ok := http.DefaultTransport.(*http.Transport); ok && transport != nil {
		return transport.Clone()
	}
	return &http.Transport{}
}

func closeIdleConnections(transport http.RoundTripper) {
	if closer, ok := transport.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

// pollProvider returns "claude" or "codex" for enabled OAuth credentials of
// those providers and "" for everything else.
func pollProvider(auth *coreauth.Auth) string {
	if auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled {
		return ""
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	if provider != "claude" && provider != "codex" {
		return ""
	}
	if auth.AuthKind() != coreauth.AuthKindOAuth {
		return ""
	}
	return provider
}

// codexAccountID resolves the ChatGPT account ID the same way the panel does
// (resolveCodexChatgptAccountId): explicit fields first, then the id_token claim.
func codexAccountID(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	for _, key := range []string{"account_id", "chatgpt_account_id", "chatgptAccountId"} {
		if value, ok := auth.Metadata[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
		if value := strings.TrimSpace(auth.Attributes[key]); value != "" {
			return value
		}
	}
	idToken, _ := auth.Metadata["id_token"].(string)
	if strings.TrimSpace(idToken) == "" {
		return ""
	}
	claims, errParse := codexauth.ParseJWTToken(strings.TrimSpace(idToken))
	if errParse != nil || claims == nil {
		return ""
	}
	return strings.TrimSpace(claims.CodexAuthInfo.ChatgptAccountID)
}

func retryAfterDuration(raw string, now time.Time) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if seconds, errParse := strconv.ParseFloat(raw, 64); errParse == nil && seconds > 0 {
		return time.Duration(seconds * float64(time.Second))
	}
	if when, errParse := http.ParseTime(raw); errParse == nil && when.After(now) {
		return when.Sub(now)
	}
	return 0
}
