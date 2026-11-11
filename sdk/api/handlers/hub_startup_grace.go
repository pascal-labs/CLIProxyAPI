package handlers

import "time"

// The server starts accepting requests a moment before the initial credential
// load has registered every credential's models. A client that reconnects right
// after a restart (Codex re-opens its responses websocket within milliseconds)
// would then get "unknown provider for model", a non-retryable 400 that ends the
// client's run. During a short window after process start, an unroutable model
// waits for the registry to fill before it is reported as unknown.
var (
	hubProcessStart       = time.Now()
	hubStartupGraceWindow = 90 * time.Second
	hubStartupMaxWait     = 15 * time.Second
	hubStartupPoll        = 100 * time.Millisecond
	hubNow                = time.Now
	hubSleep              = time.Sleep
)

// waitForStartupProviders re-runs lookup until it returns providers, the
// per-request wait elapses, or the startup window closes. Outside the startup
// window it returns nil immediately, so a genuinely unknown model is reported
// at once.
func waitForStartupProviders(lookup func() []string) []string {
	deadline := hubNow().Add(hubStartupMaxWait)
	if windowEnd := hubProcessStart.Add(hubStartupGraceWindow); windowEnd.Before(deadline) {
		deadline = windowEnd
	}
	for hubNow().Before(deadline) {
		hubSleep(hubStartupPoll)
		if providers := lookup(); len(providers) > 0 {
			return providers
		}
	}
	return nil
}
