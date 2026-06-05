// Package cache is WaxSeal's token cache: in-memory, TTL-bounded, and
// size-capped, keyed by the full token key. It is generic over the value type so
// it stays decoupled from the public Token type, with the caller supplying each
// entry's authoritative expiry.
package cache

import (
	"sync"
	"time"
)

// Memory is a concurrency-safe cache with per-entry expiry and a bounded size.
type Memory[V any] struct {
	mu      sync.Mutex
	max     int
	entries map[string]entry[V]
	now     func() time.Time
}

type entry[V any] struct {
	value     V
	expiresAt time.Time
}

// New returns a cache holding at most maxEntries (default 256 if <= 0).
func New[V any](maxEntries int) *Memory[V] {
	if maxEntries <= 0 {
		maxEntries = 256
	}
	return &Memory[V]{max: maxEntries, entries: make(map[string]entry[V]), now: time.Now}
}

// Get returns the value if present and unexpired, deleting it on expiry.
func (m *Memory[V]) Get(key string) (V, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[key]
	if !ok {
		var zero V
		return zero, false
	}
	if m.expired(e) {
		delete(m.entries, key)
		var zero V
		return zero, false
	}
	return e.value, true
}

// Set stores value under key with the given expiry (zero = never expires).
// A full cache first sheds expired entries, then the soonest-to-expire.
func (m *Memory[V]) Set(key string, value V, expiresAt time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.entries[key]; !exists && len(m.entries) >= m.max {
		m.evictLocked()
	}
	m.entries[key] = entry[V]{value: value, expiresAt: expiresAt}
}

// Delete removes a key (e.g. on a 403/invalidate).
func (m *Memory[V]) Delete(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, key)
}

// Purge empties the cache (invalidate_caches).
func (m *Memory[V]) Purge() {
	m.mu.Lock()
	defer m.mu.Unlock()
	clear(m.entries)
}

// Len reports the current entry count (primarily for tests/metrics).
func (m *Memory[V]) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.entries)
}

func (m *Memory[V]) expired(e entry[V]) bool {
	return !e.expiresAt.IsZero() && !m.now().Before(e.expiresAt)
}

// evictLocked drops expired entries first; if still full, the soonest-to-expire
// (entries that never expire are evicted only as a last resort).
func (m *Memory[V]) evictLocked() {
	for k, e := range m.entries {
		if m.expired(e) {
			delete(m.entries, k)
		}
	}
	if len(m.entries) < m.max {
		return
	}
	var victim string
	var best time.Time
	for k, e := range m.entries {
		if e.expiresAt.IsZero() {
			continue
		}
		if victim == "" || e.expiresAt.Before(best) {
			victim, best = k, e.expiresAt
		}
	}
	if victim == "" {
		for k := range m.entries { // all non-expiring; evict any one
			victim = k
			break
		}
	}
	delete(m.entries, victim)
}
