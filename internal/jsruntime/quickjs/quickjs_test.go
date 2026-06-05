package quickjs_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/colespringer/waxseal/internal/jsassets"
	"github.com/colespringer/waxseal/internal/jsruntime"
	"github.com/colespringer/waxseal/internal/jsruntime/quickjs"
)

func newEngine(t *testing.T, opts quickjs.Options) *quickjs.Engine {
	t.Helper()
	eng, err := quickjs.NewEngine(context.Background(), jsassets.QJSWasm, opts)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close(context.Background()) })
	return eng
}

func newRT(t *testing.T, eng *quickjs.Engine) jsruntime.Runtime {
	t.Helper()
	rt, err := eng.NewRuntime(context.Background())
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	return rt
}

// newBundledRT loads the real bg_bundle (shim + bgutils-js + entrypoint) into a
// runtime, exercising setTimeout/__wx_runTimers and proving the bundle loads
// under QuickJS (not only Node).
func newBundledRT(t *testing.T) jsruntime.Runtime {
	t.Helper()
	eng := newEngine(t, quickjs.Options{PreloadBundle: jsassets.BGBundle})
	return newRT(t, eng)
}

// Eval and string round-trip through the length-prefixed ABI.
func TestEvalAndStringRoundTrip(t *testing.T) {
	rt := newRT(t, newEngine(t, quickjs.Options{}))
	ctx := context.Background()

	got, err := rt.Eval(ctx, "1 + 1")
	if err != nil {
		t.Fatalf("eval 1+1: %v", err)
	}
	if string(got) != "2" {
		t.Fatalf("1+1 = %s, want 2", got)
	}

	// Non-ASCII string survives the UTF-8 boundary intact.
	var s string
	out, err := rt.Eval(ctx, `"héllo " + "wörld 日本 🎵"`)
	if err != nil {
		t.Fatalf("eval string: %v", err)
	}
	if err := json.Unmarshal(out, &s); err != nil {
		t.Fatalf("unmarshal: %v (raw %s)", err, out)
	}
	if s != "héllo wörld 日本 🎵" {
		t.Fatalf("string round-trip = %q", s)
	}

	// Object -> JSON.
	out, err = rt.Eval(ctx, `({a:1,b:[2,3],c:"x"})`)
	if err != nil {
		t.Fatalf("eval object: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("obj unmarshal: %v (%s)", err, out)
	}
	if m["a"].(float64) != 1 || m["c"].(string) != "x" {
		t.Fatalf("object = %s", out)
	}
}

// A JS exception is a recoverable error; the runtime stays usable.
func TestJSExceptionIsRecoverable(t *testing.T) {
	rt := newRT(t, newEngine(t, quickjs.Options{}))
	ctx := context.Background()

	_, err := rt.Eval(ctx, `throw new TypeError("boom")`)
	if err == nil {
		t.Fatal("expected error")
	}
	if _, ok := jsruntime.AsJSError(err); !ok {
		t.Fatalf("want *JSError, got %T: %v", err, err)
	}
	if rt.Poisoned() {
		t.Fatal("JS exception must not poison the runtime")
	}
	// Still usable.
	if got, err := rt.Eval(ctx, "3 * 4"); err != nil || string(got) != "12" {
		t.Fatalf("post-exception eval: %s %v", got, err)
	}
}

// A Promise resolved purely via microtasks is driven to
// completion by the pump (no virtual timer needed).
func TestPromiseMicrotaskPump(t *testing.T) {
	rt := newRT(t, newEngine(t, quickjs.Options{}))
	got, err := rt.Eval(context.Background(),
		`Promise.resolve(1).then(x => x + 1).then(x => x * 21)`)
	if err != nil {
		t.Fatalf("promise eval: %v", err)
	}
	if string(got) != "42" {
		t.Fatalf("promise chain = %s, want 42", got)
	}
}

// A Promise that only settles when a setTimeout fires
// proves the virtual-timer pump and the "microtasks before timers" ordering.
func TestPromiseSetTimeoutPump(t *testing.T) {
	rt := newBundledRT(t)
	got, err := rt.Eval(context.Background(), `
		new Promise((resolve) => {
			let log = [];
			Promise.resolve().then(() => log.push("micro"));
			setTimeout(() => { log.push("timer"); resolve(log.join(",")); }, 50);
		})`)
	if err != nil {
		t.Fatalf("settimeout promise: %v", err)
	}
	var s string
	_ = json.Unmarshal(got, &s)
	if s != "micro,timer" {
		t.Fatalf("ordering = %q, want micro,timer (microtasks drain before timers)", s)
	}
}

// Promise.race(work, setTimeout(reject)) must resolve to real work, never the
// synthetic timeout, matching the botGuardClient.ts:73 snapshot pattern.
func TestPromiseRaceRealWorkBeatsTimeout(t *testing.T) {
	rt := newBundledRT(t)
	got, err := rt.Eval(context.Background(), `
		Promise.race([
			Promise.resolve().then(() => "real"),
			new Promise((_, reject) => setTimeout(() => reject("TIMEOUT"), 3000))
		])`)
	if err != nil {
		t.Fatalf("race: %v", err)
	}
	var s string
	_ = json.Unmarshal(got, &s)
	if s != "real" {
		t.Fatalf("race = %q, want real (synthetic timeout must not beat real progress)", s)
	}
}

// CSPRNG entropy is wired through __wx_random_fill (WASI random_get ->
// crypto/rand). Two fills differ; output is non-trivial.
func TestCSPRNGEntropy(t *testing.T) {
	rt := newRT(t, newEngine(t, quickjs.Options{}))
	ctx := context.Background()

	fill := func() []int {
		out, err := rt.Eval(ctx, `(() => { const a = new Uint8Array(32); __wx_random_fill(a); return Array.from(a); })()`)
		if err != nil {
			t.Fatalf("random_fill: %v", err)
		}
		var v []int
		if err := json.Unmarshal(out, &v); err != nil {
			t.Fatalf("unmarshal fill: %v (%s)", err, out)
		}
		if len(v) != 32 {
			t.Fatalf("len = %d", len(v))
		}
		return v
	}
	a, b := fill(), fill()
	same, allZero := true, true
	for i := range a {
		if a[i] != b[i] {
			same = false
		}
		if a[i] != 0 {
			allZero = false
		}
	}
	if same {
		t.Fatal("two CSPRNG fills identical; entropy not wired")
	}
	if allZero {
		t.Fatal("CSPRNG produced all zeros")
	}

	// Math.random override + __wx_random_double in [0,1).
	out, err := rt.Eval(ctx, `__wx_random_double()`)
	if err != nil {
		t.Fatalf("random_double: %v", err)
	}
	var d float64
	_ = json.Unmarshal(out, &d)
	if d < 0 || d >= 1 {
		t.Fatalf("random double out of range: %v", d)
	}
}

// Many isolated runtimes from one CompiledModule, each with independent state.
func TestRuntimeIsolation(t *testing.T) {
	eng := newEngine(t, quickjs.Options{})
	ctx := context.Background()

	r1, _ := eng.NewRuntime(ctx)
	r2, _ := eng.NewRuntime(ctx)
	defer r1.Close(ctx)
	defer r2.Close(ctx)

	if _, err := r1.Eval(ctx, `globalThis.X = 111`); err != nil {
		t.Fatalf("r1 set: %v", err)
	}
	// r2 must not see r1's global.
	out, err := r2.Eval(ctx, `typeof globalThis.X`)
	if err != nil {
		t.Fatalf("r2 read: %v", err)
	}
	var s string
	_ = json.Unmarshal(out, &s)
	if s != "undefined" {
		t.Fatalf("isolation breached: r2 sees X (%s)", out)
	}
}

func TestWatchdogDefaultUnset(t *testing.T) {
	// Sanity: a sub-second op completes well within the default watchdog.
	rt := newRT(t, newEngine(t, quickjs.Options{Watchdog: 2 * time.Second}))
	if _, err := rt.Eval(context.Background(), `let s=0; for(let i=0;i<100000;i++) s+=i; s`); err != nil {
		t.Fatalf("tight loop within watchdog: %v", err)
	}
}
