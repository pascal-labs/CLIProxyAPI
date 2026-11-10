package quotapoller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/MoeForge/moego/base64"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type fakeManager struct {
	mu       sync.Mutex
	auths    []*coreauth.Auth
	observed map[string][]coreauth.QuotaWindow
}

func (f *fakeManager) List() []*coreauth.Auth {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*coreauth.Auth, 0, len(f.auths))
	for _, auth := range f.auths {
		out = append(out, auth.Clone())
	}
	return out
}

func (f *fakeManager) ObserveQuotaWindows(authID string, windows []coreauth.QuotaWindow) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.observed == nil {
		f.observed = make(map[string][]coreauth.QuotaWindow)
	}
	f.observed[authID] = append(f.observed[authID], windows...)
	return true
}

func (f *fakeManager) observations(authID string) []coreauth.QuotaWindow {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]coreauth.QuotaWindow(nil), f.observed[authID]...)
}

type usageServer struct {
	mu       sync.Mutex
	requests []*http.Request
	status   int
	body     []byte
	headers  http.Header
}

func (s *usageServer) handler(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, r.Clone(context.Background()))
	status, body, headers := s.status, s.body, s.headers
	s.mu.Unlock()
	for key, values := range headers {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (s *usageServer) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func (s *usageServer) lastRequest() *http.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) == 0 {
		return nil
	}
	return s.requests[len(s.requests)-1]
}

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newTestPoller(t *testing.T, manager AuthManager, claude, codex *usageServer, clock *testClock) *Poller {
	t.Helper()
	opts := Options{
		Manager:  manager,
		Interval: func() time.Duration { return 10 * time.Minute },
		Now:      clock.Now,
		Jitter:   -1,
	}
	if claude != nil {
		server := httptest.NewServer(http.HandlerFunc(claude.handler))
		t.Cleanup(server.Close)
		opts.ClaudeURL = server.URL + "/api/oauth/usage"
	}
	if codex != nil {
		server := httptest.NewServer(http.HandlerFunc(codex.handler))
		t.Cleanup(server.Close)
		opts.CodexURL = server.URL + "/backend-api/wham/usage"
	}
	return New(opts)
}

func claudeAuth(id, token string) *coreauth.Auth {
	return &coreauth.Auth{ID: id, Provider: "claude", Metadata: map[string]any{"access_token": token, "type": "claude"}}
}

func TestPollerSendsPanelClaudeRequestAndRecordsWindows(t *testing.T) {
	claude := &usageServer{body: readFixture(t, "claude_usage.json")}
	manager := &fakeManager{auths: []*coreauth.Auth{claudeAuth("claude-a.json", "tok-a")}}
	clock := &testClock{now: time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)}
	poller := newTestPoller(t, manager, claude, nil, clock)

	poller.RunOnce(context.Background())

	req := claude.lastRequest()
	if req == nil {
		t.Fatal("no usage request sent")
	}
	if req.Method != http.MethodGet || req.URL.Path != "/api/oauth/usage" {
		t.Fatalf("request = %s %s", req.Method, req.URL.Path)
	}
	for header, want := range map[string]string{
		"Authorization":  "Bearer tok-a",
		"User-Agent":     "claude-cli/2.1.280 (external, cli)",
		"Content-Type":   "application/json",
		"Anthropic-Beta": "oauth-2025-04-20",
		"Accept":         "application/json",
	} {
		if got := req.Header.Get(header); got != want {
			t.Fatalf("header %s = %q, want %q", header, got, want)
		}
	}
	byKey := windowsByKey(manager.observations("claude-a.json"))
	if byKey["seven_day_fable"].UsedPercent != 64 || byKey["five_hour"].UsedPercent != 42.5 {
		t.Fatalf("observed windows = %#v", byKey)
	}

	// Not due again until the interval passes.
	poller.RunOnce(context.Background())
	if claude.requestCount() != 1 {
		t.Fatalf("requests = %d, want 1 before the interval elapses", claude.requestCount())
	}
	clock.Advance(10 * time.Minute)
	poller.RunOnce(context.Background())
	if claude.requestCount() != 2 {
		t.Fatalf("requests = %d, want 2 after the interval", claude.requestCount())
	}
}

