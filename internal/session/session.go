// Package session manages WaxSeal's warm-minter pool. The expensive, CPU-bound
// BotGuard VM snapshot runs once per minter key and is reused until expiry:
// either to mint many identifiers through the integrity path or to serve the
// single websafe fallback token. Entries refresh ahead of expiry off the request
// path, so user requests keep serving the current token while a replacement is
// built. A global semaphore bounds concurrent snapshots, singleflight collapses
// cold callers per key, a per-key circuit breaker handles repeated drift, and
// wasm-boundary faults poison and evict their runtime.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"time"

	"github.com/colespringer/waxseal/internal/botguard"
	"github.com/colespringer/waxseal/internal/httpx"
	"github.com/colespringer/waxseal/internal/innertube"
	"github.com/colespringer/waxseal/internal/jsruntime"
	"github.com/colespringer/waxseal/internal/metrics"
)

// BreakerStore persists per-minter-key circuit-breaker cooldowns so a restart
// honors an active cooldown instead of starting another Create call immediately.
// *persist.Store satisfies it.
type BreakerStore interface {
	LoadBreakers() (map[string]time.Time, error)
	SaveBreaker(key string, openUntil time.Time) error
}

// TokenKind distinguishes a per-identifier integrity-minted token from the
// single websafe fallback token. They have different lifetime/validity
// semantics, so the Client keeps them on separate cache paths.
type TokenKind string

const (
	KindIntegrity TokenKind = "integrity"
	KindFallback  TokenKind = "fallback"
)

// Request is one token request against a warm-minter key.
type Request struct {
	Key           string            // full minter key (egress/endpoint/profile hash/client)
	Identifier    string            // visitor_data / video_id / opaque mint identifier
	ProfileJSON   json.RawMessage   // BrowserProfile JSON for runBotguard
	AttestationUA string            // UA for Create/GenerateIT
	Client        *httpx.Client     // egress (shared transport + jar)
	Endpoint      botguard.Endpoint // WAA endpoint (Create/GenerateIT URLs); zero = default
	ForceNew      bool              // 403/bypass: discard the warm entry, re-attest

	// Challenge inputs (priority: caller -> InnerTube att/get -> Create).
	// A caller-provided challenge is used only on the synchronous cold path;
	// background refreshes fetch their own challenge.
	Challenge        *botguard.Challenge
	InnertubeContext json.RawMessage // sent verbatim to att/get; nil uses a default
	DisableInnertube bool            // skip att/get, go straight to Create
}

// Result is a minted (or fallback) token with its authoritative expiry.
type Result struct {
	Token     string
	ExpiresAt time.Time
	Kind      TokenKind
}

// Options tune the manager.
type Options struct {
	SnapshotConcurrency int           // max concurrent ~910ms snapshots (default GOMAXPROCS/2)
	DefaultTTL          time.Duration // used when GenerateIT omits a lifetime (default 1h)
	MaxTTL              time.Duration // caps cached validity, never extends it (0 = uncapped)
	RefreshFraction     float64       // refresh-ahead point as a fraction of lifetime (default 0.9)
	RefreshTimeout      time.Duration // bound on a background refresh/prewarm (default 90s)
	Discovery           bool          // keep the shim's API drift probe trap on (dev/doctor)
	BreakerThreshold    int           // consecutive attestation failures before cool-down (default 5)
	BreakerCooldown     time.Duration // cool-down duration (default 60s)
	Logger              *slog.Logger
	Metrics             *metrics.Metrics // instrumentation; nil installs a private set
	BreakerStore        BreakerStore     // optional cooldown persistence (default = none)
	now                 func() time.Time
}

// Manager owns the shared engine and the per-key warm entries.
type Manager struct {
	engine jsruntime.Engine
	opts   Options
	sem    chan struct{}
	sf     flightGroup

	mu        sync.Mutex
	entries   map[string]*entry
	breakers  map[string]*httpx.Breaker
	cooldowns map[string]time.Time // persisted cooldowns awaiting their breaker's first use
}

