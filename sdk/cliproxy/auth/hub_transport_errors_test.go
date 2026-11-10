package auth

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func hubSocketError(op string, errno syscall.Errno) error {
	return &net.OpError{
		Op:     op,
		Net:    "tcp",
		Source: &net.TCPAddr{IP: net.ParseIP("2001:db8::1"), Port: 58601},
		Addr:   &net.TCPAddr{IP: net.ParseIP("2001:db8::10"), Port: 443},
		Err:    &os.SyscallError{Syscall: op, Err: errno},
	}
}

// hubTransportFailures are the transport-level failures seen on a live hub
// (a stream read timing out mid-response, and in-flight requests failing with
// "no route to host" / "broken pipe" during the same network blip), plus the
// same failures after losing their typed identity.
func hubTransportFailures() map[string]error {
	return map[string]error{
		"stream read timeout":           hubSocketError("read", syscall.ETIMEDOUT),
		"request no route to host":      &url.Error{Op: "Post", URL: "https://upstream.example/v1/messages", Err: hubSocketError("read", syscall.EHOSTUNREACH)},
		"request broken pipe":           &url.Error{Op: "Post", URL: "https://upstream.example/v1/messages", Err: hubSocketError("write", syscall.EPIPE)},
		"connection reset":              hubSocketError("read", syscall.ECONNRESET),
		"host down":                     hubSocketError("read", syscall.EHOSTDOWN),
		"untyped macOS read timeout":    errors.New("read tcp [2001:db8::1]:58601->[2001:db8::10]:443: read: operation timed out"),
		"untyped linux read timeout":    errors.New("read tcp 192.0.2.1:5555->192.0.2.9:443: read: connection timed out"),
		"untyped http2 stream reset":    errors.New("stream error: stream ID 7; INTERNAL_ERROR; received from peer"),
		"unexpected eof":                io.ErrUnexpectedEOF,
		"wrapped unexpected eof":        &url.Error{Op: "Post", URL: "https://upstream.example/v1/messages", Err: io.ErrUnexpectedEOF},
		"client canceled":               context.Canceled,
		"client canceled wrapped":       &url.Error{Op: "Post", URL: "https://upstream.example/v1/messages", Err: context.Canceled},
		"result message only (timeout)": &Error{Message: "read tcp [2001:db8::1]:58601->[2001:db8::10]:443: read: operation timed out"},
	}
}

func TestHubTransportFailuresNeverCoolCredential(t *testing.T) {
	withQuotaCooldownEnabled(t)
	prevTransient := transientErrorCooldownSeconds.Load()
	SetTransientErrorCooldownSeconds(60)
	t.Cleanup(func() { transientErrorCooldownSeconds.Store(prevTransient) })

	for name, failure := range hubTransportFailures() {
		for _, model := range []string{"claude-opus-5-5", ""} {
			t.Run(name+"/model="+model, func(t *testing.T) {
				m := NewManager(nil, nil, nil)
				auth := &Auth{ID: "hub-transport-" + uuid.NewString(), Provider: "claude", Status: StatusActive}
				if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
					t.Fatalf("register: %v", errRegister)
				}
				resultErr := resultErrorFromError(failure)
				if !shouldSkipCredentialCooldown(resultErr) {
					t.Fatalf("shouldSkipCredentialCooldown(%q code=%q) = false", resultErr.Message, resultErr.Code)
				}
				if shouldRebindSessionAffinityOnError(resultErr) {
					t.Fatalf("transport failure %q would rebind the session", resultErr.Message)
				}
				m.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: "claude", Model: model, Error: resultErr})
				assertNoCooldown(t, m, auth.ID, model)
				updated, _ := m.GetByID(auth.ID)
				if !IsAuthAvailableForModel(updated, "claude-opus-5-5", time.Now()) {
					t.Fatal("credential is not available for the next request")
				}
			})
		}
	}
}

func TestHubHTTPFailuresStillCoolCredential(t *testing.T) {
	withQuotaCooldownEnabled(t)
	prevTransient := transientErrorCooldownSeconds.Load()
	SetTransientErrorCooldownSeconds(60)
	t.Cleanup(func() { transientErrorCooldownSeconds.Store(prevTransient) })

	const model = "claude-opus-5-5"
	for _, status := range []int{http.StatusTooManyRequests, http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			auth := &Auth{ID: "hub-http-" + uuid.NewString(), Provider: "claude", Status: StatusActive}
			if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
				t.Fatalf("register: %v", errRegister)
			}
			// A transport-looking body must not soften a real HTTP status.
			m.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: "claude", Model: model, Error: &Error{HTTPStatus: status, Message: "read tcp: operation timed out"}})
			updated, _ := m.GetByID(auth.ID)
			state := updated.ModelStates[model]
			if state == nil || !state.Unavailable || !state.NextRetryAfter.After(time.Now()) {
				t.Fatalf("status %d did not cool the model: %#v", status, state)
			}
		})
	}
}

