package minter

import (
	"context"
	"errors"
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
	mint         func(identifier string) (browser.MintResult, error)
	playerCtx    func(videoID string) (browser.PlayerContext, error)
	ping         func() error // nil reports a healthy browser
	establishErr error
	cookies      []*http.Cookie
	cookiesErr   error
	id           browser.Identity // zero value reports a default visitor_data
	closed       atomic.Bool
}

func (f *fakeSession) Mint(_ context.Context, id string) (browser.MintResult, error) {
	return f.mint(id)
}
func (f *fakeSession) PlayerContext(ctx context.Context, videoID string) (browser.PlayerContext, error) {
	// Model the real Session, whose poll loops return ctx.Err() on cancel/deadline, so
	// the cancel test's guarantee holds even if ensure ever stops gating a warm session.
	if err := ctx.Err(); err != nil {
		return browser.PlayerContext{}, err
	}
	if f.playerCtx == nil {
		return browser.PlayerContext{ServerAbrStreamingURL: "https://example.googlevideo.com/videoplayback?n=scrambled", VisitorData: "vd"}, nil
	}
	return f.playerCtx(videoID)
}
func (f *fakeSession) EnsureEstablished(context.Context) error { return f.establishErr }

// Ping gives cancellation precedence over the configured result.
func (f *fakeSession) Ping(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.ping == nil {
		return nil
	}
	return f.ping()
}
func (f *fakeSession) AttestKind() string { return "integrity" }
func (f *fakeSession) Identity() browser.Identity {
	if f.id.VisitorData == "" {
		return browser.Identity{VisitorData: "vd"}
	}
	return f.id
}
func (f *fakeSession) BrowserCookies() ([]*http.Cookie, error) { return f.cookies, f.cookiesErr }
func (f *fakeSession) Close()                                  { f.closed.Store(true) }

// newTestMinter returns a Minter whose launcher records each created session and
// uses the supplied per-mint behaviour.
func newTestMinter(mint func(id string) (browser.MintResult, error)) (*Minter, *int64, *[]*fakeSession, *sync.Mutex) {
	m, launches, sessions, smu := newTestMinterFull(mint, nil)
	return m, launches, sessions, smu
}

// newTestMinterFull is newTestMinter with an explicit per-session PlayerContext
// behaviour (nil uses the fakeSession default).
func newTestMinterFull(mint func(id string) (browser.MintResult, error), playerCtx func(videoID string) (browser.PlayerContext, error)) (*Minter, *int64, *[]*fakeSession, *sync.Mutex) {
	var launches int64
	var sessions []*fakeSession
	var smu sync.Mutex
	m := NewMinter("v", browser.Options{})
	m.launch = func(context.Context) (minterSession, error) {
		atomic.AddInt64(&launches, 1)
		fs := &fakeSession{mint: mint, playerCtx: playerCtx}
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

	// The already minted token remains valid, so the cache serves it without a relaunch.
	if _, cached, _ := m.Mint(ctx, "gvs", "vd"); !cached {
		t.Errorf("cached token should survive a crash (no needless re-attest)")
	}
	if got := atomic.LoadInt64(launches); got != 1 {
		t.Errorf("launches = %d, want 1 (a crash alone must not re-attest)", got)
	}

	// A new binding misses the cache and causes a generation-2 relaunch.
	if _, cached, _ := m.Mint(ctx, "player", "vid2"); cached {
		t.Errorf("new binding should not be cached")
	}
	if got := atomic.LoadInt64(launches); got != 2 {
		t.Errorf("launches = %d, want 2 (cache miss after crash relaunches)", got)
	}

	// The generation-1 gvs/vd entry is stale after the relaunch.
	if _, cached, _ := m.Mint(ctx, "gvs", "vd"); cached {
		t.Errorf("old-generation cache entry should be invalidated by the relaunch")
	}
}

// TestMinterPlayerContextReusesWarmSession: PlayerContext serves off the warm
// attested session without a fresh attestation. The URL depends on the browser's
// provenance rather than a new mint.
func TestMinterPlayerContextReusesWarmSession(t *testing.T) {
	var calls int64
	m, launches, _, _ := newTestMinterFull(
		func(string) (browser.MintResult, error) { return browser.MintResult{Lifetime: 3600}, nil },
		func(videoID string) (browser.PlayerContext, error) {
			atomic.AddInt64(&calls, 1)
			return browser.PlayerContext{
				Status:                "OK",
				ServerAbrStreamingURL: "https://r1.googlevideo.com/videoplayback?n=scram-" + videoID,
				VisitorData:           "vd",
				ClientVersion:         "2.20260606.02.00",
				AudioFormats:          []browser.AudioFormat{{Itag: 251, LMT: "1719185012384481", MimeType: "audio/webm"}},
			}, nil
		},
	)
	ctx := context.Background()
	if err := m.Warm(ctx); err != nil { // gen 1
		t.Fatalf("warm: %v", err)
	}
	pc, err := m.PlayerContext(ctx, "vid")
	if err != nil {
		t.Fatalf("player-context: %v", err)
	}
	if pc.ServerAbrStreamingURL != "https://r1.googlevideo.com/videoplayback?n=scram-vid" || len(pc.AudioFormats) != 1 {
		t.Fatalf("unexpected context: %+v", pc)
	}
	if got := atomic.LoadInt64(launches); got != 1 {
		t.Errorf("launches = %d, want 1 (player-context reuses the warm session)", got)
	}
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Errorf("player-context calls = %d, want 1", got)
	}
}