type entry struct {
	key       string
	result    *botguard.GenerateITResult
	kind      TokenKind
	createdAt time.Time
	expiresAt time.Time
	refreshAt time.Time

	mu         sync.Mutex        // serialize mint() on the single-threaded runtime
	rt         jsruntime.Runtime // live warm runtime (integrity path only); nil for fallback
	refreshing bool              // a background refresh is in flight
}

// New builds a manager over engine. engine is shared (compile-once) and is
// closed by the caller, not by Manager.Close.
func New(engine jsruntime.Engine, opts Options) *Manager {
	if opts.SnapshotConcurrency <= 0 {
		opts.SnapshotConcurrency = max(1, runtime.GOMAXPROCS(0)/2)
	}
	if opts.DefaultTTL <= 0 {
		opts.DefaultTTL = time.Hour
	}
	if opts.RefreshFraction <= 0 || opts.RefreshFraction >= 1 {
		opts.RefreshFraction = 0.9
	}
	if opts.RefreshTimeout <= 0 {
		opts.RefreshTimeout = 90 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Metrics == nil {
		opts.Metrics = metrics.New()
	}
	if opts.now == nil {
		opts.now = time.Now
	}
	m := &Manager{
		engine:    engine,
		opts:      opts,
		sem:       make(chan struct{}, opts.SnapshotConcurrency),
		entries:   make(map[string]*entry),
		breakers:  make(map[string]*httpx.Breaker),
		cooldowns: make(map[string]time.Time),
	}
	// Seed persisted cooldowns so a restart honors active backoff before making
	// another Create call. Cooldowns are applied lazily because breakers are
	// created per key on demand.
	if opts.BreakerStore != nil {
		if cd, err := opts.BreakerStore.LoadBreakers(); err != nil {
			opts.Logger.Warn("session: load persisted breaker cooldowns failed", "err", err)
		} else {
			m.cooldowns = cd
		}
	}
	return m
}

// Token returns a token for req, building a warm entry if needed and minting (or
// serving the fallback) from it.
func (m *Manager) Token(ctx context.Context, req Request) (Result, error) {
	e, err := m.ensure(ctx, req)
	if err != nil {
		return Result{}, err
	}
	return m.serve(ctx, e, req)
}

// Prewarm builds the warm entry for req in the background so the first real
// request skips the ~910ms cold snapshot. Best-effort; failures are logged.
func (m *Manager) Prewarm(req Request) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), m.opts.RefreshTimeout)
		defer cancel()
		if _, err := m.ensure(ctx, req); err != nil {
			m.opts.Logger.Debug("session prewarm failed", "err", err)
			return
		}
		m.opts.Metrics.Prewarms.Inc()
	}()
}

