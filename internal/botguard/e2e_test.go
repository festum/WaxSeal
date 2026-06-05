//go:build e2e

// Live BotGuard checks against real YouTube/Google. Run:
//
//	go test -tags e2e ./internal/botguard/ -run TestGateB -v
//
// Network-dependent and intentionally excluded from the default build. These
// tests make real Create/GenerateIT calls; keep runs sparse.
package botguard_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"testing"
	"time"

	"github.com/colespringer/waxseal/internal/botguard"
	"github.com/colespringer/waxseal/internal/httpx"
	"github.com/colespringer/waxseal/internal/jsassets"
	"github.com/colespringer/waxseal/internal/jsruntime/quickjs"
)

func liveSetup(t *testing.T) (context.Context, *quickjs.Engine, *httpx.Client, *bytes.Buffer) {
	t.Helper()
	ctx := context.Background()
	stderr := &bytes.Buffer{}
	eng, err := quickjs.NewEngine(ctx, jsassets.QJSWasm, quickjs.Options{
		PreloadBundle: jsassets.BGBundle,
		Watchdog:      15 * time.Second, // the real VM snapshot is heavier than the fake
		Stderr:        stderr,
	})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close(ctx) })
	jar, _ := cookiejar.New(nil)
	return ctx, eng, httpx.New(&http.Client{Timeout: 30 * time.Second, Jar: jar}), stderr
}

// TestGateBVMExecutes verifies the core risk: the real obfuscated BotGuard
// interpreter runs inside QuickJS-on-wazero, Google's GenerateIT accepts the
// resulting response, and the returned token validates (field 6).
func TestGateBVMExecutes(t *testing.T) {
	ctx, eng, client, stderr := liveSetup(t)
	rtctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	rt, _ := eng.NewRuntime(rtctx)
	defer rt.Close(rtctx)

	ch, err := botguard.FetchCreateChallenge(rtctx, client, "")
	if err != nil {
		t.Fatalf("challenge: %v", err)
	}
	t.Logf("challenge OK: program=%d B, globalName=%q, interpreter=%d B",
		len(ch.Program), ch.GlobalName, len(ch.InterpreterJS))

	snapStart := time.Now()
	bgResp, err := botguard.Snapshot(rtctx, rt, ch, nil)
	snapDur := time.Since(snapStart)
	if err != nil {
		t.Fatalf("VM snapshot: %v\nshim log:\n%s", err, stderr.String())
	}
	t.Logf("BotGuard VM ran in QuickJS-on-wazero: botguardResponse=%d B in %v", len(bgResp), snapDur)

	it, err := botguard.GenerateIT(rtctx, client, "", bgResp)
	if err != nil {
		t.Fatalf("GenerateIT rejected the VM response: %v\nshim log:\n%s", err, stderr.String())
	}
	if it.HasIntegrity() {
		t.Logf("integrity token issued: lifetime=%ds threshold=%d", it.LifetimeSecs, it.RefreshThreshold)
		return
	}
	// Fallback-only is still accepted; assert it is field-6-valid.
	if !it.HasFallback() {
		t.Fatal("GenerateIT returned neither integrity nor fallback token")
	}
	if _, verr := botguard.ValidatePOToken(it.FallbackToken); verr != nil {
		t.Fatalf("fallback token failed field-6 validate: %v", verr)
	}
	t.Logf("GenerateIT accepted the VM response; field-6-valid fallback token returned (%d B), lifetime=%ds",
		len(it.FallbackToken), it.LifetimeSecs)
	if probes := stderr.String(); probes != "" {
		t.Logf("discovery / API-DRIFT log:\n%s", probes)
	}
}

// TestGateBIntegrityMint exercises the full minter path (newMinter -> mint ->
// validate). It requires GenerateIT to issue an integrity token (arr[0]).
//
// With the build/js/dom.js fidelity layer (real prototype chains, native-looking
// Function.toString, canvas/WebGL/SVG/media, platform interfaces, and Date/Intl
// coherence), this should mint and warm re-mint end-to-end. A fallback-only
// response usually means YouTube drift or IP risk-scoring after too many Create
// calls in a short window.
func TestGateBIntegrityMint(t *testing.T) {
	ctx, eng, client, stderr := liveSetup(t)
	rtctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	rt, _ := eng.NewRuntime(rtctx)
	defer rt.Close(rtctx)

	const visitorData = "Cgs4bFZSaUotYTYtQSiJnvu8BjIKCgJERRIEEgAgFw=="
	token, it, err := botguard.MintToken(rtctx, rt, client, "", visitorData)
	if err != nil {
		if se, ok := err.(*botguard.StageError); ok && se.Stage == botguard.StageGenerateIT {
			t.Skipf("integrity token not issued; GenerateIT returned fallback only. "+
				"GenerateIT stage: %v\nshim discovery log:\n%s", se.Err, stderr.String())
		}
		t.Fatalf("MintToken failed at unexpected stage: %v", err)
	}

	if _, verr := botguard.ValidatePOToken(token); verr != nil {
		t.Fatalf("minted token failed validate: %v", verr)
	}
	t.Logf("minted and validated token (%d B), integrity lifetime=%ds", len(token), it.LifetimeSecs)

	// One warm minter mints many identifiers.
	token2, err := botguard.Mint(rtctx, rt, "Cgs4bFZSaUotYTYtQSiJnvu8BjIKCgJERRIEEgAgFh==")
	if err != nil {
		t.Fatalf("warm re-mint: %v", err)
	}
	if token2 == token {
		t.Fatal("warm minter produced identical tokens for different identifiers")
	}
	t.Logf("warm re-mint OK (distinct token)")
}

// TestGateBDebug runs the flow step-by-step with full visibility (raw GenerateIT
// body plus the shim's API drift discovery log, useful when YouTube drifts.
func TestGateBDebug(t *testing.T) {
	ctx, eng, client, stderr := liveSetup(t)
	rtctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	rt, _ := eng.NewRuntime(rtctx)
	defer rt.Close(rtctx)

	ch, err := botguard.FetchCreateChallenge(rtctx, client, "")
	if err != nil {
		t.Fatalf("challenge: %v", err)
	}
	t.Logf("challenge: program=%d B, globalName=%q, interpreterJS=%d B, url=%q",
		len(ch.Program), ch.GlobalName, len(ch.InterpreterJS), ch.InterpreterURL)

	bgResp, err := botguard.Snapshot(rtctx, rt, ch, nil)
	if err != nil {
		t.Fatalf("snapshot: %v\nshim log:\n%s", err, stderr.String())
	}
	t.Logf("botguardResponse: %d B", len(bgResp))

	body, _ := json.Marshal([]string{botguard.RequestKey, bgResp})
	req, _ := http.NewRequestWithContext(rtctx, http.MethodPost, botguard.GenerateITURL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json+protobuf")
	req.Header.Set("x-goog-api-key", botguard.GoogAPIKey)
	req.Header.Set("x-user-agent", "grpc-web-javascript/0.1")
	req.Header.Set("User-Agent", botguard.DefaultUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GenerateIT http: %v", err)
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	t.Logf("GenerateIT HTTP %d, body: %s", resp.StatusCode, string(raw))
	if probes := stderr.String(); probes != "" {
		t.Logf("shim discovery / API-DRIFT log:\n%s", probes)
	}
}
