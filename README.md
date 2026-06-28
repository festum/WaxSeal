# WaxSeal

WaxSeal is a YouTube **PO Token (POT)** provider that runs Google's BotGuard in
a real headless Chromium through [go-rod](https://github.com/go-rod/rod). It
provides a bgutil-compatible HTTP daemon, a CLI, and reusable Go clients.

Using a real browser lets BotGuard inspect the actual navigator and reliably
produce tokens with the **integrity** grade.

> WaxSeal requires a system Chromium at runtime. WaxSeal auto-detects the
> executable. Set `WAXSEAL_CHROME_BIN` to override it. The Go binary
> cross-compiles normally but is not self-contained.

## Quick start

```sh
go build ./...
go test ./...

# Start the daemon on 127.0.0.1:4416.
go run ./cmd/waxseal server

# Mint a token, export an attested identity, or get a streaming context.
curl -s localhost:4416/get_pot -d '{"content_binding":"<video_id>"}'
curl -s localhost:4416/session
curl -s localhost:4416/player-context -d '{"video_id":"<video_id>"}'

# Health and metrics.
curl -s localhost:4416/ping
curl -s localhost:4416/metrics
```

The daemon binds its socket before browser startup, but it is ready only when
`/ping` returns `{"ok":true,...}`. Startup attests the first tenant, caches a
GVS token, and attempts a full-length streaming proof, which usually takes
10-30 seconds. A mint failure stops startup. A failed streaming proof is logged
and retried by `/player-context` or `/session`.

Other CLI commands:

```sh
go run ./cmd/waxseal -c <content_binding>       # one-shot token generation
go run ./cmd/waxseal player-context <video_id> # one-shot streaming context
go run ./cmd/waxseal doctor                    # report identity and token grade
go run ./cmd/waxseal ping                      # check a running daemon
```

One-shot generation launches a fresh browser each time. Prefer the warm daemon
for repeated requests. Commands that accept `--video` require a bare video ID,
not a URL.

## HTTP API

| Method | Endpoint | Purpose |
|---|---|---|
| `POST` | `/get_pot` | Mint or retrieve a cached PO token |
| `GET`, `POST` | `/player-context` | Return an attested streaming context |
| `GET` | `/session` | Export the attested guest identity and cookies |
| `POST` | `/report` | Report a degraded stream and recycle its session |
| `GET` | `/ping` | Check the current browser session without minting |
| `GET` | `/metrics` | Return operational counters; keyed daemons redact tenant detail unless `--metrics-key` or `--metrics-public` is set |

### Tokens and identity

For `/get_pot`, `content_binding` is a **video ID** for a player token or
**visitor data** for a GVS token. It is limited to 4096 bytes. The optional
`scope` may be `player` or `gvs`. It only separates cache entries.

Tokens and exported identities are bound to the minting host's egress IP. The
consumer must use that same IP for SABR media requests.

`/session` proves full-length streaming, then returns the tenant's anonymous
`visitor_data`, cookies, and `session_generation`. Exported sessions do not
contain a Google login.

### Player context

`POST /player-context {"video_id":"<id>"}` or
`GET /player-context?video_id=<id>` returns the browser's streaming context and
`session_generation`. The response includes the signed SABR URL, ustreamer
config, visitor data, client version, player URL, and audio formats. Consumers
use the player URL to descramble the SABR URL's throttling nonce.

`playability_status` is YouTube's string status, such as `"OK"`. It is not the
SABR status-1 protection code, which is embedded in the signed SABR URL.

### Session reports

When a stream is degraded, report the generation returned by `/session` or
`/player-context`:

```sh
curl -s localhost:4416/report -d '{
  "session_generation": 1,
  "video_id": "<video_id>",
  "reason": "truncated"
}'
```

`session_generation` is required. Optional `video_id` and `reason` values must
be 1-64 characters from `[A-Za-z0-9_-]`. The response includes the current
`generation` and the boolean fields `accepted`, `retired`, and
`retirement_pending`.

Reports are scoped and rate-limited per tenant. After a report-driven recycle,
another report within `--report-debounce` (default `5m`) is rejected and returns
`retry_after_seconds`. Stale or future generations are ignored.

### Authentication and tenants

By default the daemon is keyless and single-tenant. Configure isolated browser
contexts with API keys:

```sh
go run ./cmd/waxseal server --tenant-keys "alice=KEYA,bob=KEYB"
curl -s localhost:4416/get_pot \
  -H "X-API-Key: KEYA" \
  -d '{"content_binding":"<id>"}'
```

Keys may be sent with `X-API-Key`, `Authorization: Bearer <key>`, or
`?key=<key>`. A keyless daemon bound to a non-loopback host exposes its guest
identity through `/session` and `/player-context`. Use `--tenant-keys` when
exposing the service.

`--tenant-keys` accepts comma-separated `label=key` entries or bare keys, which
receive generated labels. Labels and keys must be non-empty and unique. Invalid
configurations stop startup before Chromium launches.

#### Metrics access

`/metrics` exposes tenant labels and per-tenant activity. On a keyed daemon it is
**redacted by default**: unauthenticated scrapes, tenant-key scrapes, and
requests with the wrong key receive a label-free aggregate. Full tenant detail
requires the operator metrics key or an explicit public configuration. A keyless
daemon still serves full detail. All variants return HTTP 200; redaction is a
successful response, not a `401`.

| Daemon / request | `/metrics` returns |
|---|---|
| keyless (default) | **full** per-tenant detail (unchanged) |
| keyed, no key / tenant key / wrong key | **redacted aggregate**: daemon-wide summed counters, no labels, no tenant count |
| keyed, correct `--metrics-key` | **full** per-tenant detail |
| keyed, `--metrics-public` | **full** per-tenant detail, unauthenticated |

Tenant keys never unlock detail. Only the dedicated `--metrics-key` (operator
key) or `--metrics-public` does. This keeps minting keys separate from metrics
access and prevents one tenant from reading another tenant's counters.
`--metrics-key` must differ from every tenant key; a collision stops startup.
When both flags are set, `--metrics-public` wins. Both flags are ignored on a
keyless daemon, which already serves full detail.

The two response shapes are the full
`{"tenants":N,"per_tenant":{"<label>":{...}}}` and the redacted
`{"redacted":true,"aggregate":{...}}`; see [The `/metrics` schema](#the-metrics-schema).

### Errors

Recognized endpoints return errors as
`{"error":"human-readable message","code":"machine-readable-code"}`. For
`video-unavailable`, the optional `details` field contains the playability
status.

| Code | HTTP | Meaning |
|---|---:|---|
| `invalid-request` | 400 | Malformed or invalid input |
| `unauthorized` | 401 | Missing or invalid API key |
| `method-not-allowed` | 405 | Unsupported HTTP method |
| `video-unavailable` | 422 | Terminal playability status |
| `mint-failed`, `player-context-failed` | 502 | Upstream operation failed |
| `no-session` | 503 | No attested session is available |
| `timeout` | 504 | Player-context deadline elapsed |

`/ping` is the exception: after authentication it returns HTTP 200 by default,
with either `ok:true` or `ok:false`. An always-present `reason` field
distinguishes the two `ok:false` cases:

- `reason:"ok"`: healthy (`ok:true`).
- `reason:"no-session"`: benign. A `POST /report` retires the session and
  re-establishment is lazy, so `ok` briefly reads `false` until the next
  streaming call. This is expected, not a fault.
- `reason:"probe-failed"`: a live session's health probe failed. The server logs
  this at `warn`.

Alert only on `reason:"probe-failed"`. Status-code-only health checks
(k8s liveness/readiness, `curl -f`, HAProxy) can pass
`?strict=true` to map a `probe-failed` to **HTTP 503** while `no-session` and
healthy stay **200**. This avoids liveness failures during the benign
re-establishment window. In the `ok:false` branch the other fields, such as
`navigator_webdriver` and `attest`, reflect a zero or last-known state rather
than a fresh reading; a human-readable `error` is also included.

The `waxseal ping` CLI mirrors this: by default it exits non-zero unless a live
session is present (a readiness check), and `waxseal ping --strict` sends
`?strict=true` and exits non-zero only on `probe-failed`, treating the benign
`no-session` window as healthy. Use `--strict` for container or systemd liveness
probes so they do not fail while a session re-establishes.

The `client` package parses API errors into `*client.APIError` and provides
matching code constants.

### The `/metrics` schema

`/metrics` returns one of two shapes (see [Metrics access](#metrics-access) for
which, and the HTTP 200 guarantee).

**Full per-tenant view**: `{"tenants":N,"per_tenant":{"<label>":{...}}}`. Each
per-tenant object carries lifetime counters (`mints`, `crashes`,
`player_contexts`, and so on) plus current state. Detail fields are **always
present** so the schema stays stable across session retirement, crash, and
recycle. WaxSeal uses sentinel values when a field does not apply:

- `last_browser_proof_outcome`: `""` when no proof has run or no session is live.
- `last_browser_proof_age_secs`: JSON `null` when no proof has run or no session
  is live, otherwise an integer. This keeps `0` reserved for "just proved."
- `streaming_suspect_video`: `""` when the session is not suspect.
- `streaming_seconds_until_recycle`: present **only** when time-based recycling
  is enabled (`--streaming-max-age` > 0). The value is an integer when a live
  session has an armed deadline and `null` when recycling is enabled but no live
  session has a deadline. If the field is absent, recycling is disabled.

When a detail field does not apply, WaxSeal emits `null` or `""` rather than
omitting the field.

**Redacted aggregate view**: `{"redacted":true,"aggregate":{...}}`. The
`aggregate` object holds each lifetime counter summed across all tenants. It has
no tenant labels, no per-tenant breakdown, and no tenant count. Counter keys are
always present and have value zero when there are no tenants.

## Operations

One Chromium process hosts an isolated incognito context per tenant. Additional
tenants attest on their first token, player-context, or session request.

WaxSeal runs Chromium under go-rod's leakless process guard and cleans up
abandoned WaxSeal profiles that it can identify on Unix startup. On shared
hosts, set `TMPDIR` to a private directory. If `/tmp` is mounted `noexec`, set
`TMPDIR` to a writable filesystem that permits execution.

The `crashes` metric counts unexpected browser loss detected by Chromium events
or a failed health probe. Session retirement caused by age, a consumer report,
or operation retries does not increment it.

Use `go run ./cmd/waxseal server --help` for configuration options, including
session recycling, report debounce, bind address, headful mode, and metrics
access (`--metrics-key`, `--metrics-public`).

## Development

```sh
go test ./...           # offline unit tests
make deps               # install browser-bundle build dependencies
make jsbundle-browser   # regenerate internal/browser/bg_browser_bundle.js
```

The committed browser bundle means normal Go builds do not require Node. The
`client` package is a reusable, WaxTap-free HTTP client. The separate
`provider/` module adapts it to WaxTap's `potoken.Provider` interface.

CLI exit codes are `0` for success, `1` for runtime failure, `2` for usage
errors, `3` for unavailable videos, and `130` for interruption.

## License

MIT. Implemented independently. The GPL-3.0 bgutil project is a behavioral and
wire reference only. See [THIRD-PARTY-NOTICES.md](THIRD-PARTY-NOTICES.md).
