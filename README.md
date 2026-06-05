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

> **Status.** The QuickJS-on-wazero core is in place, and the live integrity path
> mints bound tokens that pass field-6 validation. The public Client, WaxTap
> adapter, InnerTube challenge/visitor_data source, per-egress transports,
> bgutil-compatible HTTP daemon, and CLI commands (`generate`, `server`,
> `doctor`) are implemented. Offline tests cover the runtime ABI, the
> challenge/mint/validate pipeline, the warm-minter session, server handlers, and
> config precedence; live BotGuard tests run under `-tags e2e`. Artifact pins and
> hashes live in [build/PROVENANCE.md](build/PROVENANCE.md).

## Layout

```
waxseal.go / profile.go / egress.go  public Client: orchestration, profiles, per-egress transports
provider/                  WaxTap potoken.Provider adapter (separate, WaxTap-only module)
server/                    bgutil-wire-compatible net/http daemon
cmd/waxseal/               CLI: generate (default) / server / doctor
config/                    layered config (flags > env > file > defaults)
build/wasm/host.c          WaxSeal QuickJS host ABI (the only C we ship; WASI reactor)
build/js/{shim,entrypoint,dom}.js  browser shim + DOM model + BotGuard entrypoint
internal/jsassets/         go:embed qjs.wasm + bg_bundle.js (committed build outputs)
internal/jsruntime/        Runtime/Engine interface + quickjs (wazero) backend
internal/botguard/         challenge fetch/descramble/parse, GenerateIT, mint, field-6 validation
internal/innertube/        InnerTube att/get challenge source + guest visitor_data
internal/session/          warm-minter pool (refresh-ahead, snapshot semaphore, breaker)
internal/cache/ httpx/     token cache; shared Google-facing HTTP (retry/backoff/breaker)
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

## Run

```
# CLI generate mode, compatible with bgutil's script provider.
go run ./cmd/waxseal -c <content_binding>

# HTTP daemon (defaults to loopback 127.0.0.1:4416).
go run ./cmd/waxseal server
curl -s localhost:4416/get_pot -d '{"content_binding":"<videoID>"}'   # -> {"poToken",...}
curl -s localhost:4416/ping                                          # health check; never mints

# Redacted diagnostics by stage.
go run ./cmd/waxseal doctor
```

The daemon treats `content_binding` as an opaque mint identifier. It rejects the
deprecated `visitor_data`/`data_sync_id` fields with HTTP 400, and ignores
per-request `proxy`/`source_address`/`disable_tls_verification` unless started
with `--allow-request-egress-override`. Set `POT_SERVER_SECRET` to require an
`X-WaxSeal-Secret` header on every endpoint except `/ping`.

Config precedence is flags > env (`POT_SERVER_HOST`/`POT_SERVER_PORT`,
`CACHE_DIR`, `CACHE_MAX_TTL`, `HTTP(S)_PROXY`, `DISABLE_INNERTUBE`,
`LOG_LEVEL`/`LOG_FORMAT`) > file > defaults.

## License

MIT. Implemented independently; the GPL-3.0 bgutil project is a behavioral/wire
reference only (no code copied). See [THIRD-PARTY-NOTICES.md](THIRD-PARTY-NOTICES.md).
