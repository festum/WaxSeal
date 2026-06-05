package session

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colespringer/waxseal/internal/botguard"
	"github.com/colespringer/waxseal/internal/httpx"
	"github.com/colespringer/waxseal/internal/jsruntime"
)

// Test fakes.

// fakeEngine hands out fakeRuntimes and counts how many were created.
type fakeEngine struct {
	created atomic.Int32
	live    atomic.Int32
	poison  bool // produced runtimes poison themselves on mint
}

func (e *fakeEngine) NewRuntime(ctx context.Context) (jsruntime.Runtime, error) {
	e.created.Add(1)
	e.live.Add(1)
	return &fakeRuntime{eng: e, poison: e.poison}, nil
}
func (e *fakeEngine) Close(ctx context.Context) error { return nil }

type fakeRuntime struct {
	eng       *fakeEngine
	poison    bool
	panicMint bool // panic inside Call("mint") to exercise lock panic-safety
	mintCalls atomic.Int32
	poisoned  atomic.Bool
	closed    atomic.Bool
}

func (r *fakeRuntime) Eval(ctx context.Context, src string) (json.RawMessage, error) {
	return json.RawMessage("null"), nil
}
func (r *fakeRuntime) Call(ctx context.Context, name string, args ...any) (json.RawMessage, error) {
	switch name {
	case "runBotguard":
		return json.Marshal("FAKE_BOTGUARD_RESPONSE")
	case "newMinter":
		return json.RawMessage("true"), nil
	case "mint":
		r.mintCalls.Add(1)
		if r.panicMint {
			panic("simulated mint panic")
		}
		if r.poison {
			r.poisoned.Store(true)
			return nil, jsruntime.WrapBoundary("mint", fmt.Errorf("boom"))
		}
		id, _ := args[0].(string)
		return json.Marshal(validToken("mint-" + id))
	}
	return json.RawMessage("null"), nil
}
func (r *fakeRuntime) SetWatchdog(time.Duration) {}
func (r *fakeRuntime) Poisoned() bool            { return r.poisoned.Load() }
func (r *fakeRuntime) Close(ctx context.Context) error {
	if !r.closed.Swap(true) {
		r.eng.live.Add(-1)
	}
	return nil
}

// fakeTransport answers the Create / GenerateIT endpoints with canned bodies.
// attGetCount tracks InnerTube att/get hits; by default att/get 404s so callers
// that disable InnerTube (most minter-mechanics tests) stay on the Create path.
type fakeTransport struct {
	createCount, genITCount, attGetCount atomic.Int32
	genITBody                            func() (int, string) // status, body
	serveAttGet                          bool                 // answer att/get + interpreter fetch
}

func (f *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	mk := func(status int, body string) *http.Response {
		return &http.Response{
			StatusCode: status,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
			Request:    req,
		}
	}
	switch {
	case strings.HasSuffix(req.URL.Path, "/att/get"):
		f.attGetCount.Add(1)
		if !f.serveAttGet {
			return mk(404, ""), nil
		}
		// bgChallenge points at an allowlisted interpreter URL (fetched below).
		return mk(200, `{"bgChallenge":{"interpreterUrl":{"privateDoNotAccessOrElseTrustedResourceUrlWrappedValue":"//www.google.com/js/bg.js"},"program":"PROGRAM","globalName":"globalName"}}`), nil
	case strings.HasSuffix(req.URL.Path, "/js/bg.js"):
		return mk(200, "VAR_GLOBAL=1;"), nil // the interpreter JS
	case strings.HasSuffix(req.URL.Path, "/Create"):
		f.createCount.Add(1)
		// Structured family: arr[0] is the challenge array with INLINE interpreter.
		return mk(200, `[["v",["VAR_GLOBAL=1;"],[],0,"PROGRAM","globalName"]]`), nil
	case strings.HasSuffix(req.URL.Path, "/GenerateIT"):
		f.genITCount.Add(1)
		status, body := f.genITBody()
		return mk(status, body), nil
	}
	return mk(404, ""), nil
}

