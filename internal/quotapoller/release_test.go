package quotapoller

import (
	"context"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

type releasingManager struct {
	fakeManager
	calls int
}

func (r *releasingManager) ReleaseRecoveredQuota(_ context.Context, _ string, _ []coreauth.QuotaWindow) bool {
	r.calls++
	return true
}

func TestMaybeReleaseQuotaSpacing(t *testing.T) {
	mgr := &releasingManager{}
	p := New(Options{Manager: mgr})
	auth := &coreauth.Auth{ID: "codex-a"}
	windows := []coreauth.QuotaWindow{{Key: "seven_day", UsedPercent: 0}}
	entry := log.WithField("t", "x")
	now := time.Now()

	p.maybeReleaseQuota(context.Background(), auth, windows, now, entry)
	p.maybeReleaseQuota(context.Background(), auth, windows, now.Add(10*time.Minute), entry)
	if mgr.calls != 1 {
		t.Fatalf("calls within spacing = %d, want 1", mgr.calls)
	}
	p.maybeReleaseQuota(context.Background(), auth, windows, now.Add(releaseSpacing+time.Minute), entry)
	if mgr.calls != 2 {
		t.Fatalf("calls after spacing = %d, want 2", mgr.calls)
	}
}
