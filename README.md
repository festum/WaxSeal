# WaxSeal

WaxSeal is a YouTube **PO Token (POT)** provider that runs Google's BotGuard in
a real headless Chromium, driven over the Chrome DevTools Protocol by a
repository-local standard-library client. It provides a bgutil-compatible HTTP
daemon, a CLI, and reusable Go clients.

Using a real browser lets BotGuard inspect the actual navigator and reliably
produce tokens with the **integrity** grade.

> WaxSeal requires a system Chromium at runtime. WaxSeal auto-detects the
> executable. Set `WAXSEAL_CHROME_BIN` to override it. The Go binary
> cross-compiles normally but is not self-contained.

## Quick start

```sh
go build ./...
go test ./...   # offline unit tests; no browser or network

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

For `/get_pot`, `content_binding` is the value the token is bound to: a
**video ID** for a player token or **visitor data** for a GVS token. It is
limited to 4096 bytes. The optional `scope` may be `player`, `gvs`, `pot`, or
omitted. Scope only namespaces cache entries; `content_binding` selects the
token type. `/get_pot` sets `X-Pot-Cache: hit` when the token was served from
the cache or `X-Pot-Cache: miss` when it was freshly minted.

Tokens and exported identities are bound to the minting host's egress IP. The
consumer must use that same IP for SABR media requests.

`/session` verifies full-length streaming, then returns the tenant's anonymous
`visitor_data`, cookies, and `session_generation`. Each cookie includes
`expires` (RFC3339, omitted for session cookies) and `same_site`. Exported
sessions do not contain a Google login.

### Player context

`POST /player-context {"video_id":"<id>"}` or
`GET /player-context?video_id=<id>` returns the browser's streaming context and
`session_generation`. The response includes the signed SABR URL, ustreamer
config, visitor data, client version, player URL, and audio formats. Consumers
use the player URL to descramble the SABR URL's throttling nonce.

Select each `audio_formats` entry by its `itag`, `lmt`, and `xtags` **together**,
never by `itag` alone. The same `itag` can appear more than once. A clean track
and a DRC track both use `itag` 251 and differ only in `xtags`, so an `itag`-only
selector is ambiguous. An inconsistent tuple makes the SABR server return a
player-response reload instead of media.

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

A report's disposition is counted in `/metrics`:

- `degradation_reports_accepted`: applied to the current live session.
- `degradation_reports_rate_limited`: rejected within the `--report-debounce` window.
- `degradation_reports_rejected_stale`: named a genuinely old or replaced
  generation.
- `degradation_reports_already_retired`: named the current generation whose
  session was already retired by a crash or a prior report. This is a benign
  no-op, distinct from a stale report.

### Wire reference

The `client` package structs mirror these shapes and keep the JSON tags in sync;
the field names below are authoritative. A consumer can implement all four
contracts from this section alone. Optional fields are marked; the streaming
`n` parameter in `server_abr_streaming_url` stays scrambled and must be
descrambled with `player_url` before use.

`POST /get_pot` mints or fetches a cached token.

```jsonc
// request
{"content_binding": "<video_id | visitor_data>", "scope": "player"}  // scope optional: "player" | "gvs" | "pot" | omitted
// response
{
  "poToken": "MnRV...",
  "contentBinding": "<echoed content_binding>",
  "expiresAt": "2026-07-01T18:00:00Z",   // RFC3339; now+6h when the grant lifetime is unknown
  "warning": "content_binding looks like a URL; ..."  // optional; present only when the binding looks like a URL
}
```

`POST /player-context {"video_id":"<id>"}` (or `GET /player-context?video_id=<id>`).

```jsonc
// response
{
  "playability_status": "OK",
  "player_url": "https://www.youtube.com/s/player/<hash>/player_ias.vflset/en_US/base.js",
  "server_abr_streaming_url": "https://...&n=<scrambled>",
  "video_playback_ustreamer_config": "<base64>",
  "visitor_data": "<base64>",
  "client_version": "2.YYYYMMDD.NN.NN",
  "title": "<video title>",
  "author": "<channel name>",
  "length_seconds": 634,
  "audio_formats": [
    {
      "itag": 251,
      "lmt": "1699999999999999",
      "xtags": "",                          // clean track
      "mime_type": "audio/webm; codecs=\"opus\"",
      "bitrate": 130000,
      "content_length": 10318791,
      "approx_duration_ms": 634601,
      "audio_sample_rate": 48000,
      "audio_channels": 2,
      "audio_quality": "AUDIO_QUALITY_MEDIUM",
      "is_drc": false,
      "audio_track_id": ""     // empty for the default or only track
    },
    {
      "itag": 251,                          // Same itag as above. Select by the
      "lmt": "1699999999999999",            // (itag, lmt, xtags) tuple, never by
      "xtags": "CggKA2RyYxIBMQ",            // itag alone. This is the DRC variant.
      "mime_type": "audio/webm; codecs=\"opus\"",
      "bitrate": 130000,
      "content_length": 10321456,
      "approx_duration_ms": 634601,
      "audio_sample_rate": 48000,
      "audio_channels": 2,
      "audio_quality": "AUDIO_QUALITY_MEDIUM",
      "is_drc": true,
      "audio_track_id": ""
    }
  ],
  "session_generation": 1
}
```

`GET /session` exports the guest identity for the session-adoption path
(`--session-url` + `--potoken-url`). No request body.

```jsonc
// response
{
  "visitor_data": "<base64>",
  "user_agent": "Mozilla/5.0 ...",
  "client_version": "2.YYYYMMDD.NN.NN",
  "cookies": [
    {
      "name": "VISITOR_INFO1_LIVE",
      "value": "...",
      "domain": ".youtube.com",
      "path": "/",
      "secure": true,
      "http_only": true,
      "same_site": "None",          // optional: "Strict" | "Lax" | "None"; omitted when unset
      "expires": "2035-01-02T03:04:05Z"  // optional RFC3339; omitted for session cookies
    }
  ],
  "cookie_header": "VISITOR_INFO1_LIVE=...; YSC=...",
  "session_generation": 1
}
```

`POST /report` reports a degraded stream.

```jsonc
// request
{"session_generation": 1, "video_id": "<id>", "reason": "truncated"}  // video_id, reason optional
// response
{
  "accepted": false,
  "retired": false,
  "retirement_pending": false,
  "generation": 1,
  "retry_after_seconds": 300   // optional; present only when rate-limited
}
```

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

Errors from recognized endpoints and unknown paths use the JSON envelope
`{"error":"human-readable message","code":"machine-readable-code"}`. Unknown
paths return `not-found`. For `video-unavailable`, the optional `details` field
contains the playability status.

Two path shapes are handled by the stock `http.ServeMux` before any WaxSeal
handler runs (the same cleaning that keeps path traversal safe), and they do
**not** share the JSON envelope:

- **Non-canonical paths** with `.`, `..`, or repeated slashes (for example
  `//get_pot`) get a **307** redirect to their cleaned form (with a `Location`
  header). The redirect body is never the JSON envelope. Per Go's `http.Redirect`
  it is a short `text/html` snippet for **GET/HEAD** and **empty** for other
  methods, so `GET //ping` returns HTML while `POST //get_pot` returns an empty
  body. A client that does not follow redirects must not expect JSON here.
