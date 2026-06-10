package minter

import (
	"context"
	"fmt"
	"github.com/colespringer/waxseal/internal/browser"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSession is an in-memory minterSession for testing the Minter's reliability
// logic without a browser.
type fakeSession struct {
	mint   func(identifier string) (browser.MintResult, error)
	id     browser.Identity // zero value reports a default visitor_data
	closed atomic.Bool
}

func (f *fakeSession) Mint(_ context.Context, id string) (browser.MintResult, error) {
	return f.mint(id)
}
func (f *fakeSession) AttestKind() string { return "integrity" }
func (f *fakeSession) Identity() browser.Identity {
	if f.id.VisitorData == "" {
		return browser.Identity{VisitorData: "vd"}
	}
	return f.id
}
func (f *fakeSession) BrowserCookies() []*http.Cookie { return nil }
func (f *fakeSession) Close()                         { f.closed.Store(true) }

// newTestMinter returns a Minter whose launcher records each created session and
// uses the supplied per-mint behaviour.
func newTestMinter(mint func(id string) (browser.MintResult, error)) (*Minter, *int64, *[]*fakeSession, *sync.Mutex) {
	var launches int64
	var sessions []*fakeSession
	var smu sync.Mutex
	m := NewMinter("v", browser.Options{})
	m.launch = func(context.Context) (minterSession, error) {
		atomic.AddInt64(&launches, 1)
		fs := &fakeSession{mint: mint}
		smu.Lock()
		sessions = append(sessions, fs)
		smu.Unlock()
		return fs, nil
	}
	return m, &launches, &sessions, &smu
}

// TestMinterSingleFlightAttestation: many concurrent callers during one launch
// coalesce into a single attestation.
func TestMinterSingleFlightAttestation(t *testing.T) {
	var launches int64
	var once sync.Once
	launchStarted := make(chan struct{})
	release := make(chan struct{})
	m := NewMinter("v", browser.Options{})
	m.launch = func(context.Context) (minterSession, error) {
		atomic.AddInt64(&launches, 1)
		once.Do(func() { close(launchStarted) })
		<-release // hold the launch open so concurrent callers pile up
		return &fakeSession{mint: func(string) (browser.MintResult, error) { return browser.MintResult{}, nil }}, nil
	}

	ctx := context.Background()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = m.Warm(ctx) }()
	<-launchStarted // one launch is now in flight
	for i := 0; i < 9; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = m.Warm(ctx) }()
	}
	time.Sleep(50 * time.Millisecond) // let the 9 reach the single-flight wait
	close(release)
	wg.Wait()

	if got := atomic.LoadInt64(&launches); got != 1 {
		t.Fatalf("launches = %d, want 1 (single-flight should coalesce)", got)
	}
	if got := m.metrics.Attestations.Load(); got != 1 {
		t.Errorf("attestations metric = %d, want 1", got)
	}
}

// TestMinterCacheNoReattest: a repeat request for the same (scope, binding) is
// served from cache, with no second mint and no second attestation (the
// "a 403-driven retry must not re-attest" guarantee). A new binding mints again
// on the same session (still one attestation).
func TestMinterCacheNoReattest(t *testing.T) {
	var mints int64
	m, launches, _, _ := newTestMinter(func(id string) (browser.MintResult, error) {
		atomic.AddInt64(&mints, 1)
		return browser.MintResult{Kind: "integrity", Token: "tok-" + id, TokenLen: 4, Identifier: id, Lifetime: 3600}, nil
	})
	ctx := context.Background()

	r1, c1, err := m.Mint(ctx, "gvs", "vd")
	if err != nil || c1 {
		t.Fatalf("first mint: cached=%v err=%v, want cached=false", c1, err)
	}
	r2, c2, err := m.Mint(ctx, "gvs", "vd")
	if err != nil || !c2 {
		t.Fatalf("repeat mint: cached=%v err=%v, want cached=true", c2, err)
	}
	if r1.Token != r2.Token {
		t.Errorf("cached token = %q, want same as first %q", r2.Token, r1.Token)
	}
	if got := atomic.LoadInt64(&mints); got != 1 {
		t.Errorf("mints = %d, want 1 (second served from cache)", got)
	}
	// A different binding mints again, but still on the one attestation.
	if _, c3, _ := m.Mint(ctx, "player", "vid"); c3 {
		t.Errorf("new binding should not be a cache hit")
	}
	if got := atomic.LoadInt64(&mints); got != 2 {
		t.Errorf("mints = %d, want 2", got)
	}
	if got := atomic.LoadInt64(launches); got != 1 {
		t.Errorf("launches = %d, want 1 (never re-attest for cache/new-binding)", got)
	}
}

