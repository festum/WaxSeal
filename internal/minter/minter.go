package minter

import (
	"context"
	"fmt"
	"github.com/colespringer/waxseal/internal/browser"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-rod/rod/lib/proto"
)

// Minter wraps a single browser.Session with the reliability the bare Session
// lacks: single-flighted attestation (concurrent callers during a (re)launch
// trigger one attestation, not N, since attestation is the per-IP-scarce step), a
// generation-keyed token cache (a consumer retrying after a downstream 403 gets
// the same cached token, never a fresh attestation), an escalation ladder on mint
// failure (cache, then an in-place retry, then relaunch, so a transient blip does
// not burn an attestation), proactive crash recovery, a max-age recycle, and
// metrics.
//
// It is the single-identity minter; per-tenant contexts are handled by Tenants.
// The mint runs on one page, so mints serialize on mintMu.
type Minter struct {
	video  string
	opts   browser.Options
	log    *slog.Logger
	maxAge time.Duration // recycle the session once it is older than this

	// launch is the expensive launch+attest, injectable so the reliability logic
	// (single-flight, cache, escalation, crash) is unit-testable without a browser.
	launch func(ctx context.Context) (minterSession, error)

	mu          sync.Mutex
	sess        minterSession
	gen         uint64 // bumps on each (re)attest; invalidates older cache entries
	attestedAt  time.Time
	watchCancel context.CancelFunc // cancels the live session's crash watcher on teardown
	launching   chan struct{}      // non-nil while an attestation is in flight (single-flight)
	cache       map[string]cachedToken

	mintMu  sync.Mutex // serializes the in-browser mint calls (single page)
	metrics minterMetrics
}

// minterSession is the slice of *browser.Session the Minter needs; an interface so tests
// can inject a fake. *browser.Session satisfies it.
type minterSession interface {
	Mint(ctx context.Context, identifier string) (browser.MintResult, error)
	AttestKind() string
	Identity() browser.Identity
	BrowserCookies() []*http.Cookie
	Close()
}

type cachedToken struct {
	res    browser.MintResult
	expiry time.Time
	gen    uint64
}

type minterMetrics struct {
	Attestations   atomic.Int64
	LaunchFailures atomic.Int64
	Mints          atomic.Int64
	MintFailures   atomic.Int64
	Escalations    atomic.Int64
	CacheHits      atomic.Int64
	CacheMisses    atomic.Int64
	Crashes        atomic.Int64
}

const (
	minterMaxCacheTTL   = 6 * time.Hour
	minterCacheMargin   = 5 * time.Minute // don't hand out a token within this of expiry
	minterDefaultMaxAge = 11 * time.Hour  // < the ~12h integrity lifetime
)

// NewMinter builds a single-identity minter for video (the landing watch id). It
// does not launch a browser until the first Warm/Mint/Identity call.
func NewMinter(video string, opts browser.Options) *Minter {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	m := &Minter{
		video:  video,
		opts:   opts,
		log:    log,
		maxAge: minterDefaultMaxAge,
		cache:  make(map[string]cachedToken),
	}
	m.launch = m.launchReal
	return m
}

// launchReal is the default launcher: browser.Launch + the expensive Attest.
func (m *Minter) launchReal(ctx context.Context) (minterSession, error) {
	sess, err := browser.Launch(ctx, m.video, m.opts)
	if err != nil {
		return nil, err
	}
	if err := sess.Attest(ctx); err != nil {
		sess.Close()
		return nil, err
	}
	return sess, nil
}

// Warm forces the (single-flighted) attestation now, so the first request is fast
// and startup fails loudly if the browser/IP can't attest.
func (m *Minter) Warm(ctx context.Context) error {
	_, _, err := m.ensure(ctx)
	return err
}

