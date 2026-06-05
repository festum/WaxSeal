// Package quickjs is the sole jsruntime.Runtime backend: quickjs-ng compiled to
// a WASI reactor (build/wasm/host.c + qjs.wasm), executed by wazero. It is pure
// Go, uses no CGo, and is architecture-neutral. It compiles the module once and
// instantiates many isolated runtimes, wiring entropy to a CSPRNG and bounding
// untrusted VM code with a memory cap, a stack cap, an in-engine interrupt
// deadline, and an outer wazero context watchdog.
package quickjs

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"

	"github.com/colespringer/waxseal/internal/jsruntime"
)

// Options tunes the engine and the runtimes it spawns.
type Options struct {
	// MemoryLimitBytes caps QuickJS's own allocation accounting (JS_SetMemoryLimit).
	// A breach throws a catchable JS OutOfMemory, not a host crash. Default 64 MiB.
	MemoryLimitBytes uint32
	// MaxStackBytes caps JS recursion depth (JS_SetMaxStackSize). Default 1 MiB,
	// below the 4 MiB wasm shadow stack so QuickJS throws before a wasm trap.
	MaxStackBytes uint32
	// MemoryLimitPages caps the wasm linear memory (1 page = 64 KiB). Default 1280
	// pages (80 MiB), above MemoryLimitBytes so QuickJS's softer limit fires first.
	MemoryLimitPages uint32
	// Watchdog is the default in-engine interrupt deadline per Eval/Call. The
	// outer wazero context watchdog is layered on top per call. Default 5s.
	Watchdog time.Duration
	// Stderr receives VM console.* output (API-drift probes, timer errors).
	Stderr io.Writer
	// CompilationCacheDir persists wazero's AOT compilation so container
	// restarts skip the ~hundreds-of-ms recompile. Empty = none.
	CompilationCacheDir string
	// PreloadBundle, when non-nil, is evaluated in each new runtime at creation
	// (the bg_bundle: shim + bgutils-js + entrypoint). Empty = bare engine.
	PreloadBundle []byte
}

func (o Options) withDefaults() Options {
	if o.MemoryLimitBytes == 0 {
		o.MemoryLimitBytes = 64 << 20
	}
	if o.MaxStackBytes == 0 {
		o.MaxStackBytes = 1 << 20
	}
	if o.MemoryLimitPages == 0 {
		o.MemoryLimitPages = 1280 // 80 MiB
	}
	if o.Watchdog == 0 {
		o.Watchdog = 5 * time.Second
	}
	if o.Stderr == nil {
		o.Stderr = io.Discard
	}
	return o
}

// Engine holds the compile-once CompiledModule reused for every instance.
type Engine struct {
	rt       wazero.Runtime
	compiled wazero.CompiledModule
	cache    wazero.CompilationCache
	opts     Options
}

// NewEngine compiles qjs.wasm a single time. wasm is the embedded jsassets.QJSWasm.
func NewEngine(ctx context.Context, wasm []byte, opts Options) (*Engine, error) {
	opts = opts.withDefaults()

	rcfg := wazero.NewRuntimeConfig().
		// The outer, Go-owned watchdog: a per-call context deadline interrupts
		// runaway VM code wazero-side even if QuickJS's interrupt handler can't
		// (e.g. a pathological regex in libregexp).
		WithCloseOnContextDone(true).
		WithMemoryLimitPages(opts.MemoryLimitPages)

	var cache wazero.CompilationCache
	if opts.CompilationCacheDir != "" {
		c, err := wazero.NewCompilationCacheWithDir(opts.CompilationCacheDir)
		if err != nil {
			return nil, fmt.Errorf("quickjs: compilation cache: %w", err)
		}
		cache = c
		rcfg = rcfg.WithCompilationCache(c)
	}

	r := wazero.NewRuntimeWithConfig(ctx, rcfg)
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, r); err != nil {
		_ = r.Close(ctx)
		return nil, fmt.Errorf("quickjs: wasi instantiate: %w", err)
	}

	compiled, err := r.CompileModule(ctx, wasm)
	if err != nil {
		_ = r.Close(ctx)
		return nil, fmt.Errorf("quickjs: compile module: %w", err)
	}

	return &Engine{rt: r, compiled: compiled, cache: cache, opts: opts}, nil
}

// Close releases the compiled module, the wazero runtime, and the cache.
func (e *Engine) Close(ctx context.Context) error {
	err := e.rt.Close(ctx)
	if e.cache != nil {
		_ = e.cache.Close(ctx)
	}
	return err
}