// TestMinterPlayerContextEscalation: a player-context that fails twice triggers one
// in-place retry then a relaunch+re-attest on a fresh session, mirroring the mint
// escalation ladder; the failed session is closed.
func TestMinterPlayerContextEscalation(t *testing.T) {
	var attempt int64
	m, launches, sessions, smu := newTestMinterFull(
		func(string) (browser.MintResult, error) { return browser.MintResult{Lifetime: 3600}, nil },
		func(string) (browser.PlayerContext, error) {
			if n := atomic.AddInt64(&attempt, 1); n <= 2 {
				return browser.PlayerContext{}, fmt.Errorf("transient failure %d", n)
			}
			return browser.PlayerContext{Status: "OK", ServerAbrStreamingURL: "https://r/ok", VisitorData: "vd"}, nil
		},
	)
	ctx := context.Background()

	pc, err := m.PlayerContext(ctx, "vid")
	if err != nil {
		t.Fatalf("player-context after escalation: %v", err)
	}
	if pc.ServerAbrStreamingURL != "https://r/ok" {
		t.Fatalf("got URL=%q, want https://r/ok", pc.ServerAbrStreamingURL)
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

// TestMinterPlayerContextUnplayableNoEscalation: a terminal ErrUnplayable does not
// walk the ladder (no relaunch, no re-attest, the warm session survives), since
// relaunching cannot make an unplayable video playable.
func TestMinterPlayerContextUnplayableNoEscalation(t *testing.T) {
	m, launches, sessions, smu := newTestMinterFull(
		func(string) (browser.MintResult, error) { return browser.MintResult{Lifetime: 3600}, nil },
		func(string) (browser.PlayerContext, error) {
			return browser.PlayerContext{}, fmt.Errorf("%w: UNPLAYABLE", browser.ErrUnplayable)
		},
	)
	ctx := context.Background()
	if err := m.Warm(ctx); err != nil { // gen 1
		t.Fatalf("warm: %v", err)
	}
	_, err := m.PlayerContext(ctx, "vid")
	if err == nil || !errors.Is(err, browser.ErrUnplayable) {
		t.Fatalf("err = %v, want ErrUnplayable", err)
	}
	if got := atomic.LoadInt64(launches); got != 1 {
		t.Errorf("launches = %d, want 1 (unplayable must not relaunch/re-attest)", got)
	}
	if got := m.metrics.Escalations.Load(); got != 0 {
		t.Errorf("escalations = %d, want 0", got)
	}
	if got := m.metrics.PlayerContextFailures.Load(); got != 1 {
		t.Errorf("player_context_failures = %d, want 1", got)
	}
	smu.Lock()
	defer smu.Unlock()
	if (*sessions)[0].closed.Load() {
		t.Errorf("session should not be retired for an unplayable video")
	}
}

// TestMinterPlayerContextUnplayableNegativeCache: a repeat request for a
// known-unplayable video is served from the negative cache: the session's
// PlayerContext is not called again (no mintMu, no eval).
func TestMinterPlayerContextUnplayableNegativeCache(t *testing.T) {
	var calls int64
	m, _, _, _ := newTestMinterFull(
		func(string) (browser.MintResult, error) { return browser.MintResult{Lifetime: 3600}, nil },
		func(string) (browser.PlayerContext, error) {
			atomic.AddInt64(&calls, 1)
			return browser.PlayerContext{}, fmt.Errorf("%w: LOGIN_REQUIRED", browser.ErrUnplayable)
		},
	)
	ctx := context.Background()
	if err := m.Warm(ctx); err != nil {
		t.Fatalf("warm: %v", err)
	}
	if _, err := m.PlayerContext(ctx, "vid"); !errors.Is(err, browser.ErrUnplayable) {
		t.Fatalf("first: err = %v, want ErrUnplayable", err)
	}
	if _, err := m.PlayerContext(ctx, "vid"); !errors.Is(err, browser.ErrUnplayable) {
		t.Fatalf("second: err = %v, want ErrUnplayable (from negative cache)", err)
	}
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Errorf("session PlayerContext calls = %d, want 1 (second served from negative cache)", got)
	}
	if got := m.metrics.PlayerContextFailures.Load(); got != 2 {
		t.Errorf("player_context_failures = %d, want 2", got)
	}
}

// TestMinterPlayerContextCancelNoEscalation: a cancelled caller context fails without
// escalating: the warm attested session is not retired and there is no relaunch.
func TestMinterPlayerContextCancelNoEscalation(t *testing.T) {
	m, launches, sessions, smu := newTestMinterFull(
		func(string) (browser.MintResult, error) { return browser.MintResult{Lifetime: 3600}, nil },
		func(string) (browser.PlayerContext, error) { return browser.PlayerContext{}, context.Canceled },
	)
	if err := m.Warm(context.Background()); err != nil { // gen 1, live ctx
		t.Fatalf("warm: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // client disconnected
	if _, err := m.PlayerContext(ctx, "vid"); err == nil {
		t.Fatal("want error on cancelled ctx")
	}
	if got := atomic.LoadInt64(launches); got != 1 {
		t.Errorf("launches = %d, want 1 (a cancel must not relaunch)", got)
	}
	if got := m.metrics.Escalations.Load(); got != 0 {
		t.Errorf("escalations = %d, want 0", got)
	}
	smu.Lock()
	defer smu.Unlock()
	if (*sessions)[0].closed.Load() {
		t.Errorf("warm session should survive a client cancel")
	}
}

// TestMinterNegCacheBoundedEvicts: at capacity with every entry live, a new terminal
// result is still cached (evicting an older one) instead of dropped, so the map stays
// bounded and the newest unplayable id is the one kept.
func TestMinterNegCacheBoundedEvicts(t *testing.T) {
	m := NewMinter("v", browser.Options{})
	for i := 0; i < minterNegCacheMax; i++ {
		m.negCachePut(fmt.Sprintf("vid%05d", i), browser.ErrUnplayable)
	}
	if got := len(m.negCache); got != minterNegCacheMax {
		t.Fatalf("negCache size = %d, want %d (filled to capacity)", got, minterNegCacheMax)
	}
	m.negCachePut("newestUnplay", browser.ErrUnplayable) // one past capacity, all others live
	if got := len(m.negCache); got != minterNegCacheMax {
		t.Errorf("negCache size = %d, want %d (stays bounded after eviction)", got, minterNegCacheMax)
	}
	if err := m.negCacheGet("newestUnplay"); !errors.Is(err, browser.ErrUnplayable) {
		t.Errorf("newest terminal result should be cached after eviction, got %v", err)
	}
}

// newPingMinter records each session and configures its Ping result.
func newPingMinter(ping func() error) (*Minter, *[]*fakeSession, *sync.Mutex) {
	var sessions []*fakeSession
	var smu sync.Mutex
	m := NewMinter("v", browser.Options{})
	m.launch = func(context.Context) (minterSession, error) {
		fs := &fakeSession{
			mint: func(string) (browser.MintResult, error) { return browser.MintResult{Lifetime: 3600}, nil },
			ping: ping,
		}
		smu.Lock()
		sessions = append(sessions, fs)
		smu.Unlock()
		return fs, nil
	}
	return m, &sessions, &smu
}

// An unwarmed tenant must report errNoSession without launching a browser.
func TestMinterHealthyNoSession(t *testing.T) {
	m, sessions, smu := newPingMinter(nil)
	if _, _, err := m.Healthy(context.Background()); !errors.Is(err, errNoSession) {
		t.Errorf("Healthy = %v, want errNoSession", err)
	}
	smu.Lock()
	defer smu.Unlock()
	if len(*sessions) != 0 {
		t.Errorf("Healthy launched %d sessions, want 0", len(*sessions))
	}
}

// A successful probe returns metadata from the existing session.
func TestMinterHealthyLivePing(t *testing.T) {
	m, sessions, smu := newPingMinter(nil)
	if err := m.Warm(context.Background()); err != nil {
		t.Fatalf("warm: %v", err)
	}
	id, kind, err := m.Healthy(context.Background())
	if err != nil {
		t.Errorf("Healthy returned error %v, want nil", err)
	}
	if id.VisitorData == "" {
		t.Error("Healthy returned an empty identity for a live session")
	}
	if kind != "integrity" {
		t.Errorf("attestation grade = %q, want integrity", kind)
	}
	smu.Lock()
	defer smu.Unlock()
	if (*sessions)[0].closed.Load() {
		t.Error("a healthy session must not be retired")
	}
}

// A failed probe retires the session when no browser operation is in progress.
func TestMinterHealthyDeadPingRetires(t *testing.T) {
	m, sessions, smu := newPingMinter(func() error { return errors.New("cdp connection closed") })
	if err := m.Warm(context.Background()); err != nil {
		t.Fatalf("warm: %v", err)
	}
	if _, _, err := m.Healthy(context.Background()); err == nil {
		t.Fatal("Healthy = nil, want the probe error")
	}
	smu.Lock()
	defer smu.Unlock()
	if !(*sessions)[0].closed.Load() {
		t.Error("failed probe did not retire the idle session")
	}
}

// A failed probe must not retire a session while its page is in use.
func TestMinterHealthyDeadPingHeldMintMuNoRetire(t *testing.T) {
	m, sessions, smu := newPingMinter(func() error { return errors.New("cdp connection closed") })
	if err := m.Warm(context.Background()); err != nil {
		t.Fatalf("warm: %v", err)
	}
	m.mintMu.Lock() // Hold the page lock during the probe.
	_, _, err := m.Healthy(context.Background())
	m.mintMu.Unlock()
	if err == nil {
		t.Fatal("Healthy = nil, want the probe error")
	}
	smu.Lock()
	defer smu.Unlock()
	if (*sessions)[0].closed.Load() {
		t.Error("probe retired a session while its page was in use")
	}
}

// Canceling the probe must not retire the session.
func TestMinterHealthyCanceledCtxNoRetire(t *testing.T) {
	m, sessions, smu := newPingMinter(func() error { return errors.New("cdp connection closed") })
	if err := m.Warm(context.Background()); err != nil {
		t.Fatalf("warm: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := m.Healthy(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Healthy = %v, want context.Canceled", err)
	}
	smu.Lock()
	defer smu.Unlock()
	if (*sessions)[0].closed.Load() {
		t.Error("a canceled health check must not retire the session")
	}
}

// Healthy retries when the probed session is replaced concurrently.
func TestMinterHealthyReprobesAfterRecycle(t *testing.T) {
	m := NewMinter("v", browser.Options{})
	sess2 := &fakeSession{
		id:   browser.Identity{VisitorData: "vd2"},
		mint: func(string) (browser.MintResult, error) { return browser.MintResult{Lifetime: 3600}, nil },
	}
	sess1 := &fakeSession{
		id:   browser.Identity{VisitorData: "vd1"},
		mint: func(string) (browser.MintResult, error) { return browser.MintResult{Lifetime: 3600}, nil },
		ping: func() error {
			// Replace the session while its probe is in progress.
			m.mu.Lock()
			m.sess = sess2
			m.gen++
			m.mu.Unlock()
			return errors.New("probed session was recycled")
		},
	}
	m.mu.Lock()
	m.sess = sess1
	m.gen = 1
	m.attestedAt = time.Now()
	m.mu.Unlock()

	id, _, err := m.Healthy(context.Background())
	if err != nil {
		t.Fatalf("Healthy after session replacement = %v, want nil", err)
	}
	if id.VisitorData != "vd2" {
		t.Errorf("identity = %q, want vd2 from the replacement session", id.VisitorData)
	}
	if sess1.closed.Load() {
		t.Error("Healthy retired the superseded session")
	}
}

// SelfTest caches its GVS token under the regular mint key.
func TestMinterSelfTestCachesGVSMint(t *testing.T) {
	var mints int64
	m, _, _, _ := newTestMinter(func(id string) (browser.MintResult, error) {
		atomic.AddInt64(&mints, 1)
		return browser.MintResult{Kind: "integrity", Token: "gvs-" + id, TokenLen: 5, Identifier: id, Lifetime: 3600}, nil
	})
	ctx := context.Background()
	if err := m.SelfTest(ctx); err != nil {
		t.Fatalf("SelfTest: %v", err)
	}
	if got := atomic.LoadInt64(&mints); got != 1 {
		t.Errorf("mints during self-test = %d, want 1", got)
	}
	// The default fake identity reports visitor_data "vd".
	if _, cached, err := m.Mint(ctx, "gvs", "vd"); err != nil || !cached {
		t.Errorf("gvs/vd after self-test: cached=%v err=%v, want cached=true", cached, err)
	}
	if got := atomic.LoadInt64(&mints); got != 1 {
		t.Errorf("mints after the cache hit = %d, want 1", got)
	}
}

// SelfTest returns a persistent mint failure after its bounded retry.
func TestMinterSelfTestMintFatal(t *testing.T) {
	defer func(d time.Duration) { selfTestMintRetryDelay = d }(selfTestMintRetryDelay)
	selfTestMintRetryDelay = time.Millisecond
	m, _, _, _ := newTestMinter(func(string) (browser.MintResult, error) {
		return browser.MintResult{}, errors.New("mint broken")
	})
	if err := m.SelfTest(context.Background()); err == nil {
		t.Fatal("SelfTest = nil, want an error after a persistent mint failure")
	}
}

// SelfTest retries a transient mint failure without relaunching.
func TestMinterSelfTestMintRetrySucceeds(t *testing.T) {
	defer func(d time.Duration) { selfTestMintRetryDelay = d }(selfTestMintRetryDelay)
	selfTestMintRetryDelay = time.Millisecond
	var attempt int64
	m, launches, _, _ := newTestMinter(func(string) (browser.MintResult, error) {
		if atomic.AddInt64(&attempt, 1) == 1 {
			return browser.MintResult{}, errors.New("temporary mint failure")
		}
		return browser.MintResult{Kind: "integrity", Token: "ok", Lifetime: 3600}, nil
	})
	if err := m.SelfTest(context.Background()); err != nil {
		t.Fatalf("SelfTest: %v", err)
	}
	if got := atomic.LoadInt64(launches); got != 1 {
		t.Errorf("launches = %d, want 1", got)
	}
}

// SelfTest logs establishment failures instead of returning them.
func TestMinterSelfTestEstablishmentWarnOnly(t *testing.T) {
	m := NewMinter("v", browser.Options{})
	m.launch = func(context.Context) (minterSession, error) {
		return &fakeSession{
			mint: func(string) (browser.MintResult, error) {
				return browser.MintResult{Kind: "integrity", Token: "t", Lifetime: 3600}, nil
			},
			establishErr: errors.New("full-length proof failed"),
		}, nil
	}
	if err := m.SelfTest(context.Background()); err != nil {
		t.Errorf("SelfTest = %v, want nil after a logged establishment failure", err)
	}
}

// SessionSnapshot returns identity and cookies after establishment.
func TestMinterSessionSnapshot(t *testing.T) {
	wantCookie := &http.Cookie{Name: "VISITOR_INFO1_LIVE", Value: "abc"}
	m := NewMinter("v", browser.Options{})
	m.launch = func(context.Context) (minterSession, error) {
		return &fakeSession{
			id:      browser.Identity{VisitorData: "vd-snap"},
			cookies: []*http.Cookie{wantCookie},
			mint:    func(string) (browser.MintResult, error) { return browser.MintResult{Lifetime: 3600}, nil },
		}, nil
	}
	id, cookies, err := m.SessionSnapshot(context.Background())
	if err != nil {
		t.Fatalf("SessionSnapshot: %v", err)
	}
	if id.VisitorData != "vd-snap" {
		t.Errorf("visitor_data = %q, want vd-snap", id.VisitorData)
	}
	if len(cookies) != 1 || cookies[0].Name != "VISITOR_INFO1_LIVE" {
		t.Errorf("cookies = %+v, want one VISITOR_INFO1_LIVE", cookies)
	}
}

// SessionSnapshot must not export a session that failed establishment.
func TestMinterSessionSnapshotEstablishmentError(t *testing.T) {
	m := NewMinter("v", browser.Options{})
	m.launch = func(context.Context) (minterSession, error) {
		return &fakeSession{
			id:           browser.Identity{VisitorData: "vd"},
			establishErr: errors.New("not established"),
			mint:         func(string) (browser.MintResult, error) { return browser.MintResult{Lifetime: 3600}, nil },
		}, nil
	}
	if _, _, err := m.SessionSnapshot(context.Background()); err == nil {
		t.Fatal("SessionSnapshot = nil, want the establishment error")
	}
}

// SessionSnapshot returns cookie-read failures.
func TestMinterSessionSnapshotCookieError(t *testing.T) {
	m := NewMinter("v", browser.Options{})
	m.launch = func(context.Context) (minterSession, error) {
		return &fakeSession{
			id:         browser.Identity{VisitorData: "vd"},
			cookiesErr: errors.New("cdp cookie read failed"),
			mint:       func(string) (browser.MintResult, error) { return browser.MintResult{Lifetime: 3600}, nil },
		}, nil
	}
	if _, _, err := m.SessionSnapshot(context.Background()); err == nil {
		t.Fatal("SessionSnapshot = nil, want the propagated cookie error")
	}
}
