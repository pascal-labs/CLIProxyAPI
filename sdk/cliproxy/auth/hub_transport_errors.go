package auth

import (
	"context"
	"errors"
	"strings"
	"syscall"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// isHubTransientErrno extends isTransientSyscallError with socket errors that
// describe the local network path rather than the credential (interface or
// route down, connection reset by the network).
func isHubTransientErrno(errno syscall.Errno) bool {
	switch errno {
	case syscall.EHOSTDOWN, syscall.ENETDOWN, syscall.ENETRESET:
		return true
	default:
		return false
	}
}

// isHubTransientTransportMessage extends isTransientTransportMessage for errors
// that reached the conductor without their typed identity. It is only
// consulted when no HTTP status is attached, so provider error bodies cannot
// match it. The strings are the platform renderings of socket errors (macOS
// "operation timed out" and Linux "connection timed out" for ETIMEDOUT),
// net.OpError prefixes for reads and writes on an established connection, and
// HTTP/2 stream resets and GOAWAY frames.
func isHubTransientTransportMessage(lower string) bool {
	for _, marker := range []string{
		"operation timed out",
		"connection timed out",
		"host is down",
		"network is down",
		"read tcp ",
		"write tcp ",
		"read udp ",
		"stream error: stream id",
		"http2: server sent goaway",
		"http2: response body closed",
		"server closed idle connection",
		"tls: bad record mac",
		"local error: tls:",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// sessionTransportRetries tracks the single same-credential retry a
// session-bound request gets after a transport failure.
//
// Within one retry round the conductor normally excludes a credential that
// already failed and moves on to the next one. For a network failure (no HTTP
// status: timeouts, resets, broken pipes, dropped streams) that only moves a
// session with a warm prompt cache onto another credential with a cold one,
// because every credential shares the same network path. A session-bound
// request therefore retries its own credential once first; a second
// transport failure falls back to the normal rotation.
type sessionTransportRetries map[string]struct{}

// allow reports whether authID may be retried in place after err, and records
// the retry. Home dispatches, client cancellations, HTTP-status failures, and
// requests without a canonical session never qualify.
func (r sessionTransportRetries) allow(ctx context.Context, metadata map[string]any, authID string, err error) bool {
	if r == nil || authID == "" || err == nil {
		return false
	}
	if ctx != nil && ctx.Err() != nil {
		return false
	}
	if _, retried := r[authID]; retried {
		return false
	}
	if !isSessionBoundTransportFailure(err) {
		return false
	}
	sessionID, _ := metadata[cliproxyexecutor.CanonicalSessionIDMetadataKey].(string)
	if strings.TrimSpace(sessionID) == "" {
		return false
	}
	r[authID] = struct{}{}
	return true
}

// isSessionBoundTransportFailure reports transport-level failures that carry no
// HTTP status and say nothing about the credential.
func isSessionBoundTransportFailure(err error) bool {
	if err == nil || statusCodeFromError(err) != 0 {
		return false
	}
	if isCredentialScopedError(err) || isRequestScopedError(err) || isRequestInvalidError(err) {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) && !isConfiguredTLSHandshakeTimeoutError(err) {
		return false
	}
	return isTransientTransportError(err) || isConnectionLifecycleError(err)
}
