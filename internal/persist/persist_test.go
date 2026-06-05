package persist

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// openBackend opens a fresh store of the named backend in dir, failing the test
// on error. A frozen clock keeps expiry deterministic.
func openBackend(t *testing.T, dir, backend string, now time.Time, assets ...string) Store {
	t.Helper()
	s, err := Open(Options{
		Dir:         dir,
		Backend:     backend,
		AssetHashes: assets,
		now:         func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("Open(%s): %v", backend, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestStoreRoundTrip(t *testing.T) {
	for _, backend := range []string{"bbolt", "json"} {
		t.Run(backend, func(t *testing.T) {
			dir := t.TempDir()
			now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
			exp := now.Add(time.Hour)

			s := openBackend(t, dir, backend, now)
			if err := s.SaveToken("k1", TokenRecord{Data: []byte("tok-1"), ExpiresAt: exp}); err != nil {
				t.Fatalf("SaveToken: %v", err)
			}
			if err := s.SaveToken("k2", TokenRecord{Data: []byte("tok-2"), ExpiresAt: now.Add(-time.Minute)}); err != nil {
				t.Fatalf("SaveToken expired: %v", err)
			}
			if err := s.SaveBreaker("mkey", now.Add(30*time.Second)); err != nil {
				t.Fatalf("SaveBreaker: %v", err)
			}
			if err := s.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			// Reopen with the same fingerprint: unexpired token + active cooldown
			// survive; the expired token is dropped on load.
			s2 := openBackend(t, dir, backend, now)
			toks, err := s2.LoadTokens()
			if err != nil {
				t.Fatalf("LoadTokens: %v", err)
			}
			if len(toks) != 1 {
				t.Fatalf("LoadTokens = %d entries, want 1 (expired dropped): %v", len(toks), keysOf(toks))
			}
			if got := string(toks["k1"].Data); got != "tok-1" {
				t.Fatalf("k1 data = %q, want tok-1", got)
			}
			if !toks["k1"].ExpiresAt.Equal(exp) {
				t.Fatalf("k1 expiry = %v, want %v", toks["k1"].ExpiresAt, exp)
			}
			brks, err := s2.LoadBreakers()
			if err != nil {
				t.Fatalf("LoadBreakers: %v", err)
			}
			if len(brks) != 1 || !brks["mkey"].Equal(now.Add(30*time.Second)) {
				t.Fatalf("LoadBreakers = %v, want mkey at +30s", brks)
			}
		})
	}
}

func TestDeleteAndPurge(t *testing.T) {
	for _, backend := range []string{"bbolt", "json"} {
		t.Run(backend, func(t *testing.T) {
			dir := t.TempDir()
			now := time.Now()
			s := openBackend(t, dir, backend, now)
			exp := now.Add(time.Hour)
			_ = s.SaveToken("a", TokenRecord{Data: []byte("1"), ExpiresAt: exp})
			_ = s.SaveToken("b", TokenRecord{Data: []byte("2"), ExpiresAt: exp})

			if err := s.DeleteToken("a"); err != nil {
				t.Fatalf("DeleteToken: %v", err)
			}
			toks, _ := s.LoadTokens()
			if _, ok := toks["a"]; ok {
				t.Fatal("DeleteToken left a behind")
			}
			if _, ok := toks["b"]; !ok {
				t.Fatal("DeleteToken removed the wrong key")
			}

			if err := s.PurgeTokens(); err != nil {
				t.Fatalf("PurgeTokens: %v", err)
			}
			toks, _ = s.LoadTokens()
			if len(toks) != 0 {
				t.Fatalf("PurgeTokens left %d entries", len(toks))
			}
		})
	}
}

func TestSaveBreakerClears(t *testing.T) {
	for _, backend := range []string{"bbolt", "json"} {
		t.Run(backend, func(t *testing.T) {
			dir := t.TempDir()
			now := time.Now()
			s := openBackend(t, dir, backend, now)
			_ = s.SaveBreaker("k", now.Add(time.Minute))
			// A success clears the cooldown; persisting a zero time removes it.
			if err := s.SaveBreaker("k", time.Time{}); err != nil {
				t.Fatalf("SaveBreaker clear: %v", err)
			}
			brks, _ := s.LoadBreakers()
			if len(brks) != 0 {
				t.Fatalf("cleared cooldown still present: %v", brks)
			}
			// A past time is also treated as cleared, never loaded as active.
			_ = s.SaveBreaker("k2", now.Add(-time.Minute))
			brks, _ = s.LoadBreakers()
			if len(brks) != 0 {
				t.Fatalf("elapsed cooldown loaded as active: %v", brks)
			}
		})
	}
}

// TestFingerprintChurn checks that changed asset hashes reset the store, while
// unchanged hashes preserve it.
func TestFingerprintChurn(t *testing.T) {
	for _, backend := range []string{"bbolt", "json"} {
		t.Run(backend, func(t *testing.T) {
			dir := t.TempDir()
			now := time.Now()
			exp := now.Add(time.Hour)

			s := openBackend(t, dir, backend, now, "wasm-A", "bundle-A")
			_ = s.SaveToken("k", TokenRecord{Data: []byte("v"), ExpiresAt: exp})
			_ = s.SaveBreaker("mk", now.Add(time.Minute))
			_ = s.Close()

			// Same fingerprint: state survives.
			same := openBackend(t, dir, backend, now, "wasm-A", "bundle-A")
			if toks, _ := same.LoadTokens(); len(toks) != 1 {
				t.Fatalf("same fingerprint dropped tokens: %d", len(toks))
			}
			_ = same.Close()

			// New asset hash (a qjs.wasm upgrade): store is wiped.
			upgraded := openBackend(t, dir, backend, now, "wasm-B", "bundle-A")
			if toks, _ := upgraded.LoadTokens(); len(toks) != 0 {
				t.Fatalf("fingerprint change did not wipe tokens: %d", len(toks))
			}
			if brks, _ := upgraded.LoadBreakers(); len(brks) != 0 {
				t.Fatalf("fingerprint change did not wipe breakers: %d", len(brks))
			}
		})
	}
}

// TestBoltLockFailSoft confirms a second opener of the same bbolt file times out
// (rather than hanging), so the caller can fall back to memory-only.
func TestBoltLockFailSoft(t *testing.T) {
	dir := t.TempDir()
	first, err := Open(Options{Dir: dir, Backend: "bbolt"})
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	defer first.Close()

	start := time.Now()
	_, err = Open(Options{Dir: dir, Backend: "bbolt", OpenTimeout: 200 * time.Millisecond})
	if err == nil {
		t.Fatal("second open of a locked bbolt file unexpectedly succeeded")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("second open took %v; the timeout did not bound it", elapsed)
	}
}

func TestEmptyDirIsNop(t *testing.T) {
	s, err := Open(Options{Dir: ""})
	if err != nil {
		t.Fatalf("Open(empty dir): %v", err)
	}
	if _, ok := s.(nopStore); !ok {
		t.Fatalf("empty dir returned %T, want nopStore", s)
	}
	if err := s.SaveToken("k", TokenRecord{Data: []byte("v")}); err != nil {
		t.Fatalf("nop SaveToken: %v", err)
	}
	toks, _ := s.LoadTokens()
	if len(toks) != 0 {
		t.Fatalf("nop store returned %d tokens", len(toks))
	}
}

func TestUnknownBackend(t *testing.T) {
	_, err := Open(Options{Dir: t.TempDir(), Backend: "lmdb"})
	if err == nil {
		t.Fatal("unknown backend should error")
	}
	// A misconfiguration must be distinguishable from a transient disk failure so
	// callers can treat it as fatal rather than falling back.
	if !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("unknown backend error = %v, want errors.Is ErrInvalidConfig", err)
	}
}

// TestJSONFilePermissions verifies the JSON store is written 0600 (it can hold
// sensitive token capabilities).
func TestJSONFilePermissions(t *testing.T) {
	dir := t.TempDir()
	s := openBackend(t, dir, "json", time.Now())
	_ = s.SaveToken("k", TokenRecord{Data: []byte("v"), ExpiresAt: time.Now().Add(time.Hour)})
	info, err := os.Stat(filepath.Join(dir, "store.json"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("store.json perm = %o, want 600", perm)
	}
}

func keysOf(m map[string]TokenRecord) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
