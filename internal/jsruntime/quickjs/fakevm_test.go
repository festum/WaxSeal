package quickjs_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/colespringer/waxseal/internal/botguard"
)

// fakeVMInterpreter is a hand-written stand-in for Google's obfuscated BotGuard
// interpreter. It stores no captured Google JS. It implements just enough of the
// VM contract that the real bgutils-js (BG.BotGuardClient / BG.WebPoMinter) and
// our entrypoint drive end-to-end:
//
//	vm.a(program, vmFnCallback, ...) -> [syncSnapshotFn]   (botGuardClient.ts:49)
//	vmFnCallback(asyncSnapshot, shutdown, passEvent, checkCamera)
//	asyncSnapshot(cb, [contentBinding, signedTimestamp, webPoSignalOutput, skip])
//	    sets webPoSignalOutput[0] = getMinter; cb(botguardResponse)
//	getMinter(integrityTokenU8) -> mintCallback                (webPoMinter.ts:20)
//	mintCallback(identifierU8)  -> Uint8Array (a protobuf with field 6)
//
// The mint output encodes the identifier into protobuf field 6, so the token is
// valid per ValidatePOToken and a function of the identifier, proving
// the identifier flows all the way through the VM boundary.
const fakeVMInterpreter = `
(function () {
  globalThis.fakeVM = {
    a: function (program, vmFnCallback, flag, uie, noop, arr) {
      var asyncSnapshotFunction = function (cb, args) {
        var webPoSignalOutput = args[2];
        // getMinter closure captured into webPoSignalOutput[0].
        webPoSignalOutput[0] = function getMinter(integrityTokenU8) {
          return Promise.resolve(function mintCallback(identifierU8) {
            // protobuf: field 6 (wire type 2), length-delimited = identifier.
            var id = identifierU8;
            var out = new Uint8Array(2 + id.length);
            out[0] = 0x32;        // (6 << 3) | 2
            out[1] = id.length;   // assume < 128 for the fixture
            out.set(id, 2);
            return Promise.resolve(out);
          });
        };
        // Deliver the botguard response via a microtask (exercises the pump).
        Promise.resolve().then(function () { cb("fake-botguard-response"); });
      };
      vmFnCallback(asyncSnapshotFunction, function shutdown() {}, function passEvent() {}, function checkCamera() {});
      return [function syncSnapshot(a) { return "fake-sync-snapshot"; }];
    }
  };
})();
`

// Full mint plumbing (Go -> wx_call -> entrypoint ->
// real bgutils-js -> fake VM -> snapshot -> minter -> mint -> websafe base64 ->
// Go -> field-6 validate) with no Google JS.
func TestFakeVMFullMintFlow(t *testing.T) {
	rt := newBundledRT(t)
	ctx := context.Background()

	// runBotguard(interpreterJS, program, globalName)
	resp, err := rt.Call(ctx, "runBotguard", fakeVMInterpreter, "FAKE_PROGRAM", "fakeVM")
	if err != nil {
		t.Fatalf("runBotguard: %v", err)
	}
	var respStr string
	_ = json.Unmarshal(resp, &respStr)
	if respStr != "fake-botguard-response" {
		t.Fatalf("botguardResponse = %q, want fake-botguard-response", respStr)
	}

	// newMinter(integrityToken): base64 of "fake-integrity"; bgutils base64ToU8
	// must decode it without error.
	if _, err := rt.Call(ctx, "newMinter", "ZmFrZS1pbnRlZ3JpdHk="); err != nil {
		t.Fatalf("newMinter: %v", err)
	}

	// mint(identifier) -> websafe base64 token bound to the identifier.
	const identifier = "test-visitor-data"
	out, err := rt.Call(ctx, "mint", identifier)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	var token string
	if err := json.Unmarshal(out, &token); err != nil {
		t.Fatalf("unmarshal token: %v (%s)", err, out)
	}
	if token == "" {
		t.Fatal("empty token")
	}

	// Validate field 6, the same check used for a real token.
	field6, err := botguard.ValidatePOToken(token)
	if err != nil {
		t.Fatalf("validate minted token: %v", err)
	}
	if string(field6) != identifier {
		t.Fatalf("field6 = %q, want %q (identifier must flow through the VM)", field6, identifier)
	}

	// One warm minter mints many identifiers; mint a second, different one.
	out2, err := rt.Call(ctx, "mint", "second-id")
	if err != nil {
		t.Fatalf("second mint: %v", err)
	}
	var token2 string
	_ = json.Unmarshal(out2, &token2)
	if token2 == token {
		t.Fatal("distinct identifiers produced identical tokens")
	}
	if f6, err := botguard.ValidatePOToken(token2); err != nil || string(f6) != "second-id" {
		t.Fatalf("second token validate: f6=%q err=%v", f6, err)
	}
}