- A **trailing slash** is a *distinct* path, not a non-canonical one. `/get_pot/`
  does not match `/get_pot`, so it returns the structured **404 JSON**
  (`not-found`). Contrast `//get_pot`, which is non-canonical and redirects (307)
  instead.

`/ping` is a further exception to the envelope, for a different reason. See its
note further down in this section.

| Code | HTTP | Meaning |
|---|---:|---|
| `invalid-request` | 400 | Malformed or invalid input |
| `unauthorized` | 401 | Missing or invalid API key |
| `not-found` | 404 | Unknown path or endpoint |
| `method-not-allowed` | 405 | Unsupported HTTP method |
| `video-unavailable` | 422 | Terminal playability status |
| `mint-failed`, `player-context-failed` | 502 | Upstream operation failed |
| `no-session` | 503 | No attested session is available |
| `timeout` | 504 | Request processing deadline elapsed for `/get_pot`, `/player-context`, or `/session` |

`/report` decodes strictly. An **unknown field**, often a typo such as `raeson`
for `reason`, is rejected with **400 `invalid-request`** that names the offending
key. Its `video_id` and `reason` fields are optional, so a lenient decode would
accept a report that silently lost them, while strict decoding surfaces the typo.
This assumes the daemon and its clients are version-aligned, which holds for the
co-released WaxSeal, WaxTap, and WaxBin. **`/get_pot` and `/player-context` stay
lenient** and ignore unknown fields. `/get_pot` is the bgutil-compatible
endpoint, and a generic yt-dlp client POSTs extra fields such as `proxy`,
`bypass_cache`, and `source_address` that must be tolerated. `/player-context`
reads `video_id` from the body *or* the query string, so an unmodeled body field
must not block the query fallback (a missing `video_id` still fails clearly as
`video_id is required`). Duplicate keys stay lenient everywhere, since stock
`encoding/json` keeps the last value.

`/ping` is the exception: after authentication it returns HTTP 200 by default,
with either `ok:true` or `ok:false`. An always-present `reason` field
distinguishes the two `ok:false` cases:

- `reason:"ok"`: healthy (`ok:true`).
- `reason:"no-session"`: benign. A `POST /report` retires the session and
  re-establishment is lazy, so `ok` briefly reads `false` until the next
  streaming call. This is expected, not a fault.
- `reason:"probe-failed"`: a live session's health probe failed. The server logs
  this at `warn`.

