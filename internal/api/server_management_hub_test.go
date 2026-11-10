package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestManagementHubRoutesShareAccessControl(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name                      string
		enabled, home, authorized bool
		want                      int
	}{
		{"authorized", true, false, true, http.StatusOK},
		{"missing key", true, false, false, http.StatusUnauthorized},
		{"disabled", false, false, true, http.StatusNotFound},
		{"home", true, true, true, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte("port: 8317\nremote-management: {secret-key: test-password}\n"), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Home.Enabled = tc.home
			cfg.Plugins.Dir = filepath.Dir(path)
			h := management.NewHandler(cfg, path, nil)
			h.SetLocalPassword("test-password")
			s := &Server{cfg: cfg, engine: gin.New(), mgmt: h}
			s.managementRoutesEnabled.Store(tc.enabled)
			s.registerManagementRoutes()
			for _, route := range []string{
				"/v0/management/usage/accounts", "/v8/management/usage/accounts?window=7d",
				"/v0/management/routing/order", "/v8/management/routing/order?model=claude-fable-5",
			} {
				req := httptest.NewRequest(http.MethodGet, route, nil)
				req.RemoteAddr = "127.0.0.1:1234"
				if tc.authorized {
					req.Header.Set("Authorization", "Bearer test-password")
				}
				recorder := httptest.NewRecorder()
				s.engine.ServeHTTP(recorder, req)
				if recorder.Code != tc.want {
					t.Fatalf("%s: status=%d want=%d body=%s", route, recorder.Code, tc.want, recorder.Body.String())
				}
			}
		})
	}
}