// ensure returns the live session and its generation, single-flighting the
// launch+attest so concurrent callers coalesce into one attestation. It also
// recycles a session older than maxAge.
func (m *Minter) ensure(ctx context.Context) (minterSession, uint64, error) {
	for {
		m.mu.Lock()
		// Max-age recycle: retire a session older than maxAge.
		if m.sess != nil && m.maxAge > 0 && time.Since(m.attestedAt) > m.maxAge {
			old, gen, age := m.sess, m.gen, time.Since(m.attestedAt)
			m.sess = nil
			cancel := m.watchCancel
			m.watchCancel = nil
			m.mu.Unlock()
			if cancel != nil {
				cancel()
			}
			m.log.Info("minter: max-age recycle", "gen", gen, "age", age.Round(time.Second))
			old.Close()
			continue
		}
		if m.sess != nil {
			s, g := m.sess, m.gen
			m.mu.Unlock()
			return s, g, nil
		}
		if m.launching != nil { // another goroutine is attesting; wait for it.
			ch := m.launching
			m.mu.Unlock()
			select {
			case <-ch:
			case <-ctx.Done():
				return nil, 0, ctx.Err()
			}
			continue
		}
		// We own the (single-flighted) launch.
		ch := make(chan struct{})
		m.launching = ch
		m.mu.Unlock()

		sess, err := m.launch(ctx)

		m.mu.Lock()
		m.launching = nil
		close(ch)
		if err != nil {
			m.mu.Unlock()
			m.metrics.LaunchFailures.Add(1)
			return nil, 0, err
		}
		m.sess = sess
		m.gen++
		m.attestedAt = time.Now()
		g := m.gen
		// The crash watcher must outlive the (transient) launch ctx, so give it a
		// session-scoped context cancelled only when this session is torn down.
		watchCtx, cancel := context.WithCancel(context.Background())
		m.watchCancel = cancel
		m.mu.Unlock()
		m.metrics.Attestations.Add(1)
		m.log.Info("minter: session ready", "gen", g, "attest", sess.AttestKind())
		go m.watchCrash(sess, watchCtx, g)
		return sess, g, nil
	}
}

// retire closes the session of generation gen if it is still current, so the next
// ensure relaunches. A stale gen (already retired / newer session) is a no-op.
func (m *Minter) retire(gen uint64, reason string) {
	m.mu.Lock()
	if m.sess == nil || m.gen != gen {
		m.mu.Unlock()
		return
	}
	old := m.sess
	m.sess = nil
	cancel := m.watchCancel
	m.watchCancel = nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	m.log.Warn("minter: retiring session", "gen", gen, "reason", reason)
	old.Close()
}

// watchCrash retires the session if its browser target crashes or detaches, so a
// crash is recovered proactively (next request relaunches) instead of only after
// a failed mint. No-op for a non-*browser.Session (test fake).
func (m *Minter) watchCrash(s minterSession, ctx context.Context, gen uint64) {
	real, ok := s.(*browser.Session)
	if !ok || real.Page() == nil {
		return
	}
	// Bind to the session-scoped ctx (not the page's transient launch ctx) so the
	// watch lives until the session is torn down, then exits cleanly.
	wait := real.Page().Context(ctx).EachEvent(
		func(*proto.InspectorTargetCrashed) (stop bool) {
			m.metrics.Crashes.Add(1)
			m.retire(gen, "browser target crashed")
			return true
		},
		func(e *proto.InspectorDetached) (stop bool) {
			m.retire(gen, "browser detached: "+e.Reason)
			return true
		},
	)
	wait()
}

