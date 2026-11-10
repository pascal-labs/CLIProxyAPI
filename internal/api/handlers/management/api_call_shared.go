package management

import (
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// APICallTransportForAuth returns the transport the management api-call endpoint
// uses for auth when no request proxy_url is given: the credential proxy, then
// the API-key config proxy, then the global proxy-url, then a direct connection.
// Background callers (such as the quota poller) use it to issue the same
// requests the management panel sends through api-call.
func APICallTransportForAuth(cfg *config.Config, auth *coreauth.Auth) http.RoundTripper {
	return (&Handler{cfg: cfg}).apiCallTransport(auth, "")
}

// APICallTokenForAuth returns the "$TOKEN$" substitution value api-call uses for
// auth (metadata access_token first). It does not refresh tokens; the auth
// manager's auto-refresh keeps OAuth access tokens current.
func APICallTokenForAuth(auth *coreauth.Auth) string {
	return tokenValueForAuth(auth)
}
