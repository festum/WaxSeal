# WaxSeal

A native-Go YouTube **PO Token (POT)** provider. WaxSeal handles networking,
descrambling, protobuf, caching, and orchestration in **pure Go**, and runs only
Google's **BotGuard VM** inside **QuickJS compiled to WASM**, executed by
[`wazero`](https://github.com/tetratelabs/wazero) (pure Go, no CGo).

The result is a single static binary: no CGo, no Node/V8, and no per-arch native
artifacts. The embedded `qjs.wasm` and `bg_bundle.js` are architecture-neutral,
so normal `GOOS`/`GOARCH` cross-builds work. WaxSeal can plug into
[WaxTap](https://github.com/colespringer/waxtap) as a `potoken.Provider`, run as
a bgutil-wire-compatible HTTP daemon, or run as a CLI.

> **Status.** The QuickJS-on-wazero core is in place. Offline runtime tests cover
> the ABI, memory return on close, bounded allocation/loop/regex behavior, and
> warm minting on the fake VM. Live BotGuard tests show that the real obfuscated
> VM runs in QuickJS-on-wazero and that Google's `GenerateIT` accepts its output.
> The production provider path and server/CLI hardening are still being finished.
> Artifact pins and hashes live in [build/PROVENANCE.md](build/PROVENANCE.md).

## Layout

```
build/wasm/host.c          WaxSeal QuickJS host ABI (the only C we ship; WASI reactor)
build/js/{shim,entrypoint}.js  browser shim (Proxy discovery trap) + BotGuard entrypoint
internal/jsassets/         go:embed qjs.wasm + bg_bundle.js (committed build outputs)
internal/jsruntime/        Runtime/Engine interface + quickjs (wazero) backend
internal/botguard/         challenge fetch/descramble/parse, GenerateIT, mint, field-6 validate
Makefile                   make deps / wasm / jsbundle / provenance
```

## Build & test

```
go build ./...      # needs no C/Node toolchain; artifacts are committed
go test ./...       # offline unit tests
make deps           # fetch pinned wasi-sdk + quickjs-ng, npm install (only to rebuild artifacts)
make wasm jsbundle  # regenerate qjs.wasm / bg_bundle.js
go test -tags e2e ./internal/botguard/ -run TestGateB -v   # live BotGuard check
```

## License

MIT. Implemented independently; the GPL-3.0 bgutil project is a behavioral/wire
reference only (no code copied). See [THIRD-PARTY-NOTICES.md](THIRD-PARTY-NOTICES.md).