// scriptedSessionExecutor fails the calls listed in failures and succeeds
// otherwise, recording which credential served each call.
type scriptedSessionExecutor struct {
	mu       sync.Mutex
	calls    []string
	failures map[int]error
}

func (e *scriptedSessionExecutor) Identifier() string { return "claude" }

func (e *scriptedSessionExecutor) next(auth *Auth) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, auth.ID)
	return e.failures[len(e.calls)]
}

func (e *scriptedSessionExecutor) attempts() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.calls...)
}

func (e *scriptedSessionExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if err := e.next(auth); err != nil {
		return cliproxyexecutor.Response{}, err
	}
	return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
}

func (e *scriptedSessionExecutor) ExecuteStream(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if err := e.next(auth); err != nil {
		return nil, err
	}
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(auth.ID)}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func (*scriptedSessionExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *scriptedSessionExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return e.Execute(ctx, auth, req, opts)
}

func (*scriptedSessionExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestSessionBoundTransportFailureRetriesBoundCredential(t *testing.T) {
	withQuotaCooldownEnabled(t)
	transport := &url.Error{Op: "Post", URL: "https://upstream.example/v1/messages", Err: hubSocketError("read", syscall.EHOSTUNREACH)}
	for _, path := range []string{"execute", "stream"} {
		for _, tc := range []struct {
			name     string
			session  bool
			failures map[int]error
			// wantRetry is the expected attempt sequence after the binding call,
			// as "bound"/"other".
			wantRetry []string
		}{
			{name: "session retries bound credential", session: true, failures: map[int]error{2: transport}, wantRetry: []string{"bound", "bound"}},
			{name: "second failure rotates", session: true, failures: map[int]error{2: transport, 3: transport}, wantRetry: []string{"bound", "bound", "other"}},
			{name: "http status rotates immediately", session: true, failures: map[int]error{2: &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "overloaded"}}, wantRetry: []string{"bound", "other"}},
			// Without a session the round-robin start differs; only the rotation matters.
			{name: "no session keeps rotation", session: false, failures: map[int]error{2: transport}, wantRetry: nil},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				ctx := context.Background()
				model := "claude-opus-hub-" + uuid.NewString()
				selector := NewSessionAffinitySelector(&RoundRobinSelector{})
				t.Cleanup(selector.Stop)
				manager := NewManager(nil, selector, nil)
				manager.SetRetryConfig(0, 0, 0)
				executor := &scriptedSessionExecutor{failures: tc.failures}
				manager.RegisterExecutor(executor)
				for _, id := range []string{"a-" + uuid.NewString(), "b-" + uuid.NewString()} {
					registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: model}})
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
					if _, errRegister := manager.Register(ctx, &Auth{ID: id, Provider: "claude", Status: StatusActive}); errRegister != nil {
						t.Fatal(errRegister)
					}
				}
				run := func() (string, error) {
					opts := cliproxyexecutor.Options{Metadata: map[string]any{}}
					if tc.session {
						opts.Headers = http.Header{"X-Session-Id": []string{"hub-session"}}
					}
					req := cliproxyexecutor.Request{Model: model}
					if path == "execute" {
						resp, errExecute := manager.Execute(ctx, []string{"claude"}, req, opts)
						return string(resp.Payload), errExecute
					}
					opts.Stream = true
					result, errStream := manager.ExecuteStream(ctx, []string{"claude"}, req, opts)
					if errStream != nil {
						return "", errStream
					}
					var payload []byte
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							return "", chunk.Err
						}
						payload = append(payload, chunk.Payload...)
					}
					return string(payload), nil
				}
				bound, errBind := run()
				if errBind != nil {
					t.Fatalf("binding request: %v", errBind)
				}
				served, errRun := run()
				if errRun != nil {
					t.Fatalf("request after failure: %v (attempts %v)", errRun, executor.attempts())
				}
				attempts := executor.attempts()[1:]
				if tc.wantRetry == nil {
					if len(attempts) != 2 || attempts[0] == attempts[1] || served != attempts[1] {
						t.Fatalf("attempts = %v served = %q, want a failure on one credential then success on the other", attempts, served)
					}
					return
				}
				got := make([]string, 0, len(attempts))
				for _, id := range attempts {
					if id == bound {
						got = append(got, "bound")
					} else {
						got = append(got, "other")
					}
				}
				if len(got) != len(tc.wantRetry) {
					t.Fatalf("attempts = %v, want %v", got, tc.wantRetry)
				}
				for i := range got {
					if got[i] != tc.wantRetry[i] {
						t.Fatalf("attempts = %v, want %v", got, tc.wantRetry)
					}
				}
				if want := tc.wantRetry[len(tc.wantRetry)-1]; (want == "bound") != (served == bound) {
					t.Fatalf("served by %q, bound %q, want %s", served, bound, want)
				}
				if tc.session && tc.name != "http status rotates immediately" {
					assertNoCooldown(t, manager, bound, model)
				}
			})
		}
	}
}
