// Package jsruntime defines the boundary between WaxSeal's Go orchestration and
// the BotGuard VM. The quickjs subpackage is the sole implementation today:
// QuickJS-ng compiled to WASM and executed by wazero (pure Go, no CGo).
package jsruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Runtime is a single JS execution context: one QuickJS runtime living in one
// wasm linear memory. It is not concurrency-safe: the QuickJS C API is
// single-threaded, so a Runtime is owned by exactly one goroutine at a time
// (the session pool enforces this via a buffered channel).
type Runtime interface {
	// Eval evaluates src as a global script and returns the JSON encoding of
	// the result. A Promise result is driven to completion (microtasks first,
	// virtual timers only when otherwise idle).
	Eval(ctx context.Context, src string) (json.RawMessage, error)

	// Call invokes globalThis[name](...args) and returns the JSON-encoded
	// settled result. args are JSON-marshaled to the VM.
	Call(ctx context.Context, name string, args ...any) (json.RawMessage, error)

	// SetWatchdog arms the in-engine interrupt deadline applied to subsequent
	// Eval/Call invocations (defense-in-depth beneath the wazero context
	// watchdog). Zero leaves only the outer wazero guard.
	SetWatchdog(d time.Duration)

	// Poisoned reports that the runtime faulted at the wasm boundary (outer
	// watchdog trip or a wasm trap) and must be evicted, never reused. A plain
	// JS exception does not poison the runtime.
	Poisoned() bool

	// Close frees the QuickJS runtime and releases the wasm instance memory.
	Close(ctx context.Context) error
}

// Engine compiles the wasm module once and instantiates many isolated Runtimes
// from it (each with its own linear memory). It is safe for concurrent use.
type Engine interface {
	// NewRuntime instantiates a fresh, isolated Runtime with the bg_bundle
	// preloaded.
	NewRuntime(ctx context.Context) (Runtime, error)
	// Close releases the shared compiled module and wazero runtime.
	Close(ctx context.Context) error
}

// JSError is a JavaScript exception (or rejected Promise) surfaced from the VM.
// The raw message/stack are kept for stage-tagged drift telemetry; the Go layer
// redacts before logging anything live-from-Google.
type JSError struct {
	Message string // "Name: message\n<stack>" as produced by the VM
}

func (e *JSError) Error() string { return "js exception: " + e.Message }

// ErrPoisoned is returned by operations on a runtime that has already faulted.
var ErrPoisoned = errors.New("jsruntime: runtime poisoned (must be evicted)")

// AsJSError extracts a *JSError from err, if present.
func AsJSError(err error) (*JSError, bool) {
	var je *JSError
	if errors.As(err, &je) {
		return je, true
	}
	return nil, false
}

// WrapBoundary annotates a wasm-boundary fault for telemetry and eviction
// decisions.
func WrapBoundary(stage string, err error) error {
	return fmt.Errorf("jsruntime boundary fault at %s: %w", stage, err)
}
