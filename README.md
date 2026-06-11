# WaxSeal

A YouTube **PO Token (POT)** provider that mints tokens from a **real headless
Chromium**: Google's **BotGuard** runs in the actual browser (via
[go-rod](https://github.com/go-rod/rod)), while WaxSeal handles attestation,
token binding, caching, multi-tenancy, and the HTTP/CLI surface in Go.

Running BotGuard in a genuine browser rather than an emulated JS environment lets
the attestation fingerprint a real navigator, which reliably earns the
**integrity** token grade. WaxSeal runs as a bgutil-wire HTTP daemon or a CLI, and
its HTTP API is consumed by applications that embed the
[WaxTap](https://github.com/colespringer/waxtap) library.

> **Requires a system Chromium at runtime** (auto-detected, or set
> `WAXSEAL_CHROME_BIN`). The Go binary cross-compiles normally, but it drives an
> external browser, so it is not a self-contained static binary.

## Layout

```
client/              Go client for the WaxSeal HTTP API (POToken + Session); WaxTap-free, reusable by any app
cmd/waxseal/         CLI: generate (default) / server / doctor / ping
server/              bgutil-wire HTTP daemon over the minter (get_pot/session/ping/metrics)
internal/browser/    go-rod + Chromium substrate: Session, Pool (incognito contexts), bundle
internal/minter/     reliability + multi-tenancy: Minter (single-flight/cache/escalation), Tenants
internal/botguard/   challenge fetch/descramble/parse, GenerateIT, field-6 validation
internal/innertube/  InnerTube att/get challenge source + guest visitor_data
internal/httpx/      shared Google-facing HTTP (retry/backoff)
build/js/            bgutils + BotGuard entrypoint -> internal/browser bundle (esbuild)
provider/            thin WaxTap potoken.Provider adapter over client/ (separate, WaxTap-only module)
```

Any Go application can talk to a WaxSeal daemon via the WaxTap-free `client`
package: `client.New(url).POToken(ctx, contentBinding, scope)` and `.Session(ctx)`.
The `provider/` module is a thin scope-mapping adapter that satisfies WaxTap's
`potoken.Provider`; a non-WaxTap consumer uses `client` directly or writes its own
adapter.

## Build & test

```
go build ./...      # needs no Node toolchain; the browser bundle is committed
go test ./...       # offline unit tests (race-clean)
make deps           # npm install (only to rebuild the browser bundle)
make jsbundle-browser   # regenerate internal/browser/bg_browser_bundle.js
```

## Run

```
# HTTP daemon (defaults to loopback 127.0.0.1:4416). Warms a browser at startup.
go run ./cmd/waxseal server
curl -s localhost:4416/get_pot -d '{"content_binding":"<videoID>"}'   # -> {"poToken",...}
curl -s localhost:4416/player-context -d '{"video_id":"<videoID>"}'   # -> status-1 streaming context
curl -s localhost:4416/session                                        # visitor_data + cookies (coherence handoff)
curl -s localhost:4416/ping                                           # health check; never mints
curl -s localhost:4416/metrics                                        # per-tenant counters

# One-shot CLI generate (bgutil script-provider compatible). Launches a fresh
# browser each call, so for yt-dlp prefer the warm `server`.
go run ./cmd/waxseal -c <content_binding>

# Diagnostics: launch a browser, attest, report identity + token grade.
go run ./cmd/waxseal doctor

# Health-check a running daemon (exit 0/1) for scripts/systemd/monitoring.
go run ./cmd/waxseal ping
```

`content_binding` is the mint identifier: a **video_id** for a player token, or a
**visitor_data** for a GVS token. The token is bound to the minting host's egress
IP, so the consumer must egress the **same IP** for the SABR media stage.

## Multi-tenant

One Chromium hosts N isolated incognito **browser contexts**, one guest identity
(visitor_data + cookies) per tenant, selected by per-tenant API keys:

```
go run ./cmd/waxseal server --tenant-keys "alice=KEYA,bob=KEYB"
curl -s localhost:4416/get_pot -H "X-API-Key: KEYA" -d '{"content_binding":"<id>"}'
```

With no `--tenant-keys` the daemon is **keyless single-tenant** (the bgutil wire
stays unauthenticated for generic yt-dlp). The key may be sent as `X-API-Key`,
`Authorization: Bearer <key>`, or `?key=<key>`. Per-tenant egress is a future
seam; residential self-hosting uses the one host IP.

## Player context (`/player-context`)

`POST /player-context {"video_id":"<id>"}` (or `?video_id=<id>`) returns the
attested browser's **status-1** streaming context for a video: the
`server_abr_streaming_url` (graded `STREAM_PROTECTION` status 1 by the genuine
browser's own `/player` call, carrying a **scrambled** throttling nonce the
consumer descrambles with `player_url`; `client_version` does not pin base.js),
the ustreamer config, the visitor_data a GVS token binds to, the client version,
and the audio formats (each with the itag+lmt+xtags triple needed to select a
coherent format). This is the endpoint that delivers full WEB SABR audio: WaxSeal
mints the context, the consumer runs the stream (it does no SABR/streaming itself).

## Coherence handoff (`/session`)

`GET /session` exports the tenant context's coherent {visitor_data, cookies}
identity so a consumer embedding WaxTap can adopt the browser-as-origin; pair it
with a `/get_pot` token bound to that same visitor_data, egressing the same IP.
This coherence is **necessary but not sufficient** for full streams: a fully
coherent GVS session (matching token + session + IP) still streams under
`STREAM_PROTECTION` status 2 (the ~70s preview), so use `/player-context` for the
status-1 context. The session is anonymous (no Google login).

## License

MIT. Implemented independently; the GPL-3.0 bgutil project is a behavioral/wire
reference only (no code copied). See [THIRD-PARTY-NOTICES.md](THIRD-PARTY-NOTICES.md).