// TestMinterMaxAgeRecycle: a session older than maxAge is proactively retired and
// relaunched on the next ensure.
func TestMinterMaxAgeRecycle(t *testing.T) {
	m, launches, sessions, smu := newTestMinter(func(string) (browser.MintResult, error) {
		return browser.MintResult{Kind: "integrity", Token: "t", Lifetime: 3600}, nil
	})
	m.maxAge = time.Millisecond
	ctx := context.Background()

	if err := m.Warm(ctx); err != nil { // gen 1
		t.Fatalf("warm: %v", err)
	}
	time.Sleep(5 * time.Millisecond) // exceed maxAge
	if err := m.Warm(ctx); err != nil {
		t.Fatalf("warm 2: %v", err)
	}
	if got := atomic.LoadInt64(launches); got != 2 {
		t.Errorf("launches = %d, want 2 (stale session recycled)", got)
	}
	smu.Lock()
	defer smu.Unlock()
	if !(*sessions)[0].closed.Load() {
		t.Errorf("recycled session should be closed")
	}
}

// TestMinterEscalationLadder: a mint that fails twice triggers one in-place retry
// then a relaunch (re-attest) on a fresh session; the old session is closed.
func TestMinterEscalationLadder(t *testing.T) {
	var attempt int64
	m, launches, sessions, smu := newTestMinter(func(string) (browser.MintResult, error) {
		if n := atomic.AddInt64(&attempt, 1); n <= 2 {
			return browser.MintResult{}, fmt.Errorf("transient failure %d", n)
		}
		return browser.MintResult{Kind: "integrity", Token: "ok", Lifetime: 3600}, nil
	})
	ctx := context.Background()

	r, cached, err := m.Mint(ctx, "gvs", "vd")
	if err != nil {
		t.Fatalf("mint after escalation: %v", err)
	}
	if cached || r.Token != "ok" {
		t.Fatalf("got token=%q cached=%v, want token=ok cached=false", r.Token, cached)
	}
	if got := atomic.LoadInt64(launches); got != 2 {
		t.Errorf("launches = %d, want 2 (initial + one relaunch)", got)
	}
	if got := m.metrics.Escalations.Load(); got != 1 {
		t.Errorf("escalations = %d, want 1", got)
	}
	smu.Lock()
	defer smu.Unlock()
	if len(*sessions) != 2 {
		t.Fatalf("sessions created = %d, want 2", len(*sessions))
	}
	if !(*sessions)[0].closed.Load() {
		t.Errorf("first (failed) session should be closed after escalation")
	}
	if (*sessions)[1].closed.Load() {
		t.Errorf("second (current) session should be live")
	}
}

// TestMinterCrashKeepsCacheThenRelaunchInvalidates: retiring a session (the path
// a crash takes) does not by itself force a re-attest; already-minted tokens
// outlive the browser, so a cached binding is still served (the per-IP-scarce
// attestation is preserved). A cache-missing request relaunches (bumping the
// generation), which then invalidates the old generation's cached tokens.
func TestMinterCrashKeepsCacheThenRelaunchInvalidates(t *testing.T) {
	var mints int64
	m, launches, sessions, smu := newTestMinter(func(id string) (browser.MintResult, error) {
		n := atomic.AddInt64(&mints, 1)
		return browser.MintResult{Kind: "integrity", Token: fmt.Sprintf("tok%d", n), Lifetime: 3600}, nil
	})
	ctx := context.Background()

	if _, _, err := m.Mint(ctx, "gvs", "vd"); err != nil { // gen 1, cached
		t.Fatalf("first mint: %v", err)
	}
	_, gen, err := m.ensure(ctx)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	m.retire(gen, "simulated crash") // browser dies; generation unchanged

	smu.Lock()
	firstClosed := (*sessions)[0].closed.Load()
	smu.Unlock()
	if !firstClosed {
		t.Errorf("retired session should be closed")
	}

	// The already-minted token is still valid → served from cache, no relaunch.
	if _, cached, _ := m.Mint(ctx, "gvs", "vd"); !cached {
		t.Errorf("cached token should survive a crash (no needless re-attest)")
	}
	if got := atomic.LoadInt64(launches); got != 1 {
		t.Errorf("launches = %d, want 1 (a crash alone must not re-attest)", got)
	}

	// A new binding misses the cache → relaunch (gen 2).
	if _, cached, _ := m.Mint(ctx, "player", "vid2"); cached {
		t.Errorf("new binding should not be cached")
	}
	if got := atomic.LoadInt64(launches); got != 2 {
		t.Errorf("launches = %d, want 2 (cache miss after crash relaunches)", got)
	}

	// The gen-1 gvs/vd entry is now stale (generation advanced) → re-mints.
	if _, cached, _ := m.Mint(ctx, "gvs", "vd"); cached {
		t.Errorf("old-generation cache entry should be invalidated by the relaunch")
	}
}