// NewRuntime instantiates a fresh, isolated runtime and preloads the bundle.
func (e *Engine) NewRuntime(ctx context.Context) (jsruntime.Runtime, error) {
	cfg := wazero.NewModuleConfig().
		// Reactor model: run libc/global ctors, but not a _start main loop.
		WithStartFunctions("_initialize").
		// Anonymous: allow many concurrent instances from one CompiledModule.
		WithName("").
		// Wire WASI random_get to a CSPRNG explicitly rather than relying on a
		// runtime default.
		WithRandSource(rand.Reader).
		WithStdout(io.Discard).
		WithStderr(e.opts.Stderr).
		// Monotonic + wall clock for clock_time_get (the interrupt deadline).
		WithSysWalltime().
		WithSysNanotime().
		WithSysNanosleep()

	mod, err := e.rt.InstantiateModule(ctx, e.compiled, cfg)
	if err != nil {
		return nil, fmt.Errorf("quickjs: instantiate: %w", err)
	}

	rt := &runtime{
		mod:      mod,
		mem:      mod.Memory(),
		alloc:    mod.ExportedFunction("wx_alloc"),
		free:     mod.ExportedFunction("wx_free"),
		evalFn:   mod.ExportedFunction("wx_eval"),
		callFn:   mod.ExportedFunction("wx_call"),
		deadline: mod.ExportedFunction("wx_set_deadline_ms"),
		initFn:   mod.ExportedFunction("wx_init"),
		watchdog: e.opts.Watchdog,
	}
	for name, fn := range map[string]api.Function{
		"wx_alloc": rt.alloc, "wx_free": rt.free, "wx_eval": rt.evalFn,
		"wx_call": rt.callFn, "wx_set_deadline_ms": rt.deadline, "wx_init": rt.initFn,
	} {
		if fn == nil {
			_ = mod.Close(ctx)
			return nil, fmt.Errorf("quickjs: missing export %q", name)
		}
	}

	// wx_init(memoryLimit, maxStack): set up the JSRuntime + host builtins.
	res, err := rt.initFn.Call(ctx, uint64(e.opts.MemoryLimitBytes), uint64(e.opts.MaxStackBytes))
	if err != nil {
		_ = mod.Close(ctx)
		return nil, jsruntime.WrapBoundary("wx_init", err)
	}
	if len(res) == 0 || int32(res[0]) != 0 {
		_ = mod.Close(ctx)
		return nil, fmt.Errorf("quickjs: wx_init returned %v", res)
	}

	if len(e.opts.PreloadBundle) > 0 {
		if _, err := rt.Eval(ctx, string(e.opts.PreloadBundle)); err != nil {
			_ = rt.Close(ctx)
			return nil, fmt.Errorf("quickjs: preload bundle: %w", err)
		}
	}
	return rt, nil
}

// runtime is one JS execution context (one wasm instance). Single-goroutine.
type runtime struct {
	mod      api.Module
	mem      api.Memory
	alloc    api.Function
	free     api.Function
	evalFn   api.Function
	callFn   api.Function
	deadline api.Function
	initFn   api.Function

	watchdog time.Duration
	poisoned bool
}

func (r *runtime) SetWatchdog(d time.Duration) { r.watchdog = d }
func (r *runtime) Poisoned() bool              { return r.poisoned }

func (r *runtime) Close(ctx context.Context) error {
	// Closing the module frees the entire linear memory (and thus the QuickJS
	// runtime); module.Close() returning memory is what the leak soak asserts.
	return r.mod.Close(ctx)
}

// Eval evaluates src as a global script.
func (r *runtime) Eval(ctx context.Context, src string) (json.RawMessage, error) {
	if r.poisoned {
		return nil, jsruntime.ErrPoisoned
	}
	if err := ctx.Err(); err != nil { // honor caller cancellation at the boundary
		return nil, err
	}

	ptr, err := r.writeBytes([]byte(src))
	if err != nil {
		return nil, err
	}
	defer r.freePtr(ptr)

	wdCtx, cancel := r.armWatchdog()
	defer cancel()

	res, err := r.evalFn.Call(wdCtx, uint64(ptr), uint64(len(src)))
	if err != nil {
		return nil, r.boundaryFault("wx_eval", err)
	}
	return r.readResult(res[0])
}

