package minter

import (
	"context"
	"errors"
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
	negCache    map[string]negEntry // video_id -> terminal player-context error + expiry (guarded by mu)

	mintMu  sync.Mutex // serializes the in-browser mint calls (single page)
	metrics minterMetrics
}

// minterSession is the slice of *browser.Session the Minter needs; an interface so tests
// can inject a fake. *browser.Session satisfies it.
type minterSession interface {
	Mint(ctx context.Context, identifier string) (browser.MintResult, error)
	PlayerContext(ctx context.Context, videoID string) (browser.PlayerContext, error)
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

// negEntry is one negatively-cached player-context outcome: a terminal (unplayable)
// error and when to forget it. It is generation-independent (an unplayable video
// stays unplayable across relaunches), so it carries no gen.
type negEntry struct {
	err    error
	expiry time.Time
}

// minterMetrics are process-lifetime counters. Both *Failures counters count failed
// ATTEMPTS, not requests: a request that exhausts the escalation ladder (first call,
// in-place retry, post-relaunch call) adds three. PlayerContextFailures also counts a
// negative-cache hit, where a known-unplayable id is rejected without a browser call.
type minterMetrics struct {
	Attestations          atomic.Int64
	LaunchFailures        atomic.Int64
	Mints                 atomic.Int64
	MintFailures          atomic.Int64 // per attempt (see minterMetrics doc)
	Escalations           atomic.Int64
	CacheHits             atomic.Int64
	CacheMisses           atomic.Int64
	Crashes               atomic.Int64
	PlayerContexts        atomic.Int64
	PlayerContextFailures atomic.Int64 // per attempt + negative-cache hits (see minterMetrics doc)
}

const (
	minterMaxCacheTTL   = 6 * time.Hour
	minterCacheMargin   = 5 * time.Minute // don't hand out a token within this of expiry
	minterDefaultMaxAge = 11 * time.Hour  // < the ~12h integrity lifetime
	minterNegCacheTTL   = 5 * time.Minute // remember an unplayable video_id this long
	minterNegCacheMax   = 256             // bound the negative cache
)

// NewMinter builds a single-identity minter for video (the landing watch id). It
// does not launch a browser until the first Warm/Mint/Identity call.
func NewMinter(video string, opts browser.Options) *Minter {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	m := &Minter{
		video:    video,
		opts:     opts,
		log:      log,
		maxAge:   minterDefaultMaxAge,
		cache:    make(map[string]cachedToken),
		negCache: make(map[string]negEntry),
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

// PlayerContext returns the attested browser's /player streaming context for
// videoID (the status-1 serverAbrStreamingUrl + ustreamer config + visitor_data +
// client version + audio formats). It reuses the warm attested session (the
// genuine-browser provenance is what grades the url, so no fresh attestation is
// needed), serialized on the single page like Mint. A transient failure gets one
// in-place retry, then a relaunch+re-attest, mirroring the mint escalation ladder.
// It is NOT cached (the url carries a per-request scrambled nonce and a short
// expiry, so each consumer needs a fresh one), but an unplayable video_id IS cached
// negatively so a repeat fails instantly without taking the page or escalating, and
// a cancelled caller never triggers a relaunch.
func (m *Minter) PlayerContext(ctx context.Context, videoID string) (browser.PlayerContext, error) {
	// A known-unplayable video fails before mintMu and the session, so a consumer
	// retrying a 502 (or a malicious caller) can't grind the tenant into relaunches.
	if err := m.negCacheGet(videoID); err != nil {
		m.metrics.PlayerContextFailures.Add(1)
		return browser.PlayerContext{}, err
	}

	m.mintMu.Lock() // one page → player-context calls serialize with mints
	defer m.mintMu.Unlock()

	sess, gen, err := m.ensure(ctx)
	if err != nil {
		return browser.PlayerContext{}, err
	}

	pc, err := sess.PlayerContext(ctx, videoID)
	if err == nil {
		m.metrics.PlayerContexts.Add(1)
		return pc, nil
	}
	if m.playerContextStop(ctx, videoID, err) { // terminal or cancelled: don't escalate.
		return browser.PlayerContext{}, err
	}

	// level 1: transient failure, one in-place retry, no re-attest.
	m.log.Warn("minter: player-context failed; retrying on same session", "gen", gen, "err", err)
	pc, err = sess.PlayerContext(ctx, videoID)
	if err == nil {
		m.metrics.PlayerContexts.Add(1)
		return pc, nil
	}
	if m.playerContextStop(ctx, videoID, err) {
		return browser.PlayerContext{}, err
	}

	// level 2: escalate to a relaunch and re-attest on a fresh session.
	m.metrics.Escalations.Add(1)
	m.retire(gen, "player-context failed twice; relaunching")
	sess, _, err = m.ensure(ctx)
	if err != nil {
		return browser.PlayerContext{}, err
	}
	pc, err = sess.PlayerContext(ctx, videoID)
	if err != nil {
		m.metrics.PlayerContextFailures.Add(1)
		if errors.Is(err, browser.ErrUnplayable) {
			m.negCachePut(videoID, err)
			return browser.PlayerContext{}, err
		}
		return browser.PlayerContext{}, fmt.Errorf("minter: player-context failed after relaunch: %w", err)
	}
	m.metrics.PlayerContexts.Add(1)
	return pc, nil
}

// playerContextStop records a failed player-context attempt and reports whether the
// escalation ladder must stop here rather than retry/relaunch: a terminal
// ErrUnplayable (which it also caches negatively) or a cancelled caller context
// (relaunching would burn the per-IP-scarce attestation for a request nobody awaits).
func (m *Minter) playerContextStop(ctx context.Context, videoID string, err error) bool {
	m.metrics.PlayerContextFailures.Add(1)
	if errors.Is(err, browser.ErrUnplayable) {
		m.negCachePut(videoID, err)
		return true
	}
	return ctx.Err() != nil
}

// negCacheGet returns a remembered terminal (unplayable) error for videoID while it
// is within its TTL, so a repeat of a known-unplayable video fails instantly without
// taking mintMu or touching the session.
func (m *Minter) negCacheGet(videoID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.negCache[videoID]
	if !ok {
		return nil
	}
	if time.Now().After(e.expiry) {
		delete(m.negCache, videoID)
		return nil
	}
	return e.err
}

// negCachePut remembers a terminal (unplayable) error for videoID for a short TTL.
// It prunes expired entries first; if the map is still full it evicts one live entry
// rather than dropping the new one, so the most recent unplayable id is always cached
// and a consumer retrying its last 502 won't reach the session again. Map iteration
// picks the victim arbitrarily, which is fine here: any live entry will do.
func (m *Minter) negCachePut(videoID string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	if len(m.negCache) >= minterNegCacheMax {
		for k, e := range m.negCache {
			if now.After(e.expiry) {
				delete(m.negCache, k)
			}
		}
		for len(m.negCache) >= minterNegCacheMax { // all live: evict one to make room
			for k := range m.negCache {
				delete(m.negCache, k)
				break
			}
		}
	}
	m.negCache[videoID] = negEntry{err: err, expiry: now.Add(minterNegCacheTTL)}
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
		"generation":              gen,
		"session_live":            live,
		"attest_kind":             kind,
		"session_age_secs":        ageSecs,
		"cache_entries":           cacheN,
		"attestations":            m.metrics.Attestations.Load(),
		"mints":                   m.metrics.Mints.Load(),
		"mint_failures":           m.metrics.MintFailures.Load(),
		"escalations":             m.metrics.Escalations.Load(),
		"player_contexts":         m.metrics.PlayerContexts.Load(),
		"player_context_failures": m.metrics.PlayerContextFailures.Load(),
		"crashes":                 m.metrics.Crashes.Load(),
		"cache_hits":              m.metrics.CacheHits.Load(),
		"cache_misses":            m.metrics.CacheMisses.Load(),
		"launch_failures":         m.metrics.LaunchFailures.Load(),
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
