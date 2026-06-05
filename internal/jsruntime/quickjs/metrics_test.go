package quickjs_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/colespringer/waxseal/internal/botguard"
	"github.com/colespringer/waxseal/internal/jsassets"
	"github.com/colespringer/waxseal/internal/jsruntime/quickjs"
)

// TestGateAMetrics records cold init, warm runtime spawn, warm-mint latency,
// Go<->WASM payload sizes, and concurrency behavior. Run with -v to see the
// report.
func TestGateAMetrics(t *testing.T) {
	ctx := context.Background()

	// Artifact and payload sizes.
	t.Logf("artifact sizes: qjs.wasm=%d KB  bg_bundle.js=%d KB",
		len(jsassets.QJSWasm)/1024, len(jsassets.BGBundle)/1024)
	t.Logf("artifact sha256: qjs.wasm=%s… bg_bundle=%s…",
		jsassets.QJSWasmSHA256()[:12], jsassets.BGBundleSHA256()[:12])

	// Cold init: compile the module once.
	t0 := time.Now()
	eng, err := quickjs.NewEngine(ctx, jsassets.QJSWasm, quickjs.Options{PreloadBundle: jsassets.BGBundle})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close(ctx)
	coldInit := time.Since(t0) // CompileModule (AOT) dominates
	t.Logf("cold init (CompileModule incl. AOT): %v", coldInit)

	// Warm runtime spawn: instantiate, wx_init, and preload the bundle.
	const spawnN = 10
	var spawnTotal time.Duration
	var lastRT interface {
		Call(context.Context, string, ...any) (json.RawMessage, error)
		Close(context.Context) error
	}
	for i := 0; i < spawnN; i++ {
		s := time.Now()
		rt, err := eng.NewRuntime(ctx)
		if err != nil {
			t.Fatalf("NewRuntime: %v", err)
		}
		spawnTotal += time.Since(s)
		if lastRT != nil {
			lastRT.Close(ctx)
		}
		lastRT = rt
	}
	defer lastRT.Close(ctx)
	t.Logf("warm runtime spawn (instantiate+wx_init+bundle eval): %v avg over %d", spawnTotal/spawnN, spawnN)

	// Warm-mint latency: runBotguard, newMinter, and mint on the fake VM.
	// (minus network; the real GenerateIT round-trip is Go-side HTTP, not JS.)
	if _, err := lastRT.Call(ctx, "runBotguard", fakeVMInterpreter, "P", "fakeVM"); err != nil {
		t.Fatalf("runBotguard: %v", err)
	}
	if _, err := lastRT.Call(ctx, "newMinter", "ZmFrZS1pbnRlZ3JpdHk="); err != nil {
		t.Fatalf("newMinter: %v", err)
	}
	const mintN = 200
	s := time.Now()
	var lastTok string
	for i := 0; i < mintN; i++ {
		out, err := lastRT.Call(ctx, "mint", fmt.Sprintf("visitor-%d", i))
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		_ = json.Unmarshal(out, &lastTok)
	}
	warmMint := time.Since(s) / mintN
	t.Logf("warm mint (one warm minter, repeated): %v/op over %d", warmMint, mintN)
	t.Logf("payload: minted token = %d bytes", len(lastTok))

	// Cold full cycle: fresh runtime -> snapshot -> minter -> first mint.
	s = time.Now()
	rt2, _ := eng.NewRuntime(ctx)
	_, _ = rt2.Call(ctx, "runBotguard", fakeVMInterpreter, "P", "fakeVM")
	_, _ = rt2.Call(ctx, "newMinter", "ZmFrZS1pbnRlZ3JpdHk=")
	out, _ := rt2.Call(ctx, "mint", "cold-visitor")
	coldCycle := time.Since(s)
	rt2.Close(ctx)
	var tok string
	_ = json.Unmarshal(out, &tok)
	if _, err := botguard.ValidatePOToken(tok); err != nil {
		t.Fatalf("cold-cycle validate: %v", err)
	}
	t.Logf("cold full cycle (spawn+snapshot+minter+mint+validate): %v", coldCycle)
}

// Concurrency: many runtimes from one CompiledModule mint in parallel goroutines
// (each runtime single-goroutine-owned). Proves Engine.NewRuntime is safe for
// concurrent use and runtimes are independent.
func TestConcurrentMint(t *testing.T) {
	ctx := context.Background()
	eng := newEngine(t, quickjs.Options{PreloadBundle: jsassets.BGBundle})

	const workers, perWorker = 8, 25
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	start := time.Now()

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rt, err := eng.NewRuntime(ctx)
			if err != nil {
				errs <- fmt.Errorf("worker %d spawn: %w", w, err)
				return
			}
			defer rt.Close(ctx)
			if _, err := rt.Call(ctx, "runBotguard", fakeVMInterpreter, "P", "fakeVM"); err != nil {
				errs <- fmt.Errorf("worker %d runBotguard: %w", w, err)
				return
			}
			if _, err := rt.Call(ctx, "newMinter", "ZmFrZS1pbnRlZ3JpdHk="); err != nil {
				errs <- fmt.Errorf("worker %d newMinter: %w", w, err)
				return
			}
			for i := 0; i < perWorker; i++ {
				id := fmt.Sprintf("w%d-id%d", w, i)
				out, err := rt.Call(ctx, "mint", id)
				if err != nil {
					errs <- fmt.Errorf("worker %d mint: %w", w, err)
					return
				}
				var token string
				_ = json.Unmarshal(out, &token)
				f6, err := botguard.ValidatePOToken(token)
				if err != nil || string(f6) != id {
					errs <- fmt.Errorf("worker %d token f6=%q want %q err=%v", w, f6, id, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	t.Logf("concurrent mint: %d workers × %d mints = %d tokens in %v",
		workers, perWorker, workers*perWorker, time.Since(start))
}

// BenchmarkWarmMint measures steady-state mint latency on one warm minter.
func BenchmarkWarmMint(b *testing.B) {
	ctx := context.Background()
	eng, err := quickjs.NewEngine(ctx, jsassets.QJSWasm, quickjs.Options{PreloadBundle: jsassets.BGBundle})
	if err != nil {
		b.Fatal(err)
	}
	defer eng.Close(ctx)
	rt, _ := eng.NewRuntime(ctx)
	defer rt.Close(ctx)
	_, _ = rt.Call(ctx, "runBotguard", fakeVMInterpreter, "P", "fakeVM")
	_, _ = rt.Call(ctx, "newMinter", "ZmFrZS1pbnRlZ3JpdHk=")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := rt.Call(ctx, "mint", "bench-visitor"); err != nil {
			b.Fatal(err)
		}
	}
}