func validToken(payload string) string {
	// Protobuf field 6 (wire type 2), length-delimited payload.
	raw := append([]byte{0x32, byte(len(payload))}, []byte(payload)...)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func fallbackBody() (int, string) {
	return 200, fmt.Sprintf(`[null,3600,null,%q]`, validToken("fallback"))
}
func integrityBody() (int, string) {
	return 200, fmt.Sprintf(`[%q,3600,1800,%q]`, "INTEGRITY_TOKEN", validToken("fallback"))
}

func newTestManager(eng *fakeEngine, opts Options) *Manager {
	if opts.now == nil {
		opts.now = time.Now
	}
	return New(eng, opts)
}

func clientWith(tr *fakeTransport) *httpx.Client {
	c := httpx.New(&http.Client{Transport: tr})
	c.MaxRetries = 0 // deterministic hit counts in tests
	return c
}

// req builds a minter-mechanics request. InnerTube is disabled so the challenge
// comes deterministically from Create; the challenge-source chain itself is
// covered by TestChallengeSourcePriority.
func req(key, id string, hc *httpx.Client) Request {
	return Request{Key: key, Identifier: id, AttestationUA: "UA", Client: hc, DisableInnertube: true}
}

// Manager behavior.

func TestFallbackColdThenWarmReuse(t *testing.T) {
	eng := &fakeEngine{}
	tr := &fakeTransport{genITBody: fallbackBody}
	m := newTestManager(eng, Options{})
	hc := clientWith(tr)

	r1, err := m.Token(context.Background(), req("k", "ignored", hc))
	if err != nil {
		t.Fatalf("cold: %v", err)
	}
	if r1.Kind != KindFallback {
		t.Fatalf("kind = %s, want fallback", r1.Kind)
	}
	if _, verr := validateField6(r1.Token); verr != nil {
		t.Fatalf("served token invalid: %v", verr)
	}

	r2, err := m.Token(context.Background(), req("k", "ignored2", hc))
	if err != nil {
		t.Fatalf("warm: %v", err)
	}
	if r2.Token != r1.Token {
		t.Fatal("fallback token should be identical across identifiers")
	}
	if got := tr.createCount.Load(); got != 1 {
		t.Fatalf("Create called %d times, want 1 (warm reuse)", got)
	}
	// Fallback path keeps no warm runtime: the throwaway must be closed.
	if got := eng.live.Load(); got != 0 {
		t.Fatalf("%d runtimes still live; fallback path must close the throwaway", got)
	}
}

func TestSingleflightCollapsesColdHerd(t *testing.T) {
	eng := &fakeEngine{}
	tr := &fakeTransport{genITBody: fallbackBody}
	m := newTestManager(eng, Options{})
	hc := clientWith(tr)

	const n = 24
	var wg sync.WaitGroup
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			if _, err := m.Token(context.Background(), req("k", "x", hc)); err != nil {
				t.Errorf("token: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := tr.createCount.Load(); got != 1 {
		t.Fatalf("Create called %d times under a herd, want 1 (singleflight)", got)
	}
	if got := eng.created.Load(); got != 1 {
		t.Fatalf("created %d runtimes, want 1", got)
	}
}

func TestIntegrityWarmMintsPerIdentifier(t *testing.T) {
	eng := &fakeEngine{}
	tr := &fakeTransport{genITBody: integrityBody}
	m := newTestManager(eng, Options{})
	hc := clientWith(tr)

	r1, err := m.Token(context.Background(), req("k", "id1", hc))
	if err != nil {
		t.Fatalf("mint id1: %v", err)
	}
	if r1.Kind != KindIntegrity {
		t.Fatalf("kind = %s, want integrity", r1.Kind)
	}
	r2, err := m.Token(context.Background(), req("k", "id2", hc))
	if err != nil {
		t.Fatalf("mint id2: %v", err)
	}
	if r1.Token == r2.Token {
		t.Fatal("distinct identifiers must mint distinct tokens")
	}
	if got := tr.createCount.Load(); got != 1 {
		t.Fatalf("Create called %d times, want 1 (one warm minter)", got)
	}
	// Warm runtime stays live for the integrity path.
	if got := eng.live.Load(); got != 1 {
		t.Fatalf("%d runtimes live, want 1 (warm minter retained)", got)
	}
}

func TestPoisonOnMintEvicts(t *testing.T) {
	eng := &fakeEngine{poison: true}
	tr := &fakeTransport{genITBody: integrityBody}
	m := newTestManager(eng, Options{})
	hc := clientWith(tr)

	if _, err := m.Token(context.Background(), req("k", "id1", hc)); err == nil {
		t.Fatal("expected mint error from poisoned runtime")
	}
	// The poisoned entry must be evicted and its runtime closed.
	if got := eng.live.Load(); got != 0 {
		t.Fatalf("%d runtimes live after poison, want 0 (evicted)", got)
	}
	if m.getEntry("k") != nil {
		t.Fatal("poisoned entry not evicted")
	}
}

func TestForceNewRebuilds(t *testing.T) {
	eng := &fakeEngine{}
	tr := &fakeTransport{genITBody: fallbackBody}
	m := newTestManager(eng, Options{})
	hc := clientWith(tr)

	if _, err := m.Token(context.Background(), req("k", "x", hc)); err != nil {
		t.Fatalf("cold: %v", err)
	}
	r := req("k", "x", hc)
	r.ForceNew = true
	if _, err := m.Token(context.Background(), r); err != nil {
		t.Fatalf("forced: %v", err)
	}
	if got := tr.createCount.Load(); got != 2 {
		t.Fatalf("Create called %d times, want 2 (ForceNew re-attests)", got)
	}
}

func TestBreakerOpensAfterFailures(t *testing.T) {
	eng := &fakeEngine{}
	tr := &fakeTransport{genITBody: func() (int, string) { return 500, "" }}
	m := newTestManager(eng, Options{BreakerThreshold: 2, BreakerCooldown: time.Minute})
	hc := clientWith(tr)

	for i := range 2 {
		if _, err := m.Token(context.Background(), req("k", "x", hc)); err == nil {
			t.Fatalf("attempt %d: expected GenerateIT failure", i)
		}
	}
	// Third call should be short-circuited by the open breaker (no new attest).
	before := tr.createCount.Load()
	_, err := m.Token(context.Background(), req("k", "x", hc))
	if err == nil {
		t.Fatal("expected breaker-open error")
	}
	if after := tr.createCount.Load(); after != before {
		t.Fatalf("breaker open but still attested (%d -> %d)", before, after)
	}
}

func TestExpiryRebuilds(t *testing.T) {
	eng := &fakeEngine{}
	tr := &fakeTransport{genITBody: fallbackBody}
	clock := time.Now()
	m := newTestManager(eng, Options{now: func() time.Time { return clock }})
	hc := clientWith(tr)

	if _, err := m.Token(context.Background(), req("k", "x", hc)); err != nil {
		t.Fatalf("cold: %v", err)
	}
	// GenerateIT lifetime is 3600s; jump well past it.
	clock = clock.Add(2 * time.Hour)
	if _, err := m.Token(context.Background(), req("k", "x", hc)); err != nil {
		t.Fatalf("post-expiry: %v", err)
	}
	if got := tr.createCount.Load(); got != 2 {
		t.Fatalf("Create called %d times, want 2 (rebuild after expiry)", got)
	}
}

// Regression: a stale serve whose entry was replaced by a concurrent
// refresh/ForceNew must not evict the fresh replacement by key. The rt==nil and
// post-mint-poison branches use evictEntry(key, e), which guards on the current
// entry, so a racing request cannot discard a good warm minter.
func TestEvictEntryKeepsRefreshedReplacement(t *testing.T) {
	eng := &fakeEngine{}
	m := newTestManager(eng, Options{})

	rt1, _ := eng.NewRuntime(context.Background())
	rt2, _ := eng.NewRuntime(context.Background())
	e1 := &entry{key: "k", kind: KindIntegrity, rt: rt1}
	e2 := &entry{key: "k", kind: KindIntegrity, rt: rt2}

	m.putEntry("k", e1)
	m.putEntry("k", e2) // refresh swaps in e2; closeRuntime(e1, e2) closes rt1
	if got := eng.live.Load(); got != 1 {
		t.Fatalf("after swap: %d runtimes live, want 1 (rt1 closed, rt2 kept)", got)
	}

	// A stale serve holding old e1 tries to evict; e1 is no longer current,
	// so the fresh e2 (and its live runtime) must survive untouched.
	m.evictEntry("k", e1)
	if got := m.getEntry("k"); got != e2 {
		t.Fatal("evictEntry(stale e1) discarded the fresh replacement e2")
	}
	if got := eng.live.Load(); got != 1 {
		t.Fatalf("fresh runtime closed by stale eviction: %d live, want 1", got)
	}

	// Evicting the current entry does remove it and close its runtime.
	m.evictEntry("k", e2)
	if got := m.getEntry("k"); got != nil {
		t.Fatal("evictEntry(current e2) did not remove it")
	}
	if got := eng.live.Load(); got != 0 {
		t.Fatalf("current runtime not closed on eviction: %d live, want 0", got)
	}
}

// A panic inside botguard.Mint must not leave the per-entry lock held:
// mintFromEntry defers the Unlock. Otherwise closeRuntime / eviction, which
// re-lock e.mu) would deadlock and wedge the whole minter key.
func TestMintFromEntryReleasesLockOnPanic(t *testing.T) {
	eng := &fakeEngine{}
	m := newTestManager(eng, Options{})
	rt, _ := eng.NewRuntime(context.Background())
	rt.(*fakeRuntime).panicMint = true
	e := &entry{key: "k", kind: KindIntegrity, rt: rt}

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected the mint panic to propagate")
			}
		}()
		_, _, _ = m.mintFromEntry(context.Background(), e, "id")
	}()

	// The deferred Unlock must have run despite the panic. TryLock confirms the
	// lock is free without risking a test-hanging deadlock.
	if !e.mu.TryLock() {
		t.Fatal("e.mu still held after a panicking mint; deferred Unlock did not run")
	}
	e.mu.Unlock()
}