Alert only on `reason:"probe-failed"`. A caller that disconnects mid-probe is
not reported as `probe-failed` or logged as a WARN, because the canceled request
is not a session fault. Status-code-only health checks
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
`player_contexts`, and so on) plus current state. The consumer-report
dispositions (`degradation_reports_accepted`, `degradation_reports_rate_limited`,
`degradation_reports_rejected_stale`, and `degradation_reports_already_retired`)
are defined under [Session reports](#session-reports). Detail fields are **always
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

WaxSeal launches Chromium over a CDP pipe. During normal teardown, it
terminates Chromium's process group and removes the profile. On a clean daemon
exit, closing the command pipe also lets Chromium read EOF and quit. If the
daemon dies without teardown, for example from SIGKILL or OOM, a browser process
may remain for a short time. The next startup removes abandoned WaxSeal profile
directories it can prove are not in use; it does not scan or kill processes.
Chromium generally exits after losing its profile directory. Profiles are
created under `$HOME` so snap-confined Chromium can open them, and so shared
hosts keep each daemon's profiles private.

The `crashes` metric counts unexpected browser loss detected by Chromium events
or a failed health probe. Session retirement caused by age, a consumer report,
or operation retries does not increment it.

The per-tenant `--report-debounce` (default `5m`) throttles **all**
report-driven recycles for a tenant across generations, not just repeated reports
of one generation. This is intentional anti-relaunch-storm behavior. A burst of
genuine degradations, including of a freshly-minted *replacement* generation, can
be rate-limited and return `retry_after_seconds`. Operators whose workloads see
legitimately bursty degradations may lower `--report-debounce`.

Use `go run ./cmd/waxseal server --help` for configuration options, including
session recycling, report debounce, bind address, headful mode, and metrics
access (`--metrics-key`, `--metrics-public`).

WaxSeal is intended for loopback or a trusted network and intentionally does not
implement CORS. Because it mints tokens, browser-origin access is out of scope.
An `OPTIONS` request to a recognized endpoint returns a structured 405; an
unknown path returns the structured 404 (see [Errors](#errors)).

## Development

```sh
go test ./...                       # offline unit tests; no browser or network
go test -tags live ./internal/cdp   # real-Chromium CDP pipe-transport tests
(cd provider && go test -tags e2e ./...)   # provider network e2e; needs WAXSEAL_URL/WAXSEAL_KEY
make deps                           # install browser-bundle build dependencies
make jsbundle-browser               # regenerate internal/browser/bg_browser_bundle.js
```

`go test ./...` is fully offline and deterministic. It spawns no browser and
makes no network calls. The real-Chromium CDP tests are gated behind the `live`
build tag and self-skip when no browser is found (set `WAXSEAL_CHROME_BIN` to
override the search). CI installs a browser and runs them. The `e2e` tests live
in the nested `provider/` module, so they must be run from that directory. A
root-level `go test -tags e2e ./...` does not descend into a nested module and
silently runs nothing. They need a warm daemon (`WAXSEAL_URL`, optional
`WAXSEAL_KEY`).

The committed browser bundle means normal Go builds do not require Node. The
`client` package is a reusable, WaxTap-free HTTP client. The separate
`provider/` module adapts it to WaxTap's `potoken.Provider` interface.

CLI exit codes are `0` for success, `1` for runtime failure, `2` for usage
errors, `3` for unavailable videos, and `130` for interruption.

### Manual & soak testing

Some coverage is deliberately kept out of `go test ./...` because it needs a
browser, the network, a display, or a long run. Exercise it on demand:

- **Real-Chromium CDP**: `go test -tags live ./internal/cdp` drives a local
  Chromium over the pipe transport and needs no network. Set `WAXSEAL_CHROME_BIN`
  to pick the browser, or `WAXSEAL_REQUIRE_CHROME=1` to fail instead of skip when
  none is found (CI sets this so it cannot silently lose coverage).
- **Provider e2e**: run from the nested module with `cd provider && go test -tags
  e2e ./...` against a warm daemon (`WAXSEAL_URL`, optional `WAXSEAL_KEY`),
  including the flagship full-length WEB SABR download (`cd provider && go test
  -tags e2e -run PlayerContextOnlyFullLength ./...`).
- **Time-based recycling soak**: run the daemon with a short `--streaming-max-age`
  and stream continuously to watch armed-deadline recycles and
  `streaming_seconds_until_recycle` over a long window.
- **Cache-exhaustion loop**: POST `/get_pot` 1000+ times with distinct
  `content_binding` values to exercise the mint cache and negative-cache eviction
  at capacity.
- **Headful mode**: `go run ./cmd/waxseal server --headful` to watch the real
  browser drive a session (needs a display).

## License

MIT. Implemented independently. The GPL-3.0 bgutil project is a behavioral and
wire reference only. See [THIRD-PARTY-NOTICES.md](THIRD-PARTY-NOTICES.md).
