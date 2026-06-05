// Bundle bgutils-js + shim.js + entrypoint.js into a single ES2020 IIFE that
// the QuickJS-on-wazero runtime evaluates. Output is committed (treated like a
// vendored dep): plain `go build`/`go test` need no Node toolchain.
import { build } from 'esbuild';
import { createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';

// Read versions straight from node_modules (these packages don't export
// ./package.json via their "exports" map).
const pkgVersion = (name) =>
  JSON.parse(readFileSync(`node_modules/${name}/package.json`, 'utf8')).version;
const bgutilsVersion = pkgVersion('bgutils-js');
const esbuildVersion = pkgVersion('esbuild');

const OUT = '../../internal/jsassets/bg_bundle.js';

const result = await build({
  entryPoints: ['entrypoint.js'],
  bundle: true,
  format: 'iife',
  target: 'es2020',
  platform: 'neutral',
  legalComments: 'none',
  minify: false, // keep the committed bundle readable while the shim is evolving
  banner: {
    js: `// GENERATED - do not edit. Source: build/js/{shim,entrypoint}.js + bgutils-js@${bgutilsVersion}.\n`
      + `// Rebuild: make jsbundle (esbuild@${esbuildVersion}, target es2020 IIFE).`
  },
  outfile: OUT
});

if (result.errors.length) {
  console.error(result.errors);
  process.exit(1);
}

const bytes = readFileSync(OUT);
const sha = createHash('sha256').update(bytes).digest('hex');
console.log(`bg_bundle.js: ${bytes.length} bytes  sha256=${sha}`);
console.log(`  bgutils-js@${bgutilsVersion}  esbuild@${esbuildVersion}`);
