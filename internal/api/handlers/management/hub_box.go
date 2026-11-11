package management

import (
	"encoding/json"
	"errors"
	"io/fs"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/hubsidecar"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

const (
	// boxHealthStaleAfter marks the export stale; the exporter runs about once a minute.
	boxHealthStaleAfter = 3 * time.Minute
	// boxHealthMaxBytes bounds what the endpoint will read and forward.
	boxHealthMaxBytes = 4 << 20
)

// GetBoxHealth returns the host-health export written next to the auth files
// (box-health.json) by an external exporter, wrapped with its age. The hub
// never collects these numbers itself and does not interpret the document
// beyond its generated_at time.
//
// Endpoint: GET /v8/management/box/health (also under /v0/management).
//
//	{"available": false, "reason": "..."}                      no usable export
//	{"available": true, "generated_at": "...", "age_seconds": 12,
//	 "stale": false, "box": { ...exporter document... }}
func (h *Handler) GetBoxHealth(c *gin.Context) {
	authDir := ""
	if h != nil && h.cfg != nil && h.cfg.AuthDir != "" {
		if resolved, errResolve := util.ResolveAuthDir(h.cfg.AuthDir); errResolve == nil {
			authDir = resolved
		}
	}
	if authDir == "" {
		c.JSON(http.StatusOK, gin.H{"available": false, "reason": "auth directory not configured"})
		return
	}
	path := filepath.Join(authDir, hubsidecar.BoxHealthFile)
	info, errStat := os.Stat(path)
	if errors.Is(errStat, fs.ErrNotExist) {
		c.JSON(http.StatusOK, gin.H{"available": false, "reason": "no export yet"})
		return
	}
	if errStat != nil || !info.Mode().IsRegular() || info.Size() > boxHealthMaxBytes {
		c.JSON(http.StatusOK, gin.H{"available": false, "reason": "export unreadable"})
		return
	}
	data, errRead := os.ReadFile(path)
	var head struct {
		GeneratedAt string `json:"generated_at"`
	}
	if errRead != nil || !json.Valid(data) || json.Unmarshal(data, &head) != nil {
		c.JSON(http.StatusOK, gin.H{"available": false, "reason": "export unreadable"})
		return
	}
	generated, errParse := time.Parse(time.RFC3339, head.GeneratedAt)
	if errParse != nil {
		generated = info.ModTime()
	}
	age := hubNow().Sub(generated)
	if age < 0 {
		age = 0
	}
	c.JSON(http.StatusOK, gin.H{
		"available":    true,
		"generated_at": generated.UTC().Format(time.RFC3339),
		"age_seconds":  int64(math.Round(age.Seconds())),
		"stale":        age > boxHealthStaleAfter,
		"box":          json.RawMessage(data),
	})
}
