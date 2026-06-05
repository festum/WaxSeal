package waxseal

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// newClientWith builds a client with the given options merged onto the test
// fakes. The caller owns Close (these tests reopen the same CacheDir, so the
// bbolt lock must be released between instances).
func newClientWith(t *testing.T, tr *fakeTransport, opts Options) *Client {
	t.Helper()
	opts.HTTPClient = &http.Client{Transport: tr}
	opts.engine = &fakeEngine{}
	c, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// TestClientPersistsTokensAcrossRestart checks that an opt-in disk cache is
// loaded by a fresh client for both backends.
func TestClientPersistsTokensAcrossRestart(t *testing.T) {
	for _, backend := range []string{"bbolt", "json"} {
		t.Run(backend, func(t *testing.T) {
			dir := t.TempDir()
			ctx := context.Background()
			// A stable, caller-chosen egress ID is required for disk persistence.
			req := Request{Scope: ScopeSession, VisitorData: sampleVisitorData, Egress: EgressSpec{ID: "egress-1"}}

			tr1 := &fakeTransport{genIT: fallbackGenIT}
			c1 := newClientWith(t, tr1, Options{CacheDir: dir, PersistTokens: true, DiskBackend: backend})
			tok, err := c1.Token(ctx, req)
			if err != nil {
				t.Fatalf("mint: %v", err)
			}
			if err := c1.Close(); err != nil {
				t.Fatalf("close c1: %v", err)
			}

			// A fresh client over the same directory reloads the token.
			tr2 := &fakeTransport{genIT: fallbackGenIT}
			c2 := newClientWith(t, tr2, Options{CacheDir: dir, PersistTokens: true, DiskBackend: backend})
			t.Cleanup(func() { _ = c2.Close() })
			tok2, err := c2.Token(ctx, req)
			if err != nil {
				t.Fatalf("post-restart token: %v", err)
			}
			if tok2.Value != tok.Value {
				t.Fatal("restart did not reuse the persisted token")
			}
			if got := tr2.createCount.Load(); got != 0 {
				t.Fatalf("re-Created despite a persisted token (Create=%d, want 0)", got)
			}
		})
	}
}

// TestClientPersistsServerDefaultEgress covers the normal server/CLI path. When
// WaxSeal owns the transport, an empty derived ID means direct egress, not an
// unlabeled caller-owned HTTP client.
func TestClientPersistsServerDefaultEgress(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	// ScopeOpaque is cacheable, and an all-empty egress derives ID "".
	req := Request{Scope: ScopeOpaque, Identifier: "vid-1"}

	mk := func(tr *fakeTransport) *Client {
		c, err := New(Options{
			EgressTransport: func(EgressSpec) (http.RoundTripper, error) { return tr, nil },
			engine:          &fakeEngine{},
			CacheDir:        dir,
			PersistTokens:   true,
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return c
	}

	tr1 := &fakeTransport{genIT: fallbackGenIT}
	c1 := mk(tr1)
	if _, err := c1.Token(ctx, req); err != nil {
		t.Fatalf("mint: %v", err)
	}
	_ = c1.Close()

	tr2 := &fakeTransport{genIT: fallbackGenIT}
	c2 := mk(tr2)
	t.Cleanup(func() { _ = c2.Close() })
	if _, err := c2.Token(ctx, req); err != nil {
		t.Fatalf("post-restart token: %v", err)
	}
	if got := tr2.createCount.Load(); got != 0 {
		t.Fatalf("Create=%d, want 0 (server default egress must persist when WaxSeal owns the transport)", got)
	}
}

// TestClientEmptyEgressIDNotPersisted checks the caller-owned HTTPClient path.
// Without a stable egress ID, token persistence must stay memory-only even when
// PersistTokens is true.
func TestClientEmptyEgressIDNotPersisted(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	req := Request{Scope: ScopeSession, VisitorData: sampleVisitorData} // no Egress.ID

	tr1 := &fakeTransport{genIT: fallbackGenIT}
	c1 := newClientWith(t, tr1, Options{CacheDir: dir, PersistTokens: true})
	if _, err := c1.Token(ctx, req); err != nil {
		t.Fatalf("mint: %v", err)
	}
	_ = c1.Close()

	tr2 := &fakeTransport{genIT: fallbackGenIT}
	c2 := newClientWith(t, tr2, Options{CacheDir: dir, PersistTokens: true})
	t.Cleanup(func() { _ = c2.Close() })
	if _, err := c2.Token(ctx, req); err != nil {
		t.Fatalf("post-restart token: %v", err)
	}
	if got := tr2.createCount.Load(); got != 1 {
		t.Fatalf("Create=%d, want 1 (empty egress ID must not persist to disk)", got)
	}
}

// TestClientNoPersistByDefault confirms token persistence is opt-in: without
// PersistTokens, a restart re-attests even when a CacheDir is set.
func TestClientNoPersistByDefault(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	// Non-empty egress ID, so the only reason it is not persisted is the default
	// PersistTokens=false (isolates this gate from the empty-ID gate).
	req := Request{Scope: ScopeSession, VisitorData: sampleVisitorData, Egress: EgressSpec{ID: "egress-1"}}

	tr1 := &fakeTransport{genIT: fallbackGenIT}
	c1 := newClientWith(t, tr1, Options{CacheDir: dir}) // PersistTokens defaults false
	if _, err := c1.Token(ctx, req); err != nil {
		t.Fatalf("mint: %v", err)
	}
	_ = c1.Close()

	tr2 := &fakeTransport{genIT: fallbackGenIT}
	c2 := newClientWith(t, tr2, Options{CacheDir: dir})
	t.Cleanup(func() { _ = c2.Close() })
	if _, err := c2.Token(ctx, req); err != nil {
		t.Fatalf("post-restart token: %v", err)
	}
	if got := tr2.createCount.Load(); got != 1 {
		t.Fatalf("Create=%d, want 1 (no token persistence by default)", got)
	}
}

// TestClientInvalidDiskBackendFatal confirms a misspelled backend with a
// configured CacheDir fails New (rather than silently falling back to
// memory-only and pretending persistence is on).
func TestClientInvalidDiskBackendFatal(t *testing.T) {
	_, err := New(Options{
		engine:      &fakeEngine{},
		CacheDir:    t.TempDir(),
		DiskBackend: "lmdb",
	})
	if err == nil {
		t.Fatal("New must reject an invalid disk backend, not fall back to memory-only")
	}
}

// TestClientInvalidDiskBackendIgnoredWithoutCacheDir confirms the backend value
// is irrelevant (and not validated) when persistence is not configured.
func TestClientInvalidDiskBackendIgnoredWithoutCacheDir(t *testing.T) {
	c, err := New(Options{engine: &fakeEngine{}, DiskBackend: "lmdb"}) // no CacheDir
	if err != nil {
		t.Fatalf("invalid backend with no CacheDir should be ignored: %v", err)
	}
	_ = c.Close()
}

// TestClientHydratedTokenRespectsLowerTTLCap checks that a token persisted under
// a looser cap is capped again when loaded by a stricter process.
func TestClientHydratedTokenRespectsLowerTTLCap(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	req := Request{Scope: ScopeSession, VisitorData: sampleVisitorData, Egress: EgressSpec{ID: "egress-1"}}

	// Client 1: uncapped, with the full ~1h GenerateIT lifetime persisted.
	tr1 := &fakeTransport{genIT: fallbackGenIT} // lifetime 3600s
	c1 := newClientWith(t, tr1, Options{CacheDir: dir, PersistTokens: true})
	tok1, err := c1.Token(ctx, req)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if time.Until(tok1.ExpiresAt) < 30*time.Minute {
		t.Fatalf("precondition: expected ~1h expiry uncapped, got %v", time.Until(tok1.ExpiresAt))
	}
	_ = c1.Close()

	// Client 2: a 1-minute cap. The hydrated token must be re-capped to ~now+1m.
	tr2 := &fakeTransport{genIT: fallbackGenIT}
	c2 := newClientWith(t, tr2, Options{CacheDir: dir, PersistTokens: true, CacheMaxTTL: time.Minute})
	t.Cleanup(func() { _ = c2.Close() })
	tok2, err := c2.Token(ctx, req)
	if err != nil {
		t.Fatalf("hydrated token: %v", err)
	}
	if got := tr2.createCount.Load(); got != 0 {
		t.Fatalf("Create=%d, want 0 (served from the hydrated cache)", got)
	}
	if d := time.Until(tok2.ExpiresAt); d > 2*time.Minute {
		t.Fatalf("hydrated token expiry %v exceeds the 1m cap (TTL not re-applied on load)", d)
	}
}

// TestClientPersistDisabledWithoutDir checks the in-process default: no CacheDir
// means memory-only storage.
func TestClientPersistDisabledWithoutDir(t *testing.T) {
	tr := &fakeTransport{genIT: fallbackGenIT}
	c := newClientWith(t, tr, Options{PersistTokens: true}) // PersistTokens but no CacheDir
	t.Cleanup(func() { _ = c.Close() })
	if _, err := c.Token(context.Background(), Request{Scope: ScopeSession, VisitorData: sampleVisitorData}); err != nil {
		t.Fatalf("mint: %v", err)
	}
	// Nothing to assert on disk; the point is that SaveToken on a Nop store is a
	// no-op and does not error or panic. A successful mint suffices.
}

// TestClientMetricsExposition checks the client's /metrics surface records cache
// hits/misses, attestations, and mints by kind.
func TestClientMetricsExposition(t *testing.T) {
	tr := &fakeTransport{genIT: integrityGenIT}
	c, _ := newTestClient(t, tr)
	req := Request{Scope: ScopeSession, VisitorData: sampleVisitorData}

	if _, err := c.Token(context.Background(), req); err != nil { // miss + attest + mint
		t.Fatalf("mint: %v", err)
	}
	if _, err := c.Token(context.Background(), req); err != nil { // cache hit
		t.Fatalf("cached: %v", err)
	}

	var sb strings.Builder
	if err := c.WriteMetrics(&sb); err != nil {
		t.Fatalf("WriteMetrics: %v", err)
	}
	out := sb.String()
	for _, want := range []string{
		"waxseal_cache_hits_total 1",
		"waxseal_cache_misses_total 1",
		"waxseal_attestations_total 1",
		`waxseal_mints_total{kind="integrity"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q\n---\n%s", want, out)
		}
	}
}
