package config

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestHubConfigPricesDefaultsAndOverrides(t *testing.T) {
	var cfg Config
	if errUnmarshal := yaml.Unmarshal([]byte(`
hub:
  prices:
    claude-opus-5-5: {input: 3, output: 15, cache_read: 0.3, cache_write_5m: 3.75, cache_write_1h: 6}
    GPT-6.1: {input: 1.25, output: 10, cache_read: 0.125}
  subscriptions:
    "claude-team-a.json": 200
    0123456789abcdef: 20
  session-roots: ["~/.claude", "/srv/agents/.codex"]
`), &cfg); errUnmarshal != nil {
		t.Fatalf("unmarshal: %v", errUnmarshal)
	}
	table := cfg.Hub.PriceTable()
	for model, want := range map[string]float64{
		"claude-opus-5-5":            3,    // override
		"claude-opus-5-5-20261001":   3,    // date suffix, longest prefix
		"claude-opus-5":              5,    // built-in, shorter prefix
		"claude-opus-5-1":            5,    // falls back to the claude-opus-5 family
		"anthropic/claude-fable-5-2": 8,    // provider path stripped
		"Claude-Sonnet-5-5[1m]":      2,    // case-insensitive
		"gpt-6.1-sol":                1.25, // configured OpenAI price
	} {
		price, _, ok := table.Lookup(model)
		if !ok || price.Input != want {
			t.Fatalf("Lookup(%s) = %+v, %v; want input %v", model, price, ok, want)
		}
	}
	for _, model := range []string{"gpt-5.5-codex", "unknown", "", "claude-mystery-9"} {
		if _, _, ok := table.Lookup(model); ok {
			t.Fatalf("Lookup(%s) priced; want unpriced", model)
		}
	}
	if monthly, ok := cfg.Hub.SubscriptionFor("/auths/Claude-Team-A.json", "ffff"); !ok || monthly != 200 {
		t.Fatalf("subscription by file name = %v, %v", monthly, ok)
	}
	if monthly, ok := cfg.Hub.SubscriptionFor("codex.json", "0123456789abcdef"); !ok || monthly != 20 {
		t.Fatalf("subscription by auth_index = %v, %v", monthly, ok)
	}
	if _, ok := cfg.Hub.SubscriptionFor("other.json", "x"); ok {
		t.Fatal("unconfigured credential has a subscription")
	}
	if len(cfg.Hub.SessionRoots) != 2 {
		t.Fatalf("session roots = %v", cfg.Hub.SessionRoots)
	}
}

func TestHubConfigBuiltInPricesWithoutConfig(t *testing.T) {
	var cfg Config
	table := cfg.Hub.PriceTable()
	price, key, ok := table.Lookup("claude-haiku-5-5")
	if !ok || key != "claude-haiku-5-5" || price.Output != 0.5 || price.CacheWrite1h != 0.2 {
		t.Fatalf("built-in haiku = %+v %q %v", price, key, ok)
	}
	if len(DefaultHubPrices()) != 7 {
		t.Fatalf("built-in price count = %d", len(DefaultHubPrices()))
	}
	for key := range DefaultHubPrices() {
		if key[:7] != "claude-" {
			t.Fatalf("non-Claude built-in price %q", key)
		}
	}
}

func TestHubConfigLoadsFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "port: 8317\nrequests: {}\nhub:\n  prices:\n    gpt-6.1: {input: 1, output: 2}\n  subscriptions: {\"a.json\": 200}\n  session-roots: [\"/x\"]\n"
	if errWrite := os.WriteFile(path, []byte(body), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	cfg, errLoad := LoadConfig(path)
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	if cfg.Hub.Prices["gpt-6.1"].Output != 2 || cfg.Hub.Subscriptions["a.json"] != 200 || len(cfg.Hub.SessionRoots) != 1 {
		t.Fatalf("hub = %+v", cfg.Hub)
	}
}
