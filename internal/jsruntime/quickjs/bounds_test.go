package quickjs_test

import (
	"context"
	"testing"
	"time"

	"github.com/colespringer/waxseal/internal/jsruntime"
	"github.com/colespringer/waxseal/internal/jsruntime/quickjs"
)

// BotGuard is obfuscated, untrusted JS. These tests verify that rogue scripts
// return under the QuickJS limits (memory/stack/interrupt) or the outer wazero
// watchdog, and that the host process survives to run the next case.

// Deep recursion is bounded by JS_SetMaxStackSize (caught well below the wasm
// shadow stack), surfacing as a recoverable error instead of a host crash.
func TestDeepRecursionBounded(t *testing.T) {
	rt := newRT(t, newEngine(t, quickjs.Options{Watchdog: 5 * time.Second}))
	ctx := context.Background()

	_, err := rt.Eval(ctx, `(function f(n){ return f(n+1); })(0)`)
	if err == nil {
		t.Fatal("expected unbounded recursion to be caught")
	}
	// Whether QuickJS throws RangeError (soft) or the wasm stack traps (poison),
	// the host must still be alive: a fresh runtime works.
	t.Logf("deep recursion caught as: %v", err)
	rt2 := newRT(t, newEngine(t, quickjs.Options{}))
	if got, err := rt2.Eval(ctx, "1+1"); err != nil || string(got) != "2" {
		t.Fatalf("host unusable after recursion bomb: %s %v", got, err)
	}
}

// An allocation bomb is bounded by JS_SetMemoryLimit / the wasm memory cap; the
// VM throws OOM rather than taking down the process.
func TestAllocationBombBounded(t *testing.T) {
	rt := newRT(t, newEngine(t, quickjs.Options{
		MemoryLimitBytes: 32 << 20, // 32 MiB QuickJS cap
		MemoryLimitPages: 1024,     // 64 MiB wasm cap (above the QuickJS cap)
		Watchdog:         10 * time.Second,
	}))
	ctx := context.Background()

	_, err := rt.Eval(ctx, `
		let chunks = [];
		for (;;) { chunks.push(new Uint8Array(1 << 20).fill(1)); }`)
	if err == nil {
		t.Fatal("expected allocation bomb to be caught")
	}
	t.Logf("allocation bomb caught as: %v", err)

	// Host survives.
	rt2 := newRT(t, newEngine(t, quickjs.Options{}))
	if _, err := rt2.Eval(ctx, "({ok:true})"); err != nil {
		t.Fatalf("host unusable after alloc bomb: %v", err)
	}
}

// A runaway tight loop is stopped by the watchdog within bounded wall-clock
// time. Whichever layer fires (in-engine interrupt -> recoverable; outer wazero
// close -> poison), the call returns promptly and the host lives.
func TestWatchdogStopsRunawayLoop(t *testing.T) {
	rt := newRT(t, newEngine(t, quickjs.Options{Watchdog: 500 * time.Millisecond}))
	ctx := context.Background()

	start := time.Now()
	_, err := rt.Eval(ctx, `for(;;){}`)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected runaway loop to be stopped")
	}
	// In-engine deadline (500ms) or outer watchdog (500ms+2s), bounded generously.
	if elapsed > 4*time.Second {
		t.Fatalf("watchdog took too long: %v", elapsed)
	}
	if _, isJS := jsruntime.AsJSError(err); isJS {
		t.Logf("runaway loop caught in-engine (recoverable) after %v: %v", elapsed, err)
		if rt.Poisoned() {
			t.Fatal("in-engine interrupt should not poison")
		}
	} else {
		t.Logf("runaway loop caught by outer watchdog (poison) after %v: %v", elapsed, err)
		if !rt.Poisoned() {
			t.Fatal("outer-watchdog close must mark the runtime poisoned")
		}
	}
}

// Catastrophic backtracking regex can run inside BotGuard, and libregexp may not
// poll the in-engine interrupt. Either guard must stop it within bounded
// wall-clock time, and the host must survive.
func TestCatastrophicRegexBounded(t *testing.T) {
	rt := newRT(t, newEngine(t, quickjs.Options{Watchdog: time.Second}))
	ctx := context.Background()

	start := time.Now()
	// Classic exponential backtracking: (a+)+$ against a long non-matching input.
	_, err := rt.Eval(ctx, `/(a+)+$/.test("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa!")`)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected catastrophic regex to be stopped (or to complete)")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("regex not bounded: %v", elapsed)
	}
	t.Logf("catastrophic regex stopped after %v (poisoned=%v): %v", elapsed, rt.Poisoned(), err)

	// Host survives regardless of which guard fired.
	rt2 := newRT(t, newEngine(t, quickjs.Options{}))
	if _, err := rt2.Eval(ctx, "1+1"); err != nil {
		t.Fatalf("host unusable after ReDoS: %v", err)
	}
}

// Caller cancellation is honored at the Go boundary (before acquire), without
// being wired into the runtime's execution (so it can't poison a shared warm
// runtime mid-mint).
func TestCallerCancellationHonoredAtBoundary(t *testing.T) {
	rt := newRT(t, newEngine(t, quickjs.Options{}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	_, err := rt.Eval(ctx, "1+1")
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if rt.Poisoned() {
		t.Fatal("caller cancellation must not poison the runtime")
	}
	// A fresh (non-cancelled) call still works on the same runtime.
	if got, err := rt.Eval(context.Background(), "2+2"); err != nil || string(got) != "4" {
		t.Fatalf("post-cancel eval: %s %v", got, err)
	}
}

// A poisoned runtime refuses further work (callers must evict it). Deep
// recursion reliably traps the wasm shadow stack -> boundary fault -> poison.
func TestPoisonedRuntimeRefusesWork(t *testing.T) {
	rt := newRT(t, newEngine(t, quickjs.Options{Watchdog: 5 * time.Second}))
	ctx := context.Background()

	if _, err := rt.Eval(ctx, `(function f(){ return f(); })()`); err == nil {
		t.Fatal("expected recursion fault")
	}
	if !rt.Poisoned() {
		t.Fatal("deep recursion (wasm stack trap) must poison the runtime")
	}
	if _, err := rt.Eval(ctx, "1+1"); err != jsruntime.ErrPoisoned {
		t.Fatalf("poisoned runtime should return ErrPoisoned, got %v", err)
	}
}
