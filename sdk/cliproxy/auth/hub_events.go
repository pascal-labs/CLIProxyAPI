package auth

import (
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// Hub event kinds (mirrored by internal/hubevents).
const (
	HubEventRateLimited    = "rate_limited"
	HubEventTransientError = "transient_error"
	HubEventNetworkError   = "network_error"
	HubEventFailover       = "failover"
	HubEventRebind         = "rebind"
	HubEventCooldown       = "cooldown"
	HubEventDeadLogin      = "dead_login"
	HubEventRecovered      = "recovered"
)

// HubEvent is a routing or credential-health event for the hub dashboard.
// Detail is built from fixed strings and status codes only; it never carries
// upstream messages, request or response bodies, or credential material.
type HubEvent struct {
	Time       time.Time
	Kind       string
	AuthID     string
	AuthIndex  string
	Provider   string
	Model      string
	SessionKey string
	StatusCode int
	Detail     string
	// RelatedAuthID is the credential a failover or rebind moved away from.
	// Like AuthID it identifies a credential file and must be resolved to an
	// auth_index before display.
	RelatedAuthID string
}

var hubEventSink atomic.Pointer[func(HubEvent)]

// SetHubEventSink installs the receiver of hub events; nil disables them. The
// sink is called synchronously from request paths, so it must be cheap, must
// not block, and must not call back into the Manager.
func SetHubEventSink(sink func(HubEvent)) {
	if sink == nil {
		hubEventSink.Store(nil)
		return
	}
	hubEventSink.Store(&sink)
}

func emitHubEvent(event HubEvent) {
	sink := hubEventSink.Load()
	if sink == nil || *sink == nil {
		return
	}
	if event.Time.IsZero() {
		event.Time = time.Now()
	}
	(*sink)(event)
}

func hubEventsEnabled() bool {
	sink := hubEventSink.Load()
	return sink != nil && *sink != nil
}

// hubHealth is the availability of one credential/model before or after a result.
type hubHealth struct {
	captured  bool
	blocked   bool
	unhealthy bool
	until     time.Time
}

func captureHubHealth(auth *Auth, modelKey string, now time.Time) hubHealth {
	health := hubHealth{captured: true}
	if auth == nil {
		return health
	}
	if auth.Unavailable {
		health.unhealthy = true
		if auth.NextRetryAfter.After(now) {
			health.blocked = true
			health.until = auth.NextRetryAfter
		}
	}
	if state := existingModelState(auth, modelKey); state != nil {
		if state.Unavailable || state.Status == StatusError {
			health.unhealthy = true
		}
		if state.Unavailable && state.NextRetryAfter.After(now) {
			health.blocked = true
			if state.NextRetryAfter.After(health.until) {
				health.until = state.NextRetryAfter
			}
		}
	}
	return health
}

func hubSessionKey(metadata map[string]any) string {
	if len(metadata) == 0 {
		return ""
	}
	if key, _ := metadata[cliproxyexecutor.CanonicalSessionIDMetadataKey].(string); strings.TrimSpace(key) != "" {
		return strings.TrimSpace(key)
	}
	return ""
}

// emitHubResultEvents classifies a MarkResult outcome. It runs after the
// manager lock is released and only reads the auth snapshot.
func emitHubResultEvents(result Result, before hubHealth, snapshot *Auth, modelKey string, now time.Time) {
	if snapshot == nil || !before.captured || !hubEventsEnabled() {
		return
	}
	model := strings.TrimSpace(result.Model)
	if model == "" {
		model = strings.TrimSpace(result.RouteModel)
	}
	base := HubEvent{
		Time:       now,
		AuthID:     result.AuthID,
		AuthIndex:  snapshot.Index,
		Provider:   strings.ToLower(strings.TrimSpace(result.Provider)),
		Model:      model,
		SessionKey: hubSessionKey(result.Options.Metadata),
	}
	after := captureHubHealth(snapshot, modelKey, now)
	if result.Success {
		if before.unhealthy && !after.unhealthy {
			event := base
			event.Kind = HubEventRecovered
			emitHubEvent(event)
		}
		return
	}
	kind, detail := classifyHubFailure(result.Error)
	status := statusCodeFromResult(result.Error)
	if kind != "" {
		event := base
		event.Kind, event.Detail, event.StatusCode = kind, detail, status
		emitHubEvent(event)
	}
	if after.blocked && (!before.blocked || after.until.After(before.until.Add(time.Second))) {
		event := base
		event.Kind = HubEventCooldown
		event.StatusCode = status
		event.Detail = "until " + after.until.UTC().Format(time.RFC3339)
		if kind != "" {
			event.Detail += " (" + kind + ")"
		}
		emitHubEvent(event)
	}
}

// classifyHubFailure maps a failed result to an event kind and a fixed-string
// detail. Client cancellations and request faults produce no event.
func classifyHubFailure(err *Error) (kind, detail string) {
	if err == nil {
		return "", ""
	}
	status := statusCodeFromResult(err)
	lower := strings.ToLower(err.Message)
	switch {
	case isInvalidGrantResultError(err):
		return HubEventDeadLogin, "invalid_grant"
	case isCreditBalanceTooLowResultError(err):
		return HubEventDeadLogin, "payment_required"
	case status == http.StatusUnauthorized:
		return HubEventDeadLogin, "unauthorized"
	case status == http.StatusPaymentRequired:
		return HubEventDeadLogin, "payment_required"
	case status == http.StatusForbidden:
		return HubEventDeadLogin, "forbidden"
	case status == http.StatusTooManyRequests:
		if isTransientRateLimitResultError(err) {
			return HubEventRateLimited, "transient capacity"
		}
		return HubEventRateLimited, "quota"
	case status == http.StatusRequestTimeout || status >= 500:
		return HubEventTransientError, http.StatusText(status)
	case status != 0:
		return "", ""
	case strings.Contains(lower, "context canceled") || isRequestScopedResultError(err):
		return "", ""
	case isStreamTruncatedResultError(err):
		return HubEventNetworkError, "stream truncated"
	case isTransientTransportResultError(err) || isConnectionLifecycleResultError(err):
		return HubEventNetworkError, networkErrorDetail(lower)
	default:
		return "", ""
	}
}

func networkErrorDetail(lower string) string {
	for _, pair := range [][2]string{
		{"timed out", "timeout"},
		{"timeout", "timeout"},
		{"no route to host", "no route to host"},
		{"host is down", "host down"},
		{"network is down", "network down"},
		{"network is unreachable", "network unreachable"},
		{"connection reset", "connection reset"},
		{"broken pipe", "broken pipe"},
		{"connection refused", "connection refused"},
		{"no such host", "dns failure"},
		{"tls", "tls error"},
		{"stream error", "http2 stream reset"},
		{"goaway", "http2 goaway"},
		{"eof", "unexpected eof"},
		{"websocket", "websocket closed"},
	} {
		if strings.Contains(lower, pair[0]) {
			return pair[1]
		}
	}
	return "transport error"
}

// emitHubRefreshFailure reports a token refresh rejected as unauthorized or
// with invalid_grant.
func emitHubRefreshFailure(auth *Auth, unauthorized, invalidGrant bool) {
	if auth == nil || (!unauthorized && !invalidGrant) || !hubEventsEnabled() {
		return
	}
	detail := "refresh unauthorized"
	status := http.StatusUnauthorized
	if invalidGrant {
		detail, status = "refresh invalid_grant", http.StatusBadRequest
	}
	emitHubEvent(HubEvent{Kind: HubEventDeadLogin, AuthID: auth.ID, Provider: strings.ToLower(strings.TrimSpace(auth.Provider)), StatusCode: status, Detail: detail})
}

// emitHubAffinityEvent reports a session that moved off its bound credential:
// failover keeps the binding, rebind replaces it.
func emitHubAffinityEvent(kind, sessionKey, boundAuthID string, served *Auth, model string) {
	if served == nil || !hubEventsEnabled() {
		return
	}
	emitHubEvent(HubEvent{Kind: kind, AuthID: served.ID, Provider: strings.ToLower(strings.TrimSpace(served.Provider)), Model: model, SessionKey: sessionKey, RelatedAuthID: boundAuthID})
}
