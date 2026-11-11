package api

import (
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
)

// registerManagementHubRoutes adds the hub usage, dashboard and routing views
// under both management prefixes, behind the same availability and auth
// middleware.
func (s *Server) registerManagementHubRoutes() {
	for _, prefix := range []string{"/v0/management", "/v8/management"} {
		group := s.engine.Group(prefix)
		group.Use(s.managementAvailabilityMiddleware(), s.mgmt.Middleware())
		if prefix == "/v8/management" {
			group.Use(func(c *gin.Context) { c.Set(management.ConfigV8ContextKey, true) })
		}
		group.GET("/usage/accounts", s.mgmt.GetUsageAccounts)
		group.GET("/usage/models", s.mgmt.GetUsageModels)
		group.GET("/usage/sessions", s.mgmt.GetUsageSessions)
		group.GET("/usage/runway", s.mgmt.GetUsageRunway)
		group.GET("/usage/value", s.mgmt.GetUsageValue)
		group.GET("/usage/events", s.mgmt.GetUsageEvents)
		group.GET("/routing/order", s.mgmt.GetRoutingOrder)
		group.GET("/box/health", s.mgmt.GetBoxHealth)
	}
}
