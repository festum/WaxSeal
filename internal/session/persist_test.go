package session

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/colespringer/waxseal/internal/metrics"
)

// fakeBreakerStore is an in-memory BreakerStore that records saves.
type fakeBreakerStore struct {
	mu    sync.Mutex
	m     map[string]time.Time
	saves int
}

func newFakeBreakerStore() *fakeBreakerStore {
	return &fakeBreakerStore{m: map[string]time.Time{}}
}

func (s *fakeBreakerStore) LoadBreakers() (map[string]time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]time.Time, len(s.m))
	for k, v := range s.m {
		out[k] = v
	}
	return out, nil
}

func (s *fakeBreakerStore) SaveBreaker(key string, openUntil time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saves++
	if openUntil.IsZero() {
		delete(s.m, key)
	} else {
		s.m[key] = openUntil
	}
	return nil
}

func (s *fakeBreakerStore) has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.m[key]
	return ok
}

// TestBreakerCooldownPersists checks that a fresh manager seeded from the store
// honors an open breaker before making another attestation call.
func TestBreakerCooldownPersists(t *testing.T) {
	store := newFakeBreakerStore()
	eng := &fakeEngine{}
	tr := &fakeTransport{genITBody: func() (int, string) { return 500, "" }} // GenerateIT fails
	m := newTestManager(eng, Options{BreakerThreshold: 2, BreakerCooldown: time.Minute, BreakerStore: store})
	hc := clientWith(tr)

	for i := range 2 {
		if _, err := m.Token(context.Background(), req("k", "x", hc)); err == nil {
			t.Fatalf("attempt %d: expected GenerateIT failure", i)
		}
	}
	if !store.has("k") {
		t.Fatal("breaker opened but no cooldown was persisted")
	}

	// A new manager seeded from the same store must honor the cooldown without
	// touching Google.
	eng2 := &fakeEngine{}
	tr2 := &fakeTransport{genITBody: fallbackBody}
	m2 := newTestManager(eng2, Options{BreakerStore: store})
	hc2 := clientWith(tr2)

	if _, err := m2.Token(context.Background(), req("k", "x", hc2)); err == nil {
		t.Fatal("expected breaker-open error from the persisted cooldown")
	}
	if got := tr2.createCount.Load(); got != 0 {
		t.Fatalf("attested despite a persisted cooldown (Create=%d, want 0)", got)
	}
}

// TestBreakerSuccessClearsPersisted confirms a successful attestation clears the
// persisted cooldown so a recovered key does not stay artificially cooled down
// after a restart.
func TestBreakerSuccessClearsPersisted(t *testing.T) {
	store := newFakeBreakerStore()
	// Pre-seed an active cooldown for a different key to prove only the recovered
	// key is cleared.
	store.m["other"] = time.Now().Add(time.Hour)

	m := newTestManager(&fakeEngine{}, Options{BreakerStore: store})

	// Manually open the breaker for "k", then a success should clear it.
	b := m.breaker("k")
	b.RecordFailure()
	b.RecordFailure()
	b.RecordFailure()
	b.RecordFailure()
	b.RecordFailure() // default threshold 5 -> open + persist
	if !store.has("k") {
		t.Fatal("expected cooldown persisted for k")
	}
	b.RecordSuccess()
	if store.has("k") {
		t.Fatal("success did not clear the persisted cooldown for k")
	}
	if !store.has("other") {
		t.Fatal("clearing k must not touch another key's cooldown")
	}
}

// TestMetricsRecorded checks the session increments the metric set across a cold
// attestation and a warm mint.
func TestMetricsRecorded(t *testing.T) {
	mx := metrics.New()
	eng := &fakeEngine{}
	tr := &fakeTransport{genITBody: integrityBody}
	m := newTestManager(eng, Options{Metrics: mx})
	hc := clientWith(tr)

	if _, err := m.Token(context.Background(), req("k", "id1", hc)); err != nil {
		t.Fatalf("cold mint: %v", err)
	}
	if _, err := m.Token(context.Background(), req("k", "id2", hc)); err != nil {
		t.Fatalf("warm mint: %v", err)
	}

	if got := mx.Attestations.Value(); got != 1 {
		t.Errorf("attestations = %d, want 1 (one cold path)", got)
	}
	if got := mx.Snapshots.Value(); got != 1 {
		t.Errorf("snapshots = %d, want 1", got)
	}
	var sb strings.Builder
	_ = mx.WritePrometheus(&sb)
	if !strings.Contains(sb.String(), `waxseal_mints_total{kind="integrity"} 2`) {
		t.Errorf("expected 2 integrity mints in exposition:\n%s", sb.String())
	}
}

// TestMetricsAttestFailureStage checks a stage-tagged failure counter.
func TestMetricsAttestFailureStage(t *testing.T) {
	mx := metrics.New()
	eng := &fakeEngine{}
	tr := &fakeTransport{genITBody: func() (int, string) { return 500, "" }}
	m := newTestManager(eng, Options{Metrics: mx})
	hc := clientWith(tr)

	if _, err := m.Token(context.Background(), req("k", "x", hc)); err == nil {
		t.Fatal("expected GenerateIT failure")
	}
	var sb strings.Builder
	_ = mx.WritePrometheus(&sb)
	if !strings.Contains(sb.String(), `waxseal_attest_failures_total{stage="generateit"} 1`) {
		t.Errorf("expected a generateit-stage failure:\n%s", sb.String())
	}
}
