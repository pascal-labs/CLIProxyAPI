package config

import "testing"

// The hub's own `hub:` section (prices, subscriptions, session roots) must survive the v8 layout normalization that
// the management panel's config save runs; it used to be commented out as an unknown section.
func TestV8AllowedRootsKeepsHubSection(t *testing.T) {
	if !v8AllowedRoots()["hub"] {
		t.Fatal(`v8AllowedRoots() must include "hub"`)
	}
}
