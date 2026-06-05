// Package persist provides optional disk storage for token cache entries and
// circuit-breaker cooldowns. Token persistence is opt-in because tokens are
// sensitive. Cooldown persistence is non-sensitive and lets a restart honor an
// active backoff instead of immediately starting another attestation.
//
// The default backend is bbolt, which provides file locking so two processes
// sharing a directory do not corrupt the store. The JSON backend is intended for
// a single owning process. Both backends use a fingerprint made from the schema
// version and the qjs.wasm/bg_bundle hashes; a mismatch resets stored state.
// Store files are created 0600 because token records contain bearer
// capabilities.
//
// Open uses a short bbolt lock timeout. Callers can fall back to a memory-only
// Nop store for transient open failures, while ErrInvalidConfig marks errors
// that should be treated as configuration bugs.
package persist

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrInvalidConfig marks a misconfiguration, such as an unknown backend. It is
// distinct from transient disk-open failures so callers can treat it as fatal.
var ErrInvalidConfig = errors.New("persist: invalid configuration")

// schemaVersion is bumped when the on-disk layout changes. It is mixed into
// every fingerprint so a layout change invalidates older stores automatically.
const schemaVersion = "v1"

// DefaultOpenTimeout bounds the bbolt flock acquisition so a locked or slow
// directory degrades to memory-only instead of hanging startup.
const DefaultOpenTimeout = 2 * time.Second

// TokenRecord is one persisted token: opaque serialized bytes (the caller owns
// the encoding) plus the authoritative expiry used to drop stale entries on load.
type TokenRecord struct {
	Data      []byte
	ExpiresAt time.Time
}

// tokenBlob is the on-disk encoding of a TokenRecord, shared by both backends
// (encoding/json renders Data as base64). It is kept separate from the public
// TokenRecord so the wire format is decoupled from the API type.
type tokenBlob struct {
	Data      []byte    `json:"data"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// Store persists the token cache and breaker cooldowns. Implementations are safe
// for concurrent use. A Nop store satisfies the interface with no I/O so callers
// need not nil-check.
type Store interface {
	// LoadTokens returns every unexpired persisted token, keyed identically to
	// the in-memory cache.
	LoadTokens() (map[string]TokenRecord, error)
	// SaveToken writes (or overwrites) one token record.
	SaveToken(key string, rec TokenRecord) error
	// DeleteToken removes one token (e.g. on a 403 invalidation).
	DeleteToken(key string) error
	// PurgeTokens drops every persisted token (invalidate_caches).
	PurgeTokens() error

	// LoadBreakers returns persisted, still-active cooldowns (openUntil in the
	// future), keyed by minter key.
	LoadBreakers() (map[string]time.Time, error)
	// SaveBreaker records a breaker's open-until time; a zero or past time clears
	// the persisted cooldown.
	SaveBreaker(key string, openUntil time.Time) error

	// Close flushes and releases the store (and, for bbolt, the file lock).
	Close() error
}

// Options configure Open.
type Options struct {
	Dir         string        // directory for the store file; empty disables persistence
	Backend     string        // "bbolt" (default) or "json"
	AssetHashes []string      // qjs.wasm/bg_bundle hashes mixed into the fingerprint
	OpenTimeout time.Duration // bbolt flock timeout (default DefaultOpenTimeout)
	now         func() time.Time
}

// Open returns a Store for the configured backend. An empty Dir returns a Nop
// store with no error. A configured Dir that cannot be opened returns an error;
// callers that want startup to continue should log it and use Nop.
func Open(opts Options) (Store, error) {
	if opts.Dir == "" {
		return Nop(), nil
	}
	if opts.OpenTimeout <= 0 {
		opts.OpenTimeout = DefaultOpenTimeout
	}
	if opts.now == nil {
		opts.now = time.Now
	}
	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("persist: create dir %s: %w", opts.Dir, err)
	}
	fp := fingerprint(opts.AssetHashes)
	switch strings.ToLower(opts.Backend) {
	case "", "bbolt":
		return openBolt(filepath.Join(opts.Dir, "store.db"), fp, opts)
	case "json":
		return openJSON(filepath.Join(opts.Dir, "store.json"), fp, opts.now)
	default:
		return nil, fmt.Errorf("%w: unknown backend %q (want bbolt or json)", ErrInvalidConfig, opts.Backend)
	}
}

// fingerprint hashes the schema version with the supplied asset hashes. Stores
// whose persisted fingerprint differs are wiped on open.
func fingerprint(assetHashes []string) string {
	h := sha256.New()
	h.Write([]byte(schemaVersion))
	for _, a := range assetHashes {
		h.Write([]byte{0})
		h.Write([]byte(a))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// expired reports whether a record with this expiry should be dropped on load.
// A zero expiry never expires.
func expired(expiresAt, now time.Time) bool {
	return !expiresAt.IsZero() && !now.Before(expiresAt)
}
