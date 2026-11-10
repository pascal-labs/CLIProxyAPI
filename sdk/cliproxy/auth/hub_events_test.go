package auth

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type hubEventRecorder struct {
	mu     sync.Mutex
	events []HubEvent
}

func (r *hubEventRecorder) record(event HubEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *hubEventRecorder) kinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	kinds := make([]string, 0, len(r.events))
	for _, event := range r.events {
		kinds = append(kinds, event.Kind)
	}
	return kinds
}

func (r *hubEventRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = nil
}

func installHubEventRecorder(t *testing.T) *hubEventRecorder {
	t.Helper()
	recorder := &hubEventRecorder{}
	SetHubEventSink(recorder.record)
	t.Cleanup(func() { SetHubEventSink(nil) })
	return recorder
}

func TestMarkResultEmitsHubEvents(t *testing.T) {
	withQuotaCooldownEnabled(t)
	recorder := installHubEventRecorder(t)
	m := NewManager(nil, nil, nil)
	auth := &Auth{ID: "hub-events-" + uuid.NewString(), Provider: "claude", Status: StatusActive}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatal(errRegister)
	}
	const model = "claude-opus-5-5"
	meta := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.CanonicalSessionIDMetadataKey: "claude:s1"}}
	mark := func(result Result) []string {
		recorder.reset()
		result.AuthID, result.Provider, result.Model, result.Options = auth.ID, "claude", model, meta
		m.MarkResult(context.Background(), result)
		return recorder.kinds()
	}
	retry := time.Minute
	if got := mark(Result{Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate limited: body text"}, RetryAfter: &retry}); strings.Join(got, ",") != "rate_limited,cooldown" {
		t.Fatalf("429 events = %v", got)
	}
	recorder.mu.Lock()
	first := recorder.events[0]
	recorder.mu.Unlock()
	if first.SessionKey != "claude:s1" || first.StatusCode != 429 || first.Model != model || strings.Contains(first.Detail, "body") {
		t.Fatalf("event = %+v", first)
	}
	// A second failure inside the same cooldown does not repeat the cooldown event.
	if got := mark(Result{Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "again"}, RetryAfter: &retry}); strings.Join(got, ",") != "rate_limited" {
		t.Fatalf("repeated 429 events = %v", got)
	}
	expireSessionAffinityPriorityModelCooldown(t, m, auth.ID, model)
	if got := mark(Result{Success: true}); strings.Join(got, ",") != "recovered" {
		t.Fatalf("success after cooldown events = %v", got)
	}
	if got := mark(Result{Success: true}); len(got) != 0 {
		t.Fatalf("healthy success events = %v", got)
	}
	if got := mark(Result{Error: resultErrorFromError(hubSocketError("read", 60))}); len(got) != 1 || got[0] != HubEventNetworkError {
		t.Fatalf("network failure events = %v", got)
	}
	if got := mark(Result{Error: resultErrorFromError(context.Canceled)}); len(got) != 0 {
		t.Fatalf("client cancellation events = %v", got)
	}
	if got := mark(Result{Error: &Error{HTTPStatus: http.StatusBadRequest, Message: "bad request"}}); len(got) != 0 {
		t.Fatalf("request fault events = %v", got)
	}
	if got := mark(Result{Error: &Error{HTTPStatus: http.StatusUnauthorized, Message: "unauthorized"}}); len(got) < 1 || got[0] != HubEventDeadLogin {
		t.Fatalf("401 events = %v", got)
	}
}

func TestClassifyHubFailure(t *testing.T) {
	for _, tc := range []struct {
		err        *Error
		kind, text string
	}{
		{&Error{HTTPStatus: 503, Message: "overloaded"}, HubEventTransientError, "Service Unavailable"},
		{&Error{HTTPStatus: 529, Message: "overloaded"}, HubEventTransientError, ""},
		{&Error{HTTPStatus: 402, Message: "x"}, HubEventDeadLogin, "payment_required"},
		{&Error{HTTPStatus: 403, Message: "x"}, HubEventDeadLogin, "forbidden"},
		{&Error{Code: transientTransportErrorCode, Message: "read tcp 192.0.2.1:1->192.0.2.2:443: read: operation timed out"}, HubEventNetworkError, "timeout"},
		{&Error{Message: "write tcp 192.0.2.1:1->192.0.2.2:443: write: broken pipe"}, HubEventNetworkError, "broken pipe"},
		{&Error{HTTPStatus: 404, Message: "not found"}, "", ""},
	} {
		kind, detail := classifyHubFailure(tc.err)
		if kind != tc.kind || (tc.text != "" && detail != tc.text) {
			t.Fatalf("classifyHubFailure(%+v) = %q, %q; want %q, %q", tc.err, kind, detail, tc.kind, tc.text)
		}
		if strings.Contains(detail, "192.0.2") {
			t.Fatalf("detail leaks the raw message: %q", detail)
		}
	}
}

func TestSessionAffinityEmitsFailoverAndRebind(t *testing.T) {
	recorder := installHubEventRecorder(t)
	for _, tc := range []struct {
		rebindOn string
		want     string
	}{
		{SessionAffinityRebindOnQuota, HubEventFailover},
		{SessionAffinityRebindOnAny, HubEventRebind},
	} {
		recorder.reset()
		selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &RoundRobinSelector{}, RebindOn: tc.rebindOn})
		a := &Auth{ID: "aff-a", Provider: "claude", Status: StatusActive}
		b := &Auth{ID: "aff-b", Provider: "claude", Status: StatusActive}
		opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{"s"}}, Metadata: map[string]any{}}
		first, errPick := selector.Pick(context.Background(), "claude", "claude-opus-5-5", opts, []*Auth{a, b})
		if errPick != nil {
			t.Fatal(errPick)
		}
		other := b
		if first.ID == b.ID {
			other = a
		}
		// The bound credential is cooling down after a transient 503.
		first.ModelStates = map[string]*ModelState{"claude-opus-5-5": {
			Unavailable: true, Status: StatusError, NextRetryAfter: time.Now().Add(time.Minute),
			LastError: &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "overloaded"},
		}}
		served, errPick := selector.Pick(context.Background(), "claude", "claude-opus-5-5", opts, []*Auth{a, b})
		if errPick != nil || served.ID != other.ID {
			t.Fatalf("failover pick = %v, %v", served, errPick)
		}
		selector.Stop()
		recorder.mu.Lock()
		events := append([]HubEvent(nil), recorder.events...)
		recorder.mu.Unlock()
		if len(events) != 1 || events[0].Kind != tc.want || events[0].AuthID != other.ID || events[0].RelatedAuthID != first.ID || events[0].Detail != "" {
			t.Fatalf("%s events = %+v", tc.rebindOn, events)
		}
	}
}