// ensure returns a valid warm entry for the key, building it (under
// singleflight) on a cold miss and triggering a background refresh-ahead when
// the current entry is near expiry.
func (m *Manager) ensure(ctx context.Context, req Request) (*entry, error) {
	if req.ForceNew {
		m.evict(req.Key)
	}
	now := m.opts.now()
	if e := m.getEntry(req.Key); e != nil && now.Before(e.expiresAt) {
		if now.After(e.refreshAt) {
			m.maybeRefresh(req)
		}
		return e, nil
	}
	v, err, _ := m.sf.Do(req.Key, func() (any, error) {
		now := m.opts.now()
		if e := m.getEntry(req.Key); e != nil && now.Before(e.expiresAt) {
			return e, nil // a racing caller already built it
		}
		e, err := m.attest(ctx, req)
		if err != nil {
			return nil, err
		}
		m.putEntry(req.Key, e)
		return e, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*entry), nil
}

// serve produces the token from a warm entry: the fallback token directly, or a
// freshly minted per-identifier token on the warm runtime (serialized, since
// the QuickJS runtime is single-threaded).
func (m *Manager) serve(ctx context.Context, e *entry, req Request) (Result, error) {
	if e.kind == KindFallback {
		m.opts.Metrics.MintKind(string(KindFallback))
		return Result{Token: e.result.FallbackToken, ExpiresAt: e.expiresAt, Kind: KindFallback}, nil
	}
	// Mint under e.mu (mintFromEntry releases it via defer), then evict if
	// needed. Eviction re-locks e.mu via closeRuntime, so it must run after the
	// lock is released.
	token, poisoned, err := m.mintFromEntry(ctx, e, req.Identifier)
	if err != nil {
		// A concurrent refresh or ForceNew may have already installed a fresh
		// entry under this key. Only remove e if it is still current.
		if errors.Is(err, errRuntimeUnavailable) {
			m.evictEntry(e.key, e)
			return Result{}, err
		}
		if poisoned {
			m.breaker(e.key).RecordPoison()
			m.opts.Metrics.Poisons.Inc()
			m.evictEntry(e.key, e)
		}
		return Result{}, err
	}
	m.opts.Metrics.MintKind(string(KindIntegrity))
	return Result{Token: token, ExpiresAt: e.expiresAt, Kind: KindIntegrity}, nil
}

// errRuntimeUnavailable signals the warm runtime was evicted out from under us
// (nil/poisoned before the mint). serve evicts the entry and surfaces it.
var errRuntimeUnavailable = fmt.Errorf("session: warm runtime unavailable (evicted)")

// mintFromEntry mints one identifier on the entry's single-threaded warm runtime
// under e.mu, releasing the lock with a deferred Unlock so a panic in
// botguard.Mint cannot leave e.mu held. It does not evict: eviction re-locks
// e.mu, so the caller does that after this returns.
func (m *Manager) mintFromEntry(ctx context.Context, e *entry, identifier string) (token string, poisoned bool, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.rt == nil || e.rt.Poisoned() {
		return "", false, errRuntimeUnavailable
	}
	token, err = botguard.Mint(ctx, e.rt, identifier)
	poisoned = err != nil && e.rt.Poisoned()
	return token, poisoned, err
}

// attest is the cold, expensive path: challenge -> snapshot -> GenerateIT, then
// (for the integrity path) newMinter. The runtime is promoted into an entry only
// after newMinter succeeds; otherwise it is a throwaway and is closed here.
func (m *Manager) attest(ctx context.Context, req Request) (*entry, error) {
	br := m.breaker(req.Key)
	if rem, err := br.Allow(); err != nil {
		return nil, fmt.Errorf("session: %w (cooldown %s)", err, rem.Round(time.Second))
	}

	// Bound concurrent CPU-bound snapshots so cold-egress fan-out / breaker
	// respawns can't saturate cores.
	select {
	case m.sem <- struct{}{}:
		defer func() { <-m.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	m.opts.Metrics.Attestations.Inc()

	rt, err := m.engine.NewRuntime(ctx)
	if err != nil {
		br.RecordFailure()
		m.opts.Metrics.AttestFailure("runtime")
		return nil, fmt.Errorf("session: new runtime: %w", err)
	}
	keepRuntime := false
	defer func() {
		if !keepRuntime {
			_ = rt.Close(context.Background())
		}
	}()

	if !m.opts.Discovery {
		// Quiet the shim's probe trap in production (no per-request noise).
		_, _ = rt.Eval(ctx, "globalThis.__wxDiscovery=false;globalThis.__wxAutoStub=false;")
	}

	ch, err := m.resolveChallenge(ctx, req)
	if err != nil {
		br.RecordFailure()
		m.opts.Metrics.AttestFailure("challenge")
		return nil, err
	}
	m.opts.Metrics.Snapshots.Inc()
	bgResp, err := botguard.Snapshot(ctx, rt, ch, req.ProfileJSON)
	if err != nil {
		m.recordVMFailure(br, rt)
		m.opts.Metrics.AttestFailure("vm")
		return nil, err
	}
	it, err := botguard.GenerateIT(ctx, req.Client, req.AttestationUA, bgResp, req.Endpoint)
	if err != nil {
		br.RecordFailure()
		m.opts.Metrics.AttestFailure("generateit")
		return nil, err
	}

	now := m.opts.now()
	e := &entry{key: req.Key, result: it, kind: KindFallback, createdAt: now}
	if it.HasIntegrity() {
		if err := botguard.InstallMinter(ctx, rt, it.IntegrityToken); err != nil {
			m.recordVMFailure(br, rt)
			m.opts.Metrics.AttestFailure("minter")
			return nil, err
		}
		e.kind = KindIntegrity
		e.rt = rt // keep warm after newMinter succeeds
		keepRuntime = true
	} else {
		// The fallback path has no per-identifier minter that would validate the
		// token later, so validate field 6 before serving it.
		if _, err := botguard.ValidatePOToken(it.FallbackToken); err != nil {
			br.RecordFailure()
			m.opts.Metrics.AttestFailure("validate")
			return nil, fmt.Errorf("session: fallback token failed validation: %w", err)
		}
	}
	e.expiresAt = m.expiryFrom(it, now)
	e.refreshAt = m.refreshFrom(it, now, e.expiresAt)
	br.RecordSuccess()
	return e, nil
}

// resolveChallenge chooses the challenge source. A caller-provided challenge
// wins; otherwise att/get is tried unless disabled. att/get failures fall back
// to WAA Create.
func (m *Manager) resolveChallenge(ctx context.Context, req Request) (*botguard.Challenge, error) {
	if req.Challenge != nil {
		// A caller-supplied object/URL challenge carries no inline JS; fetch it
		// through the same bounded, host-allowlisted path as Create.
		if req.Challenge.InterpreterJS == "" && req.Challenge.InterpreterURL != "" {
			if err := botguard.ResolveInterpreter(ctx, req.Client, req.Challenge, req.AttestationUA); err != nil {
				return nil, err
			}
		}
		return req.Challenge, nil
	}
	if !req.DisableInnertube {
		ch, err := innertube.GetChallenge(ctx, req.Client, req.AttestationUA, req.InnertubeContext)
		if err == nil {
			return ch, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err() // cancellation should not fall through to Create
		}
		m.opts.Logger.Debug("innertube att/get failed; falling back to Create", "err", err)
	}
	return botguard.FetchCreateChallenge(ctx, req.Client, req.AttestationUA, req.Endpoint)
}

// recordVMFailure attributes a VM-stage failure to the breaker: a wasm-boundary
// poison counts toward the poison rate, while a plain failure counts toward the
// consecutive failure streak.
func (m *Manager) recordVMFailure(br *httpx.Breaker, rt jsruntime.Runtime) {
	if rt.Poisoned() {
		br.RecordPoison()
		m.opts.Metrics.Poisons.Inc()
	} else {
		br.RecordFailure()
	}
}

// maybeRefresh kicks a single background refresh for a still-valid entry
// (stale-while-revalidate): the current token keeps serving while a fresh
// snapshot is built and swapped in.
func (m *Manager) maybeRefresh(req Request) {
	m.mu.Lock()
	e := m.entries[req.Key]
	if e == nil || e.refreshing {
		m.mu.Unlock()
		return
	}
	e.refreshing = true
	m.mu.Unlock()

	// Caller-provided challenges are single-use. A later refresh must fetch a
	// fresh challenge instead of replaying the original one.
	req.Challenge = nil

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), m.opts.RefreshTimeout)
		defer cancel()
		fresh, err := m.attest(ctx, req)
		if err != nil {
			m.opts.Metrics.RefreshErrors.Inc()
			m.opts.Logger.Warn("session refresh-ahead failed; serving current until expiry", "err", err)
			m.mu.Lock()
			if cur := m.entries[req.Key]; cur != nil {
				cur.refreshing = false
			}
			m.mu.Unlock()
			return
		}
		m.putEntry(req.Key, fresh)
		m.opts.Metrics.Refreshes.Inc()
	}()
}

