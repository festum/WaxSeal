package httpx

import (
	"errors"
	"sync"
	"time"
)

// ErrBreakerOpen is returned by Allow while the breaker is cooling down.
var ErrBreakerOpen = errors.New("httpx: circuit breaker open (cooling down)")

// Breaker is a per-scope circuit breaker. Session creates one per minter key
// (egressID/endpointMode/browserProfileHash) so one bad proxy/profile can't
// block healthy paths. After Threshold consecutive attestation failures it
// opens for Cooldown. It also opens on a runtime-poison rate so a BotGuard
// update that deterministically hangs QuickJS cannot keep spawning, timing out,
// and evicting runtimes. The cooldown is non-sensitive and can be persisted
// across restarts.
type Breaker struct {
	Threshold    int           // consecutive failures before opening (default 5)
	Cooldown     time.Duration // open duration (default 60s)
	PoisonRate   int           // poisons within PoisonWindow before opening (default 5)
	PoisonWindow time.Duration // sliding window for poisons (default 60s)

	// Persist, when set, is called whenever openUntil changes (open or clear) so
	// the non-sensitive cooldown can survive a restart. OnOpen, when set, is
	// called once per closed->open transition (for metrics). Both run without the
	// breaker lock held. Restore seeds openUntil from persistence and fires
	// neither.
	Persist func(openUntil time.Time)
	OnOpen  func()

	now func() time.Time

	mu        sync.Mutex
	failures  int
	openUntil time.Time
	poisons   []time.Time
}

// Restore seeds the breaker's open-until time from persisted state. It fires no
// callbacks.
func (b *Breaker) Restore(openUntil time.Time) {
	b.mu.Lock()
	b.openUntil = openUntil
	b.mu.Unlock()
}

// NewBreaker returns a breaker with sane defaults; zero args use them.
func NewBreaker(threshold int, cooldown time.Duration) *Breaker {
	b := &Breaker{Threshold: threshold, Cooldown: cooldown}
	b.withDefaults()
	return b
}

func (b *Breaker) withDefaults() {
	if b.Threshold <= 0 {
		b.Threshold = 5
	}
	if b.Cooldown <= 0 {
		b.Cooldown = 60 * time.Second
	}
	if b.PoisonRate <= 0 {
		b.PoisonRate = 5
	}
	if b.PoisonWindow <= 0 {
		b.PoisonWindow = 60 * time.Second
	}
	if b.now == nil {
		b.now = time.Now
	}
}

// Allow reports whether a call may proceed, returning ErrBreakerOpen (and the
// remaining cooldown) while open.
func (b *Breaker) Allow() (time.Duration, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.withDefaults()
	if rem := b.openUntil.Sub(b.now()); rem > 0 {
		return rem, ErrBreakerOpen
	}
	return 0, nil
}

// RecordSuccess clears the failure streak and any open window.
func (b *Breaker) RecordSuccess() {
	b.mu.Lock()
	changed := !b.openUntil.IsZero()
	b.failures = 0
	b.openUntil = time.Time{}
	b.poisons = nil
	persist := b.Persist
	b.mu.Unlock()
	if changed && persist != nil {
		persist(time.Time{})
	}
}

// RecordFailure counts an attestation failure and opens the breaker at the
// threshold.
func (b *Breaker) RecordFailure() {
	b.mu.Lock()
	b.withDefaults()
	now := b.now()
	wasOpen := b.openUntil.After(now)
	b.failures++
	var opened time.Time
	if b.failures >= b.Threshold {
		opened = now.Add(b.Cooldown)
		b.openUntil = opened
	}
	nowOpen := b.openUntil.After(now)
	persist, onOpen := b.Persist, b.OnOpen
	b.mu.Unlock()
	b.signal(opened, wasOpen, nowOpen, persist, onOpen)
}

// RecordPoison logs a runtime poison and opens the breaker if poisons exceed
// PoisonRate within PoisonWindow (defends against a deterministic-hang respawn
// loop).
func (b *Breaker) RecordPoison() {
	b.mu.Lock()
	b.withDefaults()
	now := b.now()
	wasOpen := b.openUntil.After(now)
	cutoff := now.Add(-b.PoisonWindow)
	kept := b.poisons[:0]
	for _, t := range b.poisons {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	b.poisons = append(kept, now)
	var opened time.Time
	if len(b.poisons) >= b.PoisonRate {
		opened = now.Add(b.Cooldown)
		b.openUntil = opened
	}
	nowOpen := b.openUntil.After(now)
	persist, onOpen := b.Persist, b.OnOpen
	b.mu.Unlock()
	b.signal(opened, wasOpen, nowOpen, persist, onOpen)
}

// signal fires the persistence and open-transition callbacks outside the lock.
// opened is the new open-until time (zero if this call did not open the
// breaker); a closed->open transition additionally fires OnOpen.
func (b *Breaker) signal(opened time.Time, wasOpen, nowOpen bool, persist func(time.Time), onOpen func()) {
	if !opened.IsZero() && persist != nil {
		persist(opened)
	}
	if nowOpen && !wasOpen && onOpen != nil {
		onOpen()
	}
}