// Mint returns a token for (scope, binding), reporting whether it came from cache.
// The escalation ladder: serve from cache, else mint; on failure retry once in
// place, then relaunch, re-attest, and mint. A downstream 403 that makes the
// consumer re-request the same binding is served from cache (no re-mint, no
// re-attest).
func (m *Minter) Mint(ctx context.Context, scope, binding string) (res browser.MintResult, cached bool, err error) {
	key := scope + "|" + binding
	if r, ok := m.cacheGet(key); ok {
		m.metrics.CacheHits.Add(1)
		return r, true, nil
	}

	m.mintMu.Lock() // one page → mints serialize
	defer m.mintMu.Unlock()
	// Double-check: a goroutine ahead of us on mintMu may have just minted this key.
	if r, ok := m.cacheGet(key); ok {
		m.metrics.CacheHits.Add(1)
		return r, true, nil
	}
	m.metrics.CacheMisses.Add(1)

	sess, gen, err := m.ensure(ctx)
	if err != nil {
		return browser.MintResult{}, false, err
	}
	res, err = sess.Mint(ctx, binding)
	if err != nil { // level 1: transient failure, one in-place retry, no re-attest.
		m.metrics.MintFailures.Add(1)
		m.log.Warn("minter: mint failed; retrying on same session", "gen", gen, "err", err)
		res, err = sess.Mint(ctx, binding)
	}
	if err != nil { // level 2: escalate to a relaunch and re-attest on a fresh session.
		m.metrics.MintFailures.Add(1)
		m.metrics.Escalations.Add(1)
		m.retire(gen, "mint failed twice; relaunching")
		sess, gen, err = m.ensure(ctx)
		if err != nil {
			return browser.MintResult{}, false, err
		}
		if res, err = sess.Mint(ctx, binding); err != nil {
			m.metrics.MintFailures.Add(1)
			return browser.MintResult{}, false, fmt.Errorf("minter: mint failed after relaunch: %w", err)
		}
	}
	m.metrics.Mints.Add(1)
	m.cachePut(key, res, gen)
	return res, false, nil
}

func (m *Minter) cacheGet(key string) (browser.MintResult, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.cache[key]
	if !ok || c.gen != m.gen || time.Now().After(c.expiry) {
		return browser.MintResult{}, false
	}
	return c.res, true
}

func (m *Minter) cachePut(key string, res browser.MintResult, gen uint64) {
	ttl := time.Duration(res.Lifetime) * time.Second
	if ttl <= 0 || ttl > minterMaxCacheTTL {
		ttl = minterMaxCacheTTL
	}
	if ttl -= minterCacheMargin; ttl < 0 {
		ttl = 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if gen != m.gen { // session was recycled mid-mint; don't cache a stale-gen token.
		return
	}
	m.cache[key] = cachedToken{res: res, expiry: time.Now().Add(ttl), gen: gen}
}

// Identity ensures a session and returns its captured identity (for /ping).
func (m *Minter) Identity(ctx context.Context) (browser.Identity, error) {
	s, _, err := m.ensure(ctx)
	if err != nil {
		return browser.Identity{}, err
	}
	return s.Identity(), nil
}

// Cookies ensures a session and returns its live youtube.com cookies (for
// /session, the coherence handoff).
func (m *Minter) Cookies(ctx context.Context) ([]*http.Cookie, error) {
	s, _, err := m.ensure(ctx)
	if err != nil {
		return nil, err
	}
	return s.BrowserCookies(), nil
}

// AttestKind ensures a session and returns its attestation grade.
func (m *Minter) AttestKind(ctx context.Context) (string, error) {
	s, _, err := m.ensure(ctx)
	if err != nil {
		return "", err
	}
	return s.AttestKind(), nil
}

// MetricsSnapshot returns counters + current state for a /metrics endpoint.
func (m *Minter) MetricsSnapshot() map[string]any {
	m.mu.Lock()
	gen := m.gen
	live := m.sess != nil
	kind := ""
	var ageSecs int
	if m.sess != nil {
		kind = m.sess.AttestKind()
		ageSecs = int(time.Since(m.attestedAt).Seconds())
	}
	cacheN := len(m.cache)
	m.mu.Unlock()
	return map[string]any{
		"generation":       gen,
		"session_live":     live,
		"attest_kind":      kind,
		"session_age_secs": ageSecs,
		"cache_entries":    cacheN,
		"attestations":     m.metrics.Attestations.Load(),
		"mints":            m.metrics.Mints.Load(),
		"mint_failures":    m.metrics.MintFailures.Load(),
		"escalations":      m.metrics.Escalations.Load(),
		"crashes":          m.metrics.Crashes.Load(),
		"cache_hits":       m.metrics.CacheHits.Load(),
		"cache_misses":     m.metrics.CacheMisses.Load(),
		"launch_failures":  m.metrics.LaunchFailures.Load(),
	}
}

// Close tears down the live session.
func (m *Minter) Close() {
	m.mu.Lock()
	s := m.sess
	m.sess = nil
	cancel := m.watchCancel
	m.watchCancel = nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if s != nil {
		s.Close()
	}
}
