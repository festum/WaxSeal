package quickjs_test

import "testing"

// TestGlobalSurfaceHidesInternalBindings verifies three classes of internal names:
//
//	removed - absent from globalThis
//	hidden  - available to the host but absent from browser proxy enumeration
//	intact  - captured host bridges still work
//
// Object.getOwnPropertyNames(globalThis) still includes hidden names because the
// host ABI resolves them by global name.
func TestGlobalSurfaceHidesInternalBindings(t *testing.T) {
	rt := newBundledRT(t)

	// 1. Fully removed from the global object.
	for _, n := range []string{
		"__wx_console", "__wx_random_fill", "__wx_random_double",
		"__wxBundleReady", "webPoSignalOutput", "minter",
		"setImmediate", "clearImmediate", "InternalError",
	} {
		evalTrue(t, rt, "removed "+n, "typeof globalThis['"+n+"'] === 'undefined'")
	}

	// 2. Reachable by name but hidden from window proxy enumeration.
	for _, n := range []string{
		"runBotguard", "newMinter", "mint", "__wx_runTimers", "__wxApplyProfile",
		"__wxDiscovery", "__wxAutoStub", "__wxGetProbes", "__wxClearProbes",
	} {
		evalTrue(t, rt, "reachable "+n, "typeof globalThis['"+n+"'] !== 'undefined'")
		evalTrue(t, rt, "non-enumerable "+n,
			"Object.getOwnPropertyDescriptor(globalThis,'"+n+"').enumerable === false")
		evalTrue(t, rt, "not in keys "+n, "!Object.keys(globalThis).includes('"+n+"')")
		evalTrue(t, rt, "hidden has "+n, "('"+n+"' in window) === false")
		evalTrue(t, rt, "hidden get "+n, "window['"+n+"'] === undefined")
		evalTrue(t, rt, "hidden ownKeys "+n, "!Object.getOwnPropertyNames(window).includes('"+n+"')")
	}

	// 3. Browser globals stay visible through the window proxy.
	for _, n := range []string{"navigator", "setTimeout", "document", "location"} {
		evalTrue(t, rt, "visible "+n, "Object.getOwnPropertyNames(window).includes('"+n+"')")
	}

	// 4. The captured host bridges still function after being stripped from global.
	evalTrue(t, rt, "Math.random works",
		"typeof Math.random() === 'number' && Math.random() >= 0 && Math.random() < 1")
	evalTrue(t, rt, "getRandomValues works", "crypto.getRandomValues(new Uint8Array(4)).length === 4")
	evalTrue(t, rt, "console works", "(() => { console.log('surface-test ok'); return true; })()")
}