func (m *Manager) expiryFrom(it *botguard.GenerateITResult, now time.Time) time.Time {
	d := time.Duration(it.LifetimeSecs) * time.Second
	if d <= 0 {
		d = m.opts.DefaultTTL
	}
	if m.opts.MaxTTL > 0 && d > m.opts.MaxTTL {
		d = m.opts.MaxTTL
	}
	return now.Add(d)
}

func (m *Manager) refreshFrom(it *botguard.GenerateITResult, now, expiresAt time.Time) time.Time {
	if it.RefreshThreshold > 0 {
		if r := now.Add(time.Duration(it.RefreshThreshold) * time.Second); r.Before(expiresAt) {
			return r
		}
	}
	total := expiresAt.Sub(now)
	return now.Add(time.Duration(float64(total) * m.opts.RefreshFraction))
}

func (m *Manager) getEntry(key string) *entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.entries[key]
}

// putEntry installs e and closes the runtime of any entry it replaces.
func (m *Manager) putEntry(key string, e *entry) {
	m.mu.Lock()
	old := m.entries[key]
	e.refreshing = false
	m.entries[key] = e
	m.mu.Unlock()
	closeRuntime(old, e)
}

// evict removes and closes whatever entry is currently under key (403/ForceNew:
// we deliberately want to discard the current entry and re-attest).
func (m *Manager) evict(key string) {
	m.mu.Lock()
	e := m.entries[key]
	delete(m.entries, key)
	m.mu.Unlock()
	closeRuntime(e, nil)
}