func TestPollerSendsCodexAccountIDHeader(t *testing.T) {
	codex := &usageServer{body: readFixture(t, "codex_usage_pro.json")}
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"acct-from-jwt"}}`))
	manager := &fakeManager{auths: []*coreauth.Auth{
		{ID: "codex-a.json", Provider: "codex", Metadata: map[string]any{"access_token": "tok-a", "account_id": "acct-meta"}},
		{ID: "codex-b.json", Provider: "codex", Metadata: map[string]any{"access_token": "tok-b", "id_token": "e30." + payload + ".sig"}},
	}}
	clock := &testClock{now: time.Unix(1785300000, 0).UTC()}
	poller := newTestPoller(t, manager, nil, codex, clock)

	poller.RunOnce(context.Background())

	codex.mu.Lock()
	requests := append([]*http.Request(nil), codex.requests...)
	codex.mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(requests))
	}
	wantAccounts := map[string]string{"Bearer tok-a": "acct-meta", "Bearer tok-b": "acct-from-jwt"}
	for _, req := range requests {
		auth := req.Header.Get("Authorization")
		if got := req.Header.Get("Chatgpt-Account-Id"); got != wantAccounts[auth] {
			t.Fatalf("%s Chatgpt-Account-Id = %q, want %q", auth, got, wantAccounts[auth])
		}
		if got := req.Header.Get("User-Agent"); got != codexRequestHeaders[1][1] {
			t.Fatalf("User-Agent = %q", got)
		}
	}
	if byKey := windowsByKey(manager.observations("codex-b.json")); byKey["seven_day"].UsedPercent != 1 {
		t.Fatalf("codex windows = %#v", byKey)
	}
}

func TestPollerBacksOffQuietlyOnRefusals(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests} {
		claude := &usageServer{status: status, body: []byte(`{"error":"nope"}`)}
		auth := claudeAuth("claude-a.json", "tok-a")
		manager := &fakeManager{auths: []*coreauth.Auth{auth}}
		clock := &testClock{now: time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)}
		poller := newTestPoller(t, manager, claude, nil, clock)

		poller.RunOnce(context.Background())
		if len(manager.observations(auth.ID)) != 0 {
			t.Fatalf("status %d recorded windows", status)
		}
		// The first refusal doubles the interval; a plain interval later is not due yet.
		clock.Advance(10 * time.Minute)
		poller.RunOnce(context.Background())
		if claude.requestCount() != 1 {
			t.Fatalf("status %d: requests = %d, want backoff to suppress the retry", status, claude.requestCount())
		}
		clock.Advance(10 * time.Minute)
		poller.RunOnce(context.Background())
		if claude.requestCount() != 2 {
			t.Fatalf("status %d: requests = %d, want retry after backoff", status, claude.requestCount())
		}
		if auth.Disabled || auth.Status != "" || auth.Unavailable {
			t.Fatalf("status %d mutated the credential: %#v", status, auth)
		}
	}
}

func TestPollerHonorsRetryAfterOn429(t *testing.T) {
	claude := &usageServer{status: http.StatusTooManyRequests, headers: http.Header{"Retry-After": []string{"3600"}}}
	manager := &fakeManager{auths: []*coreauth.Auth{claudeAuth("claude-a.json", "tok-a")}}
	clock := &testClock{now: time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)}
	poller := newTestPoller(t, manager, claude, nil, clock)

	poller.RunOnce(context.Background())
	clock.Advance(30 * time.Minute)
	poller.RunOnce(context.Background())
	if claude.requestCount() != 1 {
		t.Fatalf("requests = %d, want Retry-After to defer the retry", claude.requestCount())
	}
	clock.Advance(31 * time.Minute)
	poller.RunOnce(context.Background())
	if claude.requestCount() != 2 {
		t.Fatalf("requests = %d, want retry once Retry-After elapsed", claude.requestCount())
	}
}

func TestPollerRepollsAfterCredentialChange(t *testing.T) {
	claude := &usageServer{body: readFixture(t, "claude_usage.json")}
	auth := claudeAuth("claude-a.json", "tok-a")
	auth.CredentialVersion = 1
	manager := &fakeManager{auths: []*coreauth.Auth{auth}}
	clock := &testClock{now: time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)}
	poller := newTestPoller(t, manager, claude, nil, clock)

	poller.RunOnce(context.Background())
	manager.mu.Lock()
	manager.auths[0].CredentialVersion = 2
	manager.mu.Unlock()
	clock.Advance(10 * time.Second)
	poller.RunOnce(context.Background())
	if claude.requestCount() != 1 {
		t.Fatalf("requests = %d, want re-poll rate limited right after a poll", claude.requestCount())
	}
	clock.Advance(minRepollAfterChange)
	poller.RunOnce(context.Background())
	if claude.requestCount() != 2 {
		t.Fatalf("requests = %d, want re-poll after credential change", claude.requestCount())
	}

	// A newly added credential is polled on the next tick.
	manager.mu.Lock()
	manager.auths = append(manager.auths, claudeAuth("claude-b.json", "tok-b"))
	manager.mu.Unlock()
	poller.RunOnce(context.Background())
	if claude.requestCount() != 3 || claude.lastRequest().Header.Get("Authorization") != "Bearer tok-b" {
		t.Fatalf("requests = %d, want the new credential polled", claude.requestCount())
	}
}

func TestPollerSkipsIneligibleCredentialsAndDisabledInterval(t *testing.T) {
	claude := &usageServer{body: readFixture(t, "claude_usage.json")}
	disabled := claudeAuth("claude-disabled.json", "tok")
	disabled.Disabled = true
	manager := &fakeManager{auths: []*coreauth.Auth{
		disabled,
		{ID: "claude-apikey", Provider: "claude", Attributes: map[string]string{"api_key": "sk-ant"}},
		{ID: "gemini.json", Provider: "gemini", Metadata: map[string]any{"access_token": "tok"}},
		claudeAuth("claude-no-token.json", ""),
	}}
	clock := &testClock{now: time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)}
	poller := newTestPoller(t, manager, claude, nil, clock)
	poller.RunOnce(context.Background())
	if claude.requestCount() != 0 {
		t.Fatalf("requests = %d, want ineligible credentials skipped", claude.requestCount())
	}

	manager.mu.Lock()
	manager.auths = []*coreauth.Auth{claudeAuth("claude-a.json", "tok-a")}
	manager.mu.Unlock()
	poller.opts.Interval = func() time.Duration { return 0 }
	poller.RunOnce(context.Background())
	if claude.requestCount() != 0 {
		t.Fatalf("requests = %d, want poll-interval 0 to disable polling", claude.requestCount())
	}
}

func TestPollerStoresWindowsOnManagerAuth(t *testing.T) {
	claude := &usageServer{body: readFixture(t, "claude_usage.json")}
	manager := coreauth.NewManager(nil, nil, nil)
	if _, errRegister := manager.Register(context.Background(), claudeAuth("claude-a.json", "tok-a")); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	clock := &testClock{now: time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)}
	poller := newTestPoller(t, manager, claude, nil, clock)
	poller.RunOnce(context.Background())

	auth, ok := manager.GetByID("claude-a.json")
	if !ok || auth == nil {
		t.Fatal("auth missing")
	}
	byKey := windowsByKey(auth.Quota.Windows)
	if byKey["seven_day"].UsedPercent != 16 || byKey["seven_day_fable"].Model != "fable" {
		t.Fatalf("auth windows = %#v", auth.Quota.Windows)
	}
	if auth.Status == coreauth.StatusError || auth.Unavailable {
		t.Fatalf("poll changed auth status: %#v", auth)
	}
}

func TestPollerStartStop(t *testing.T) {
	claude := &usageServer{body: readFixture(t, "claude_usage.json")}
	manager := &fakeManager{auths: []*coreauth.Auth{claudeAuth("claude-a.json", "tok-a")}}
	server := httptest.NewServer(http.HandlerFunc(claude.handler))
	defer server.Close()
	observed := make(chan struct{})
	var once sync.Once
	poller := New(Options{
		Manager:      &notifyingManager{fakeManager: manager, notify: func() { once.Do(func() { close(observed) }) }},
		ClaudeURL:    server.URL,
		InitialDelay: -1,
		Tick:         time.Hour,
	})
	poller.Start(context.Background())
	select {
	case <-observed:
	case <-time.After(5 * time.Second):
		t.Fatal("poller did not poll at startup")
	}
	poller.Stop()
	poller.Stop()
}

type notifyingManager struct {
	*fakeManager
	notify func()
}

func (n *notifyingManager) ObserveQuotaWindows(authID string, windows []coreauth.QuotaWindow) bool {
	ok := n.fakeManager.ObserveQuotaWindows(authID, windows)
	n.notify()
	return ok
}