// Call invokes globalThis[name](...args).
func (r *runtime) Call(ctx context.Context, name string, args ...any) (json.RawMessage, error) {
	if r.poisoned {
		return nil, jsruntime.ErrPoisoned
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var argsJSON []byte
	if len(args) > 0 {
		b, err := json.Marshal(args)
		if err != nil {
			return nil, fmt.Errorf("quickjs: marshal args: %w", err)
		}
		argsJSON = b
	}

	// One buffer holds name||argsJSON; pass offsets into it.
	combined := append([]byte(name), argsJSON...)
	ptr, err := r.writeBytes(combined)
	if err != nil {
		return nil, err
	}
	defer r.freePtr(ptr)

	wdCtx, cancel := r.armWatchdog()
	defer cancel()

	res, err := r.callFn.Call(wdCtx,
		uint64(ptr), uint64(len(name)),
		uint64(ptr)+uint64(len(name)), uint64(len(argsJSON)))
	if err != nil {
		return nil, r.boundaryFault("wx_call", err)
	}
	return r.readResult(res[0])
}

// armWatchdog sets the in-engine interrupt deadline and returns a separate
// (non-caller) context bounding the wasm call. The caller's ctx deliberately
// does not close the runtime: a cancelled request must not poison a runtime
// that may be shared/warm. Cancellation is honored at Go boundaries instead.
func (r *runtime) armWatchdog() (context.Context, context.CancelFunc) {
	if r.watchdog <= 0 {
		return context.Background(), func() {}
	}
	// In-engine soft guard (catchable JS exception) at the watchdog duration.
	// Background (not the caller ctx): setting the deadline is a small wasm call,
	// and a cancelled caller context would trip WithCloseOnContextDone and close
	// the possibly shared runtime. See freePtr.
	_, _ = r.deadline.Call(context.Background(), uint64(r.watchdog.Milliseconds()))
	// Outer hard guard slightly later: if QuickJS's interrupt can't catch it
	// (pathological regex), wazero closes the module -> poison.
	return context.WithTimeout(context.Background(), r.watchdog+2*time.Second)
}

// boundaryFault marks the runtime poisoned (a wasm-level fault: outer-watchdog
// close or a trap) and wraps the error for telemetry/eviction.
func (r *runtime) boundaryFault(stage string, err error) error {
	r.poisoned = true
	return jsruntime.WrapBoundary(stage, err)
}

// readResult decodes the packed (ptr<<32|len) result buffer: [status][payload].
func (r *runtime) readResult(packed uint64) (json.RawMessage, error) {
	ptr := uint32(packed >> 32)
	length := uint32(packed)
	if ptr == 0 || length == 0 {
		return nil, jsruntime.WrapBoundary("readResult", fmt.Errorf("null result (oom?)"))
	}
	raw, ok := r.mem.Read(ptr, length)
	if !ok {
		return nil, jsruntime.WrapBoundary("readResult", fmt.Errorf("result out of range ptr=%d len=%d", ptr, length))
	}
	// Copy out before freeing (the slice aliases wasm memory).
	status := raw[0]
	payload := make([]byte, length-1)
	copy(payload, raw[1:])
	r.freePtr(ptr)

	if status == 1 {
		return nil, &jsruntime.JSError{Message: string(payload)}
	}
	return json.RawMessage(payload), nil
}

// writeBytes copies b into wasm linear memory and returns its pointer. It
// always allocates one extra byte and writes a trailing NUL: QuickJS's JS_Eval
// / JS_ParseJSON read buf[len] (they assume a NUL-terminated buffer even though
// a length is passed), so without this a garbage trailing byte corrupts parsing.
// The NUL is past the logical length, so callers still pass len(b).
//
// alloc runs on context.Background(), not the caller ctx: it is a trivial wasm
// call, and the caller's ctx is already honored at the Go boundary (Eval/Call
// check ctx.Err() first). Passing a cancelled ctx here would trip
// WithCloseOnContextDone and close the (possibly shared/warm) runtime. See freePtr.
func (r *runtime) writeBytes(b []byte) (uint32, error) {
	res, err := r.alloc.Call(context.Background(), uint64(len(b)+1))
	if err != nil {
		return 0, r.boundaryFault("wx_alloc", err)
	}
	ptr := uint32(res[0])
	if ptr == 0 {
		return 0, jsruntime.WrapBoundary("wx_alloc", fmt.Errorf("oom"))
	}
	if !r.mem.Write(ptr, b) || !r.mem.WriteByte(ptr+uint32(len(b)), 0) {
		return 0, jsruntime.WrapBoundary("wx_alloc", fmt.Errorf("write out of range"))
	}
	return ptr, nil
}

// freePtr frees a wasm buffer. It must run independently of caller cancellation:
// under WithCloseOnContextDone, a cancelled caller ctx would make wx_free close
// the module, leaving the buffer allocated and corrupting an otherwise healthy
// warm runtime after a successful VM call.
func (r *runtime) freePtr(ptr uint32) {
	if ptr != 0 && !r.poisoned {
		_, _ = r.free.Call(context.Background(), uint64(ptr))
	}
}