// evictEntry removes and closes e only if it is still the current entry for key.
// A stale serve whose entry was replaced by a concurrent refresh or ForceNew must
// not delete the fresh replacement. closeRuntime(e, nil) is idempotent if e's
// runtime was already closed by the swap.
func (m *Manager) evictEntry(key string, e *entry) {
	m.mu.Lock()
	if m.entries[key] == e {
		delete(m.entries, key)
	}
	m.mu.Unlock()
	closeRuntime(e, nil)
}

// closeRuntime closes old's warm runtime unless it is the one keep retained.
func closeRuntime(old, keep *entry) {
	if old == nil {
		return
	}
	old.mu.Lock()
	defer old.mu.Unlock()
	if old.rt != nil && (keep == nil || old.rt != keep.rt) {
		_ = old.rt.Close(context.Background())
		old.rt = nil
	}
}

func (m *Manager) breaker(key string) *httpx.Breaker {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.breakers[key]
	if b == nil {
		b = httpx.NewBreaker(m.opts.BreakerThreshold, m.opts.BreakerCooldown)
		b.OnOpen = func() { m.opts.Metrics.BreakerOpens.Inc() }
		if m.opts.BreakerStore != nil {
			if until, ok := m.cooldowns[key]; ok {
				b.Restore(until)
				delete(m.cooldowns, key) // consumed; later state comes from the live breaker
			}
			store := m.opts.BreakerStore
			b.Persist = func(openUntil time.Time) { _ = store.SaveBreaker(key, openUntil) }
		}
		m.breakers[key] = b
	}
	return b
}

// Keys returns the current warm-minter keys (for the server's minter_cache
// endpoint). Order is unspecified.
func (m *Manager) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.entries))
	for k := range m.entries {
		keys = append(keys, k)
	}
	return keys
}

// InvalidateAll evicts every warm minter (closing its runtime), forcing the next
// request to re-attest. It backs the server's invalidate_it endpoint and leaves
// the manager usable. The shared engine is untouched.
func (m *Manager) InvalidateAll() {
	m.mu.Lock()
	entries := m.entries
	m.entries = make(map[string]*entry)
	m.mu.Unlock()
	for _, e := range entries {
		closeRuntime(e, nil)
	}
}

// Close releases all warm runtimes. The shared engine is the caller's to close.
func (m *Manager) Close() error {
	m.InvalidateAll()
	return nil
}
