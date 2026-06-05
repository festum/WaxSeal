package metrics

import (
	"strings"
	"testing"
)

func TestCountersAndExposition(t *testing.T) {
	m := New()
	m.Snapshots.Inc()
	m.Snapshots.Inc()
	m.Attestations.Inc()
	m.CacheHits.Add(3)
	m.MintKind("integrity")
	m.MintKind("integrity")
	m.MintKind("fallback")
	m.AttestFailure("generateit")
	m.Poisons.Inc()

	var sb strings.Builder
	if err := m.WritePrometheus(&sb); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}
	out := sb.String()

	for _, want := range []string{
		"# TYPE waxseal_snapshots_total counter",
		"waxseal_snapshots_total 2",
		"waxseal_attestations_total 1",
		"waxseal_cache_hits_total 3",
		`waxseal_mints_total{kind="integrity"} 2`,
		`waxseal_mints_total{kind="fallback"} 1`,
		`waxseal_attest_failures_total{stage="generateit"} 1`,
		"waxseal_runtime_poisons_total 1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("exposition missing %q\n---\n%s", want, out)
		}
	}
}

// TestNilSafe confirms a nil *Metrics and nil counters are no-ops, so opt-out
// callers need no guards.
func TestNilSafe(t *testing.T) {
	var m *Metrics
	m.MintKind("integrity") // must not panic
	m.AttestFailure("vm")
	var c *Counter
	c.Inc()
	if c.Value() != 0 {
		t.Fatal("nil counter should read 0")
	}
	var sb strings.Builder
	if err := m.WritePrometheus(&sb); err != nil {
		t.Fatalf("nil WritePrometheus: %v", err)
	}
	if sb.Len() != 0 {
		t.Fatalf("nil Metrics wrote %q", sb.String())
	}
}

func TestLabeledStableOrder(t *testing.T) {
	m := New()
	for _, s := range []string{"vm", "generateit", "challenge", "vm"} {
		m.AttestFailure(s)
	}
	var sb strings.Builder
	_ = m.WritePrometheus(&sb)
	out := sb.String()
	// Sorted by label: challenge < generateit < vm.
	ci := strings.Index(out, `stage="challenge"`)
	gi := strings.Index(out, `stage="generateit"`)
	vi := strings.Index(out, `stage="vm"`)
	if !(ci < gi && gi < vi) {
		t.Fatalf("labels not in sorted order: challenge=%d generateit=%d vm=%d", ci, gi, vi)
	}
	if !strings.Contains(out, `stage="vm"} 2`) {
		t.Fatalf("vm count wrong:\n%s", out)
	}
}
