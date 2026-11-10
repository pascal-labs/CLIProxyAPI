package config

import (
	"path/filepath"
	"sort"
	"strings"
)

// HubConfig configures the hub dashboard data: API-equivalent token prices,
// subscription costs per credential, and where local agent session files live.
type HubConfig struct {
	// Prices maps a model-id prefix to USD per million tokens. The longest
	// matching prefix wins. Entries override the built-in Claude defaults per
	// key; models without a match are reported as unpriced.
	Prices map[string]HubModelPrice `yaml:"prices,omitempty" json:"prices,omitempty"`
	// Subscriptions maps an auth file name or auth_index to its monthly USD cost.
	Subscriptions map[string]float64 `yaml:"subscriptions,omitempty" json:"subscriptions,omitempty"`
	// SessionRoots overrides the directories searched for local Claude Code
	// (<root>/projects/*/<id>.jsonl) and Codex (<root>/sessions/**/rollout-*-<id>.jsonl,
	// <root>/session_index.jsonl) session files. A leading "~" expands to $HOME.
	SessionRoots []string `yaml:"session-roots,omitempty" json:"session-roots,omitempty"`
}

// HubModelPrice is a per-million-token USD price list. CacheWrite5m and
// CacheWrite1h price prompt-cache writes by TTL; cache writes whose TTL is
// unknown are priced at the 5-minute rate.
type HubModelPrice struct {
	Input        float64 `yaml:"input" json:"input"`
	Output       float64 `yaml:"output" json:"output"`
	CacheRead    float64 `yaml:"cache_read" json:"cache_read"`
	CacheWrite5m float64 `yaml:"cache_write_5m" json:"cache_write_5m"`
	CacheWrite1h float64 `yaml:"cache_write_1h" json:"cache_write_1h"`
}

// DefaultHubPrices returns the built-in Claude prices (USD per million tokens).
// No OpenAI/Codex prices are built in: those models stay unpriced until
// configured.
func DefaultHubPrices() map[string]HubModelPrice {
	return map[string]HubModelPrice{
		"claude-fable-5-2":  {Input: 8, Output: 40, CacheRead: 0.20, CacheWrite5m: 10, CacheWrite1h: 16},
		"claude-fable-5-1":  {Input: 10, Output: 50, CacheRead: 0.25, CacheWrite5m: 12.5, CacheWrite1h: 20},
		"claude-opus-5-5":   {Input: 4, Output: 20, CacheRead: 0.20, CacheWrite5m: 5, CacheWrite1h: 8},
		"claude-opus-5":     {Input: 5, Output: 25, CacheRead: 0.50, CacheWrite5m: 6.25, CacheWrite1h: 10},
		"claude-sonnet-5-5": {Input: 2, Output: 10, CacheRead: 0.10, CacheWrite5m: 2.5, CacheWrite1h: 4},
		"claude-sonnet-5":   {Input: 2, Output: 10, CacheRead: 0.20, CacheWrite5m: 2.5, CacheWrite1h: 4},
		"claude-haiku-5-5":  {Input: 0.10, Output: 0.50, CacheRead: 0.01, CacheWrite5m: 0.125, CacheWrite1h: 0.20},
	}
}

// HubPriceTable resolves model ids to prices by longest prefix.
type HubPriceTable struct {
	prefixes []string
	prices   map[string]HubModelPrice
}

// PriceTable merges the built-in defaults with configured overrides.
func (c HubConfig) PriceTable() HubPriceTable {
	prices := DefaultHubPrices()
	for key, price := range c.Prices {
		key = normalizeHubModelID(key)
		if key == "" {
			continue
		}
		prices[key] = price
	}
	prefixes := make([]string, 0, len(prices))
	for key := range prices {
		prefixes = append(prefixes, key)
	}
	// Longest first so the first match is the most specific one.
	sort.Slice(prefixes, func(i, j int) bool {
		if len(prefixes[i]) != len(prefixes[j]) {
			return len(prefixes[i]) > len(prefixes[j])
		}
		return prefixes[i] < prefixes[j]
	})
	return HubPriceTable{prefixes: prefixes, prices: prices}
}

// Lookup returns the price for model and the prefix that matched.
func (t HubPriceTable) Lookup(model string) (HubModelPrice, string, bool) {
	model = normalizeHubModelID(model)
	if model == "" {
		return HubModelPrice{}, "", false
	}
	for _, prefix := range t.prefixes {
		if strings.HasPrefix(model, prefix) {
			return t.prices[prefix], prefix, true
		}
	}
	return HubModelPrice{}, "", false
}

// normalizeHubModelID lowercases a model id and drops a provider path prefix
// such as "anthropic/" or "models/".
func normalizeHubModelID(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	if index := strings.LastIndex(model, "/"); index >= 0 {
		model = model[index+1:]
	}
	return model
}

// SubscriptionFor returns the monthly USD cost configured for a credential,
// matched by auth file name (base name, case-insensitive) or auth_index.
func (c HubConfig) SubscriptionFor(fileName, authIndex string) (float64, bool) {
	if len(c.Subscriptions) == 0 {
		return 0, false
	}
	base := strings.ToLower(filepath.Base(strings.TrimSpace(fileName)))
	authIndex = strings.TrimSpace(authIndex)
	for key, monthly := range c.Subscriptions {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if (authIndex != "" && key == authIndex) || (base != "" && base != "." && strings.EqualFold(filepath.Base(key), base)) {
			return monthly, true
		}
	}
	return 0, false
}
