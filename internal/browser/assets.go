package browser

import _ "embed"

// browserBundle is the bgutils + BotGuard entrypoint IIFE eval'd into the real
// Chromium page. It leaves the genuine navigator/window untouched (no
// fake-browser shim), so the BotGuard VM fingerprints the real browser.
// Rebuild: make jsbundle-browser.
//
//go:embed bg_browser_bundle.js
var browserBundle string
