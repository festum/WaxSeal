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
	"strings"
	"testing"
	"time"

	waxseal "github.com/colespringer/waxseal"
	"github.com/colespringer/waxseal/internal/botguard"
	"github.com/colespringer/waxseal/internal/httpx"
	"github.com/colespringer/waxseal/internal/innertube"
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

	ch, err := botguard.FetchCreateChallenge(rtctx, client, "", botguard.DefaultEndpoint)
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

	it, err := botguard.GenerateIT(rtctx, client, "", bgResp, botguard.DefaultEndpoint)
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
		t.Logf("discovery / API drift log:\n%s", probes)
	}
}

// TestGateBIntegrityMint exercises the full minter path (newMinter -> mint ->
// validate) using the production challenge order: InnerTube att/get, then WAA
// Create. The test skips when GenerateIT returns only a fallback token.
func TestGateBIntegrityMint(t *testing.T) {
	ctx, eng, client, stderr := liveSetup(t)
	rtctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	rt, _ := eng.NewRuntime(rtctx)
	defer rt.Close(rtctx)

	const visitorData = "Cgs4bFZSaUotYTYtQSiJnvu8BjIKCgJERRIEEgAgFw=="
	// Use the same Chrome version as the browser shim.
	chromeUA := waxseal.DefaultProfile().UserAgent

	// Anchor att/get to the same visitor_data used for minting.
	ch, err := innertube.GetChallenge(rtctx, client, chromeUA, innertube.GuestContext(visitorData, ""))
	if err != nil {
		t.Logf("att/get failed (%v); falling back to WAA Create", err)
		if ch, err = botguard.FetchCreateChallenge(rtctx, client, chromeUA, botguard.DefaultEndpoint); err != nil {
			t.Fatalf("challenge: %v", err)
		}
	}

	bgResp, err := botguard.Snapshot(rtctx, rt, ch, nil)
	if err != nil {
		t.Fatalf("snapshot: %v\nshim discovery log:\n%s", err, stderr.String())
	}
	it, err := botguard.GenerateIT(rtctx, client, chromeUA, bgResp, botguard.DefaultEndpoint)
	if err != nil {
		t.Fatalf("GenerateIT: %v\nshim discovery log:\n%s", err, stderr.String())
	}

	// Fallback-only responses can result from session or IP reputation limits.
	if it.HasFallback() && !it.HasIntegrity() {
		log := stderr.String()
		diag := "no missing browser APIs detected; likely a session or IP reputation issue; retry sparingly"
		if probes := botguard.DriftProbes(log); len(probes) > 0 {
			diag = "missing browser APIs: " + strings.Join(probes, ", ")
		}
		t.Skipf("fallback only, no integrity token. %s\nshim discovery log:\n%s", diag, log)
	}

	if err := botguard.InstallMinter(rtctx, rt, it.IntegrityToken); err != nil {
		t.Fatalf("InstallMinter: %v\nshim discovery log:\n%s", err, stderr.String())
	}
	token, err := botguard.Mint(rtctx, rt, visitorData) // validates field 6 internally
	if err != nil {
		t.Fatalf("mint: %v\nshim discovery log:\n%s", err, stderr.String())
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

	ch, err := botguard.FetchCreateChallenge(rtctx, client, "", botguard.DefaultEndpoint)
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
		t.Logf("shim discovery / API drift log:\n%s", probes)
	}
}
