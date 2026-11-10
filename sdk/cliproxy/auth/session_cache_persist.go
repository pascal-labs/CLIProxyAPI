package auth

import (
	"sort"
	"strings"
	"time"
)

// SessionBindingRecord is one persisted session-affinity binding: every alias
// of one logical session, the credential it is bound to, and when the binding
// expires.
type SessionBindingRecord struct {
	AuthID    string    `json:"auth_id"`
	ExpiresAt time.Time `json:"expires_at"`
	Aliases   []string  `json:"aliases"`
	SessionID string    `json:"session_id,omitempty"`
	Provider  string    `json:"provider,omitempty"`
	Sequence  uint64    `json:"sequence,omitempty"`
}

// Snapshot returns the live (unexpired) bindings, oldest selection first.
func (c *SessionCache) Snapshot() []SessionBindingRecord {
	if c == nil {
		return nil
	}
	now := time.Now()
	c.mu.RLock()
	defer c.mu.RUnlock()
	records := make([]SessionBindingRecord, 0, len(c.groups))
	for _, group := range c.groups {
		if group.authID == "" || len(group.aliases) == 0 || !now.Before(group.expiresAt) {
			continue
		}
		records = append(records, SessionBindingRecord{
			AuthID:    group.authID,
			ExpiresAt: group.expiresAt.UTC().Round(0),
			Aliases:   append([]string(nil), group.aliases...),
			SessionID: group.binding.sessionID,
			Provider:  group.binding.provider,
			Sequence:  group.binding.sequence,
		})
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].Sequence != records[j].Sequence {
			return records[i].Sequence < records[j].Sequence
		}
		return records[i].Aliases[0] < records[j].Aliases[0]
	})
	return records
}

// Restore re-installs persisted bindings. Expired records are dropped, expiries
// are capped at the cache TTL, and a record whose alias is already bound live
// is skipped so current state always wins. It returns the number restored.
func (c *SessionCache) Restore(records []SessionBindingRecord) int {
	if c == nil || len(records) == 0 {
		return 0
	}
	ordered := append([]SessionBindingRecord(nil), records...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Sequence < ordered[j].Sequence })
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensureInitializedLocked()
	restored := 0
	var maxSequence uint64
	for _, record := range ordered {
		authID := strings.TrimSpace(record.AuthID)
		if authID == "" || !now.Before(record.ExpiresAt) {
			continue
		}
		aliases := compactSessionAliases(mergeSessionAliases(nil, record.Aliases...))
		if len(aliases) == 0 {
			continue
		}
		live := false
		for _, alias := range aliases {
			if entry, ok := c.entries[alias]; ok && now.Before(entry.expiresAt) {
				live = true
				break
			}
		}
		if live {
			continue
		}
		expiresAt := record.ExpiresAt
		if c.ttl > 0 && expiresAt.After(now.Add(c.ttl)) {
			expiresAt = now.Add(c.ttl)
		}
		binding := sessionBinding{sessionID: record.SessionID, provider: record.Provider, sequence: record.Sequence}
		if binding.sessionID == "" || binding.provider == "" {
			binding = sessionBinding{}
		}
		if binding.sequence > maxSequence {
			maxSequence = binding.sequence
		}
		c.replaceAliasGroupsLocked(authID, expiresAt, aliases, binding)
		restored++
	}
	if maxSequence > c.bindingSequence {
		c.bindingSequence = maxSequence
	}
	return restored
}

// SessionBindings returns the selector's live session bindings.
func (s *SessionAffinitySelector) SessionBindings() []SessionBindingRecord {
	if s == nil {
		return nil
	}
	return s.cache.Snapshot()
}

// RestoreSessionBindings re-installs persisted bindings (see SessionCache.Restore).
func (s *SessionAffinitySelector) RestoreSessionBindings(records []SessionBindingRecord) int {
	if s == nil {
		return 0
	}
	return s.cache.Restore(records)
}

// QuotaWindowSnapshot returns a copy of every credential's structured quota
// windows, keyed by auth ID.
func (m *Manager) QuotaWindowSnapshot() map[string][]QuotaWindow {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string][]QuotaWindow, len(m.auths))
	for id, auth := range m.auths {
		if auth == nil || len(auth.Quota.Windows) == 0 {
			continue
		}
		out[id] = cloneQuotaWindows(auth.Quota.Windows)
	}
	return out
}
