// Bundle bgutils-js + browser_entrypoint.js into a single ES2020 IIFE that go-rod
// injects into a real Chromium page. The output is committed under internal/browser/
// and go:embed-ed there, so `go build` needs no Node toolchain.
// Rebuild: make jsbundle-browser.
import { build } from 'esbuild';
import { createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';

const pkgVersion = (name) =>
  JSON.parse(readFileSync(`node_modules/${name}/package.json`, 'utf8')).version;
const bgutilsVersion = pkgVersion('bgutils-js');
const esbuildVersion = pkgVersion('esbuild');

const OUT = '../../internal/browser/bg_browser_bundle.js';

const result = await build({
  entryPoints: ['browser_entrypoint.js'],
  bundle: true,
  format: 'iife',
  target: 'es2020',
  platform: 'browser',
  legalComments: 'none',
  minify: false, // keep the embedded bundle readable
  banner: {
    js: `// GENERATED - do not edit. Source: build/js/browser_entrypoint.js + bgutils-js@${bgutilsVersion}.\n`
      + `// Rebuild: make jsbundle-browser (esbuild@${esbuildVersion}).`
  },
  outfile: OUT
});

if (result.errors.length) {
  console.error(result.errors);
  process.exit(1);
}

const bytes = readFileSync(OUT);
const sha = createHash('sha256').update(bytes).digest('hex');
console.log(`bg_browser_bundle.js: ${bytes.length} bytes  sha256=${sha}`);
console.log(`  bgutils-js@${bgutilsVersion}  esbuild@${esbuildVersion}`);
