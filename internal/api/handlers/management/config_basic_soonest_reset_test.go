package management

import "testing"

func TestNormalizeRoutingStrategyAcceptsSoonestReset(t *testing.T) {
	for _, raw := range []string{"soonest-reset", " Soonest-Reset ", "soonestreset", "sr"} {
		if got, ok := normalizeRoutingStrategy(raw); !ok || got != "soonest-reset" {
			t.Fatalf("normalizeRoutingStrategy(%q) = %q, %v", raw, got, ok)
		}
	}
}
