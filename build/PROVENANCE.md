# Build provenance

The committed artifacts `internal/jsassets/qjs.wasm` and
`internal/jsassets/bg_bundle.js` are build outputs treated like vendored deps.
CI rebuilds them and diffs against these hashes for reproducibility.

## Pins

| Component | Version | Ref / notes | License |
|---|---|---|---|
| quickjs-ng | `v0.15.1` | commit `fd0a0210b7be00957751871e7e01b8291268fc29` | MIT |
| wasi-sdk | `33.0` | clang 22.1.0 (`wasm32-wasip1`) | Apache-2.0 / LLVM |
| bgutils-js | `3.2.0` | npm | MIT |
| esbuild | `0.25.12` | npm (bundler only) | MIT |
| Go | `1.26.3` | n/a | n/a |
| wazero | `v1.12.0` | pure-Go wasm runtime | Apache-2.0 |
| Binaryen `wasm-opt` | `version_119` | required canonical step: `-Os --strip-debug --strip-producers` | Apache-2.0 |

## Artifact hashes (sha256)

> Regenerate with `make provenance`. These reflect the current committed build;
> update on any artifact change.

```
qjs.wasm      2a4c75c0a3ef559055c18e210fe0466766f67fab38a6b1d378f7e04b7317691a   (901,990 B)
bg_bundle.js  1f51a29ae783d90beff9e2a08b1a745b9943b17c07d121db8633ccb02e4d0d1e   (101,989 B)
```

`qjs.wasm` is the `wasm-opt -Os` output (901,990 B, down 28.7% from the
1,265,173 B pre-opt compile). `-Os` was chosen after measuring `-Os`, `-O3`, and
`-O4` for size, wazero AOT compile time, and a CPU-bound workload. `-Os` won or
tied each case; wazero re-optimizes during AOT, so wasm-level `-O3` and `-O4`
added bytes without a runtime gain. `wasm-opt` is a required, pinned step of
`make wasm`, so the committed bytes are reproducible.

`bg_bundle.js` grew from roughly 33 KB to 101.9 KB when the DOM fidelity layer was
added (`build/js/dom.js`: prototype chains, native-looking `toString`,
canvas/WebGL/SVG/media, and the platform-interface battery).

## Rebuild

```
make deps           # fetch wasi-sdk-33 + quickjs-ng v0.15.1 into .toolchains/, npm install
make wasm           # build/wasm/host.c + quickjs-ng -> internal/jsassets/qjs.wasm (WASI reactor)
make jsbundle       # build/js/{shim,entrypoint}.js + bgutils-js -> internal/jsassets/bg_bundle.js
make provenance     # print pins + artifact hashes
```

Both artifacts are architecture-neutral: identical bytes load on every
`GOOS`/`GOARCH`, which keeps WaxSeal out of the V8/CGo build matrix.
