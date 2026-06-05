package quickjs_test

import (
	"context"
	"encoding/json"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxseal/internal/botguard"
	"github.com/colespringer/waxseal/internal/jsassets"
	"github.com/colespringer/waxseal/internal/jsruntime/quickjs"
)

// residentKB reads RSS (resident pages) from /proc/self/statm: the real
// process footprint, which includes wazero's wasm linear memory.
func residentKB(t *testing.T) int64 {
	t.Helper()
	b, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		t.Skipf("no /proc/self/statm: %v", err)
	}
	fields := strings.Fields(string(b))
	if len(fields) < 2 {
		t.Skip("unexpected statm format")
	}
	residentPages, _ := strconv.ParseInt(fields[1], 10, 64)
	return residentPages * int64(os.Getpagesize()) / 1024
}

// mintOnce runs the full create -> snapshot -> minter -> mint cycle on a fresh
// runtime using the fake VM, then evicts (Close) it.
func mintOnce(t *testing.T, eng *quickjs.Engine) {
	t.Helper()
	ctx := context.Background()
	rt, err := eng.NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx) // evict: module.Close() must return the linear memory

	if _, err := rt.Call(ctx, "runBotguard", fakeVMInterpreter, "P", "fakeVM"); err != nil {
		t.Fatalf("runBotguard: %v", err)
	}
	if _, err := rt.Call(ctx, "newMinter", "ZmFrZS1pbnRlZ3JpdHk="); err != nil {
		t.Fatalf("newMinter: %v", err)
	}
	out, err := rt.Call(ctx, "mint", "visitor-data-soak")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	var token string
	_ = json.Unmarshal(out, &token)
	if _, err := botguard.ValidatePOToken(token); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

// Leak/OOM soak: many create -> mint -> evict cycles must keep RSS flat because
// module.Close returns the memory.
func TestLeakSoak(t *testing.T) {
	if testing.Short() {
		t.Skip("soak skipped in -short")
	}
	eng := newEngine(t, quickjs.Options{PreloadBundle: jsassets.BGBundle})
	ctx := context.Background()

	const warmup, cycles = 20, 300

	for i := 0; i < warmup; i++ {
		mintOnce(t, eng)
	}
	runtime.GC()
	startRSS := residentKB(t)

	for i := 0; i < cycles; i++ {
		mintOnce(t, eng)
	}
	runtime.GC()
	// Give the OS/allocator a beat to settle.
	time.Sleep(50 * time.Millisecond)
	runtime.GC()
	endRSS := residentKB(t)

	growthKB := endRSS - startRSS
	growthPerCycleKB := float64(growthKB) / float64(cycles)
	t.Logf("RSS: start=%d KB end=%d KB growth=%d KB over %d cycles (%.2f KB/cycle)",
		startRSS, endRSS, growthKB, cycles, growthPerCycleKB)

	_ = ctx
	// A genuine per-cycle leak of a fresh QuickJS heap + minter would be on the
	// order of MB/cycle. Allow generous slack for allocator/GC noise but catch
	// real leaks: < 64 KB/cycle averaged.
	if growthPerCycleKB > 64 {
		t.Fatalf("RSS grew %.2f KB/cycle; likely a leak (module.Close not returning memory)", growthPerCycleKB)
	}
}

// module.Close() must return wasm linear memory: spawn many runtimes without
// closing to confirm they hold memory, then close all and confirm it drops.
func TestCloseReturnsMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped in -short")
	}
	eng := newEngine(t, quickjs.Options{MemoryLimitPages: 1024})
	ctx := context.Background()

	debug.FreeOSMemory()
	base := residentKB(t)

	const n = 40
	rts := make([]interface{ Close(context.Context) error }, 0, n)
	for i := 0; i < n; i++ {
		rt, err := eng.NewRuntime(ctx)
		if err != nil {
			t.Fatalf("NewRuntime %d: %v", i, err)
		}
		// Touch memory so the instance actually commits pages.
		if _, err := rt.Eval(ctx, `(() => { const a = new Uint8Array(1<<20).fill(7); return a.length; })()`); err != nil {
			t.Fatalf("touch: %v", err)
		}
		rts = append(rts, rt)
	}
	runtime.GC()
	held := residentKB(t)

	for _, rt := range rts {
		_ = rt.Close(ctx)
	}
	// Force Go to return the freed linear memory to the OS so RSS reflects it.
	debug.FreeOSMemory()
	time.Sleep(50 * time.Millisecond)
	debug.FreeOSMemory()
	afterClose := residentKB(t)

	t.Logf("RSS: base=%d KB held(%d live)=%d KB afterClose=%d KB", base, n, held, afterClose)
	// After closing all, RSS should fall well below the peak-held level.
	if afterClose >= held {
		t.Fatalf("RSS did not drop after Close (held=%d afterClose=%d); memory not returned", held, afterClose)
	}
}
