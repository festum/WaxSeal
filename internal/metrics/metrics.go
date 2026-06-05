// Package metrics provides dependency-free counters for the minter and token
// cache: VM snapshots, mints by token kind, runtime poisons, breaker trips,
// refresh-ahead activity, and cache hits/misses. It renders the set in
// Prometheus text exposition format using only atomics and a small labeled
// counter.
//
// The zero value is not usable; call New. A nil *Metrics is a valid no-op target
// (every method tolerates it), so instrumented code needs no nil checks when a
// caller opts out.
package metrics

import (
	"fmt"
	"io"
	"sort"
	"sync"
	"sync/atomic"
)

// namespace prefixes every exported series.
const namespace = "waxseal_"

// Counter is a monotonic, concurrency-safe counter.
type Counter struct{ v atomic.Int64 }

// Inc adds one. A nil receiver is a no-op.
func (c *Counter) Inc() {
	if c != nil {
		c.v.Add(1)
	}
}

// Add adds n. A nil receiver is a no-op.
func (c *Counter) Add(n int64) {
	if c != nil {
		c.v.Add(n)
	}
}

// Value returns the current count.
func (c *Counter) Value() int64 {
	if c == nil {
		return 0
	}
	return c.v.Load()
}

// labeledCounter is a counter broken out by one label value (e.g. token kind or
// failure stage). Series are created on first use.
type labeledCounter struct {
	mu   sync.Mutex
	vals map[string]*Counter
}

func newLabeled() *labeledCounter { return &labeledCounter{vals: map[string]*Counter{}} }

// Inc increments the series for value, creating it on first use. A nil receiver
// is a no-op.
func (l *labeledCounter) Inc(value string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.vals[value]
	if c == nil {
		c = &Counter{}
		l.vals[value] = c
	}
	c.Inc()
}

// snapshot returns the series sorted by label value for stable exposition.
func (l *labeledCounter) snapshot() []labeledValue {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]labeledValue, 0, len(l.vals))
	for k, c := range l.vals {
		out = append(out, labeledValue{label: k, value: c.Value()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].label < out[j].label })
	return out
}

type labeledValue struct {
	label string
	value int64
}

// Metrics is WaxSeal's full metric set. Fields are grouped by where they are
// recorded: the warm-minter session and the token cache.
type Metrics struct {
	// Session / warm minter.
	Snapshots      *Counter        // BotGuard VM snapshots performed
	Attestations   *Counter        // cold attestations begun (challenge -> snapshot -> GenerateIT)
	Mints          *labeledCounter // tokens served, by kind (integrity|fallback)
	Poisons        *Counter        // runtimes poisoned at the wasm boundary and evicted
	BreakerOpens   *Counter        // breaker cool-down trips
	Refreshes      *Counter        // background refresh-ahead swaps completed
	RefreshErrors  *Counter        // refresh-ahead attempts that failed (kept serving current)
	Prewarms       *Counter        // background prewarm builds completed
	AttestFailures *labeledCounter // failed attestations, by stage

	// Token cache.
	CacheHits   *Counter
	CacheMisses *Counter
}

// New returns a ready metric set with all series initialized.
func New() *Metrics {
	return &Metrics{
		Snapshots:      &Counter{},
		Attestations:   &Counter{},
		Mints:          newLabeled(),
		Poisons:        &Counter{},
		BreakerOpens:   &Counter{},
		Refreshes:      &Counter{},
		RefreshErrors:  &Counter{},
		Prewarms:       &Counter{},
		AttestFailures: newLabeled(),
		CacheHits:      &Counter{},
		CacheMisses:    &Counter{},
	}
}

// MintKind records one served token of the given kind ("integrity"/"fallback").
func (m *Metrics) MintKind(kind string) {
	if m != nil {
		m.Mints.Inc(kind)
	}
}

// AttestFailure records one failed attestation attributed to a pipeline stage.
func (m *Metrics) AttestFailure(stage string) {
	if m != nil {
		m.AttestFailures.Inc(stage)
	}
}

// WritePrometheus renders the metric set in Prometheus text exposition format. A
// nil *Metrics writes nothing.
func (m *Metrics) WritePrometheus(w io.Writer) error {
	if m == nil {
		return nil
	}
	for _, s := range []struct {
		name, help string
		c          *Counter
	}{
		{"snapshots_total", "BotGuard VM snapshots performed.", m.Snapshots},
		{"attestations_total", "Cold attestations begun (challenge -> snapshot -> GenerateIT).", m.Attestations},
		{"runtime_poisons_total", "Runtimes poisoned at the wasm boundary and evicted.", m.Poisons},
		{"breaker_opens_total", "Circuit breaker cool-down trips.", m.BreakerOpens},
		{"refreshes_total", "Background refresh-ahead swaps completed.", m.Refreshes},
		{"refresh_errors_total", "Refresh-ahead attempts that failed (current token kept serving).", m.RefreshErrors},
		{"prewarms_total", "Background prewarm builds completed.", m.Prewarms},
		{"cache_hits_total", "Token cache hits.", m.CacheHits},
		{"cache_misses_total", "Token cache misses.", m.CacheMisses},
	} {
		if err := writeCounter(w, s.name, s.help, s.c.Value()); err != nil {
			return err
		}
	}
	if err := writeLabeled(w, "mints_total", "Tokens served, by kind.", "kind", m.Mints); err != nil {
		return err
	}
	return writeLabeled(w, "attest_failures_total", "Failed attestations, by pipeline stage.", "stage", m.AttestFailures)
}

func writeCounter(w io.Writer, name, help string, value int64) error {
	if _, err := fmt.Fprintf(w, "# HELP %s%s %s\n# TYPE %s%s counter\n%s%s %d\n",
		namespace, name, help, namespace, name, namespace, name, value); err != nil {
		return err
	}
	return nil
}

func writeLabeled(w io.Writer, name, help, label string, lc *labeledCounter) error {
	if _, err := fmt.Fprintf(w, "# HELP %s%s %s\n# TYPE %s%s counter\n", namespace, name, help, namespace, name); err != nil {
		return err
	}
	for _, s := range lc.snapshot() {
		if _, err := fmt.Fprintf(w, "%s%s{%s=%q} %d\n", namespace, name, label, s.label, s.value); err != nil {
			return err
		}
	}
	return nil
}
