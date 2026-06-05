package httpx

import (
	"errors"
	"sync"
	"time"
)

// ErrBreakerOpen is returned by Allow when the breaker is cooling down. It is a
// clear drift signal, not a retry storm against Google.
var ErrBreakerOpen = errors.New("httpx: circuit breaker open (cooling down)")

// Breaker is a per-scope circuit breaker. Session creates one per minter key
// (egressID/endpointMode/browserProfileHash) so one bad proxy/profile can't
// block healthy paths. After Threshold consecutive attestation failures it
// opens for Cooldown. It also opens on a runtime-poison rate so a BotGuard
// update that deterministically hangs QuickJS can't spin a
// spawn->watchdog->evict->spawn loop. The cooldown is non-sensitive and can be
// persisted so a crash/restart loop stays well-behaved.
type Breaker struct {
	Threshold    int           // consecutive failures before opening (default 5)
	Cooldown     time.Duration // open duration (default 60s)
	PoisonRate   int           // poisons within PoisonWindow before opening (default 5)
	PoisonWindow time.Duration // sliding window for poisons (default 60s)
	now          func() time.Time

	mu        sync.Mutex
	failures  int
	openUntil time.Time
	poisons   []time.Time
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
	defer b.mu.Unlock()
	b.failures = 0
	b.openUntil = time.Time{}
	b.poisons = nil
}

// RecordFailure counts an attestation failure and opens the breaker at the
// threshold.
func (b *Breaker) RecordFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.withDefaults()
	b.failures++
	if b.failures >= b.Threshold {
		b.openUntil = b.now().Add(b.Cooldown)
	}
}

// RecordPoison logs a runtime poison and opens the breaker if poisons exceed
// PoisonRate within PoisonWindow (defends against a deterministic-hang respawn
// loop).
func (b *Breaker) RecordPoison() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.withDefaults()
	now := b.now()
	cutoff := now.Add(-b.PoisonWindow)
	kept := b.poisons[:0]
	for _, t := range b.poisons {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	b.poisons = append(kept, now)
	if len(b.poisons) >= b.PoisonRate {
		b.openUntil = now.Add(b.Cooldown)
	}
}