// TestChallengeSourcePriority exercises the resolveChallenge chain: a
// caller-provided challenge wins outright; otherwise InnerTube att/get is used;
// and an att/get failure falls through to Create.
func TestChallengeSourcePriority(t *testing.T) {
	t.Run("caller challenge wins (no challenge HTTP)", func(t *testing.T) {
		eng := &fakeEngine{}
		tr := &fakeTransport{genITBody: fallbackBody}
		m := newTestManager(eng, Options{})
		hc := clientWith(tr)

		r := req("k", "x", hc)
		r.DisableInnertube = false // would use att/get, but Challenge preempts it
		r.Challenge = &botguard.Challenge{InterpreterJS: "VAR=1;", Program: "P", GlobalName: "g"}
		if _, err := m.Token(context.Background(), r); err != nil {
			t.Fatalf("token: %v", err)
		}
		if got := tr.attGetCount.Load(); got != 0 {
			t.Errorf("att/get hit %d times despite a caller challenge", got)
		}
		if got := tr.createCount.Load(); got != 0 {
			t.Errorf("Create hit %d times despite a caller challenge", got)
		}
	})

	t.Run("att/get used when enabled", func(t *testing.T) {
		eng := &fakeEngine{}
		tr := &fakeTransport{genITBody: fallbackBody, serveAttGet: true}
		m := newTestManager(eng, Options{})
		hc := clientWith(tr)

		r := Request{Key: "k", Identifier: "x", AttestationUA: "UA", Client: hc}
		if _, err := m.Token(context.Background(), r); err != nil {
			t.Fatalf("token: %v", err)
		}
		if got := tr.attGetCount.Load(); got != 1 {
			t.Errorf("att/get hit %d times, want 1", got)
		}
		if got := tr.createCount.Load(); got != 0 {
			t.Errorf("Create hit %d times, want 0 (att/get succeeded)", got)
		}
	})

	t.Run("att/get failure falls through to Create", func(t *testing.T) {
		eng := &fakeEngine{}
		tr := &fakeTransport{genITBody: fallbackBody, serveAttGet: false} // att/get 404s
		m := newTestManager(eng, Options{})
		hc := clientWith(tr)

		r := Request{Key: "k", Identifier: "x", AttestationUA: "UA", Client: hc}
		if _, err := m.Token(context.Background(), r); err != nil {
			t.Fatalf("token: %v", err)
		}
		if got := tr.attGetCount.Load(); got != 1 {
			t.Errorf("att/get hit %d times, want 1 (attempted)", got)
		}
		if got := tr.createCount.Load(); got != 1 {
			t.Errorf("Create hit %d times, want 1 (fallback)", got)
		}
	})
}

// validateField6 mirrors the production check so the test asserts served tokens
// are usable without importing botguard's internals beyond the public helper.
func validateField6(token string) ([]byte, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(token, "="))
	if err != nil {
		return nil, err
	}
	if len(raw) < 2 || raw[0] != 0x32 {
		return nil, fmt.Errorf("no field 6")
	}
	return raw, nil
}
