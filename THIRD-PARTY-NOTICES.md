# Third-party notices

WaxSeal is **MIT**-licensed. It is implemented independently. The **GPL-3.0**
`bgutil-ytdlp-pot-provider` project was used only as a behavioral and wire
reference for interoperable details such as endpoints and JSON field names; no
GPL code was copied. Algorithms ported from MIT sources are attributed below.

## Bundled / embedded at runtime

- **quickjs-ng** (MIT): © 2017-present Fabrice Bellard, Charlie Gordon, and
  quickjs-ng contributors. Compiled to `internal/jsassets/qjs.wasm`.
  <https://github.com/quickjs-ng/quickjs>
- **bgutils-js** (MIT): bundled into `internal/jsassets/bg_bundle.js`.
  <https://github.com/LuanRT/BgUtils>

## Build-time only (not shipped)

- **wasi-sdk** (Apache-2.0 with LLVM exceptions): compiles `host.c` + quickjs-ng.
- **esbuild** (MIT): bundles `bg_bundle.js`.
- **Binaryen / `wasm-opt`** (Apache-2.0, `version_119`): required for the
  pinned artifact size and AOT profile (`-Os`).

## Go module dependencies

- **github.com/tetratelabs/wazero** (Apache-2.0): pure-Go WebAssembly runtime.
- **go.etcd.io/bbolt** (MIT): pure-Go embedded key/value store backing the
  default disk-persistent token cache and breaker cooldown (its own file
  locking). <https://github.com/etcd-io/bbolt>
- **github.com/spf13/cobra** / **spf13/pflag** (Apache-2.0): CLI framework.

## Ported algorithms (MIT, with attribution)

- **rustypipe-botguard** (MIT): `descramble` (`+97`/byte), `parse_challenge_data`,
  `validate_potoken` (protobuf field-6 scan), and the `bg_entrypoint.js` shape
  were ported to Go / the WaxSeal entrypoint.
  <https://codeberg.org/ThetaDev/rustypipe-botguard>
- **BgUtils** (MIT): the BotGuard client / WebPoMinter protocol (`botGuardClient.ts`,
  `webPoMinter.ts`, `webPoClient.ts`, `helpers.ts`) informed the entrypoint and
  validator. <https://github.com/LuanRT/BgUtils>
