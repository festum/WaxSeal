// Package jsassets embeds the two build outputs the QuickJS-on-wazero runtime
// needs: qjs.wasm (the WaxSeal host ABI + quickjs-ng, a WASI reactor) and
// bg_bundle.js (bgutils-js + shim + entrypoint). They are committed and treated
// like vendored dependencies, so plain `go build` and `go test` need no C/Node toolchain;
// `make wasm`/`make jsbundle` regenerate them.
//
// Both are arch-neutral: the identical bytes load on every GOOS/GOARCH.
package jsassets

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
)

//go:embed qjs.wasm
var QJSWasm []byte

//go:embed bg_bundle.js
var BGBundle []byte

// Hashes are mixed into persisted-store schema versions (token cache, breaker,
// wazero compilation cache) so artifact changes invalidate stale state cleanly.
func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// QJSWasmSHA256 is the hex sha256 of the embedded qjs.wasm.
func QJSWasmSHA256() string { return sum(QJSWasm) }

// BGBundleSHA256 is the hex sha256 of the embedded bg_bundle.js.
func BGBundleSHA256() string { return sum(BGBundle) }
