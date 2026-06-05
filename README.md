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
> `doctor`, `ping`) are implemented. The current tree also includes optional
> disk-backed token caching, persisted breaker cooldowns, Prometheus metrics, and
> a cross-platform release matrix. Offline tests cover the runtime ABI, the
> challenge/mint/validate pipeline, the warm-minter session, persistence, metrics,
> server handlers, and config precedence; live BotGuard tests run under
> `-tags e2e`. Artifact pins and hashes live in
> [build/PROVENANCE.md](build/PROVENANCE.md).

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
internal/persist/          disk store: bbolt (default) / JSON, versioned; memory fallback
internal/metrics/          dependency-free counters + Prometheus exposition
Makefile                   make deps / wasm / jsbundle / provenance / verify-assets / release
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
curl -s localhost:4416/metrics                                       # Prometheus counters

# Redacted diagnostics by stage.
go run ./cmd/waxseal doctor

# Health-check a running daemon (exit 0/1) for scripts/systemd/monitoring.
go run ./cmd/waxseal ping
```

The daemon treats `content_binding` as an opaque mint identifier. It rejects the
deprecated `visitor_data`/`data_sync_id` fields with HTTP 400, and ignores
per-request `proxy`/`source_address`/`disable_tls_verification` unless started
with `--allow-request-egress-override`. Set `POT_SERVER_SECRET` to require an
`X-WaxSeal-Secret` header on every endpoint except `/ping`.

Config precedence is flags > env (`POT_SERVER_HOST`/`POT_SERVER_PORT`,
`CACHE_DIR`, `CACHE_MAX_TTL`, `PERSIST_TOKENS`, `DISK_CACHE_BACKEND`,
`ENDPOINT_MODE`, `HTTP(S)_PROXY`, `DISABLE_INNERTUBE`, `POT_SERVER_SECRET`,
`LOG_LEVEL`/`LOG_FORMAT`) > file > defaults. `ENDPOINT_MODE` selects the WAA
attestation host: `youtube` (default, `youtube.com/api/jnn/v1`) or `googleapis`
(`jnn-pa.googleapis.com`).

## Persistence

Warm minters run `Create`/`GenerateIT` roughly once per egress per token
lifetime, usually 6 to 12 hours. A configured disk store keeps usable state
across restarts, which reduces repeated `Create` calls after daemon restarts.
When `CACHE_DIR` is set:

- the **circuit-breaker cooldown is persisted by default** (non-sensitive): an
  active cooldown survives a restart and prevents immediate attestation calls;
- the **token cache is opt-in** (`--persist-tokens` / `PERSIST_TOKENS=true`)
  because tokens are bearer capabilities; store files are `0600`.

The default backend is **bbolt**, which provides its own file locking.
`--disk-backend json` selects a JSON file for single-process use. The store
fingerprint includes the `qjs.wasm` and `bg_bundle.js` hashes; when it changes,
WaxSeal resets the stored state. If a configured store is locked or too slow to
open (for example, on a network mount), WaxSeal logs the failure and uses
memory-only storage. The in-process library also defaults to memory-only when
`CacheDir` is empty.

## Metrics

The daemon exposes Prometheus metrics at `GET /metrics` (behind the shared
secret, like every endpoint but `/ping`):

```
curl -s localhost:4416/metrics
# waxseal_snapshots_total, waxseal_attestations_total,
# waxseal_mints_total{kind="integrity|fallback"}, waxseal_runtime_poisons_total,
# waxseal_breaker_opens_total, waxseal_cache_hits_total/_misses_total,
# waxseal_attest_failures_total{stage="challenge|vm|generateit|..."}, ...
```

The counters help distinguish API drift, shown as a spike in
`attest_failures_total` by stage, from IP risk scoring, where integrity mints
downgrade to fallback tokens.

## Cross-platform

The pure-Go, no-CGo design builds as a static binary with the wasm/JS embedded;
no Node, V8, or Python runtime is required. The embedded `qjs.wasm` and
`bg_bundle.js` assets are **architecture-neutral**, so `make release` uses normal
`CGO_ENABLED=0` cross-builds for:

| OS \ Arch | amd64 | arm64 |
|-----------|:-----:|:-----:|
| Linux     |  yes  |  yes  |
| macOS     |  yes  |  yes  |
| Windows   |  yes  |  yes  |

```
make release            # cross-compiles all six into dist/
make verify-assets      # rebuild qjs.wasm/bg_bundle.js and diff the committed bytes
```

CI ([.github/workflows/ci.yml](.github/workflows/ci.yml)) runs race tests, the
cross-compile matrix, and the artifact-reproducibility diff on every push.

## License

MIT. Implemented independently; the GPL-3.0 bgutil project is a behavioral/wire
reference only (no code copied). See [THIRD-PARTY-NOTICES.md](THIRD-PARTY-NOTICES.md).
