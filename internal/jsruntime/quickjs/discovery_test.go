package quickjs_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/colespringer/waxseal/internal/jsruntime"
)

// These tests exercise discovery traps without network access or live BotGuard
// code. Each case updates the live discovery flags through globalThis.

// mustEval evaluates src for its effect and fails the test on a JS/host error.
func mustEval(t *testing.T, rt jsruntime.Runtime, src string) {
	t.Helper()
	if _, err := rt.Eval(context.Background(), src); err != nil {
		t.Fatalf("eval %q: %v", src, err)
	}
}

// getProbes reads the structured probe set the trap accumulated (sorted).
func getProbes(t *testing.T, rt jsruntime.Runtime) []string {
	t.Helper()
	out, err := rt.Eval(context.Background(), "globalThis.__wxGetProbes()")
	if err != nil {
		t.Fatalf("__wxGetProbes(): %v", err)
	}
	var probes []string
	if err := json.Unmarshal(out, &probes); err != nil {
		t.Fatalf("decode probes %s: %v", out, err)
	}
	return probes
}

func wantProbe(t *testing.T, probes []string, path string) {
	t.Helper()
	if !slices.Contains(probes, path) {
		t.Errorf("probe %q not collected; got %v", path, probes)
	}
}

// TestDiscoveryAutoStubCollectsProbes verifies each supported access path.
func TestDiscoveryAutoStubCollectsProbes(t *testing.T) {
	rt := newBundledRT(t)
	mustEval(t, rt, "globalThis.__wxAutoStub=true; globalThis.__wxClearProbes();")

	mustEval(t, rt, "void window.__wxNopeGet;")
	mustEval(t, rt, "void ('__wxNopeIn' in window);")
	mustEval(t, rt, "void Object.getOwnPropertyDescriptor(navigator,'__wxNopeGopd');")

	probes := getProbes(t, rt)
	wantProbe(t, probes, "window.__wxNopeGet")
	wantProbe(t, probes, "window.__wxNopeIn")
	wantProbe(t, probes, "navigator.__wxNopeGopd")
}

// TestDiscoveryProductionFailsClosed verifies that discovery records missing
// properties without making them appear to exist.
func TestDiscoveryProductionFailsClosed(t *testing.T) {
	rt := newBundledRT(t)
	mustEval(t, rt, "globalThis.__wxAutoStub=false; globalThis.__wxDiscovery=true; globalThis.__wxClearProbes();")

	evalTrue(t, rt, "get fails closed", "window.__wxNopeGet === undefined")
	evalTrue(t, rt, "in fails closed", "('__wxNopeIn' in window) === false")
	evalTrue(t, rt, "gopd fails closed", "Object.getOwnPropertyDescriptor(window,'__wxNopeGopd') === undefined")

	probes := getProbes(t, rt)
	wantProbe(t, probes, "window.__wxNopeGet")
	wantProbe(t, probes, "window.__wxNopeIn")
	wantProbe(t, probes, "window.__wxNopeGopd")
}

// TestDiscoveryInheritedIsNotDrift verifies that reachable inherited properties
// are not reported as missing APIs.
func TestDiscoveryInheritedIsNotDrift(t *testing.T) {
	rt := newBundledRT(t)
	mustEval(t, rt, "globalThis.__wxClearProbes();")

	evalTrue(t, rt, "createElement own-absent", "Object.getOwnPropertyDescriptor(document,'createElement') === undefined")
	evalTrue(t, rt, "createElement reachable", "typeof document.createElement === 'function'")

	if probes := getProbes(t, rt); slices.Contains(probes, "document.createElement") {
		t.Errorf("inherited document.createElement wrongly reported as drift; probes=%v", probes)
	}
}

// TestDiscoveryEmptyKeyIsNotDrift verifies that empty property names are ignored.
func TestDiscoveryEmptyKeyIsNotDrift(t *testing.T) {
	rt := newBundledRT(t)
	mustEval(t, rt, "globalThis.__wxClearProbes();")

	evalTrue(t, rt, "get empty-key", "document[''] === undefined")
	evalTrue(t, rt, "in empty-key", "('' in document) === false")
	evalTrue(t, rt, "gopd empty-key", "Object.getOwnPropertyDescriptor(document,'') === undefined")

	if probes := getProbes(t, rt); slices.Contains(probes, "document.") {
		t.Errorf("empty-key access wrongly recorded as drift; probes=%v", probes)
	}
}
