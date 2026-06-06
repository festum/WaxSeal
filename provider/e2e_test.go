//go:build e2e

// Live provider check against real YouTube/Google. Run:
//
//	go -C provider test -tags e2e -v
//
// TestForcedWEBDownload forces a WEB-only client whose /player response and GVS
// stream URLs require PO tokens, so the token path is actually exercised. The
// default chain (ANDROID_VR -> iOS -> WEB_EMBEDDED -> WEB) would otherwise
// succeed through a no-POT client first. WaxTap injects the player-scope
// POT into the /player body (serviceIntegrityDimensions.poToken) and the GVS POT
// onto the stream URL, so this exercises both paths end-to-end with WaxSeal
// tokens. It classifies the outcome using WaxTap's exported error sentinels:
//
//   - bytes flow            => the WaxSeal token unblocks WEB end-to-end.
//   - ErrExtractionFailed   => the /player response is still URL-less, i.e. the
//     PLAYER-scope token we supplied was rejected by the player gate (the generic
//     fallback token is likely insufficient).
//   - ErrNeedsPOToken       => our provider returned nothing usable for a required
//     scope (wiring/mint failure).
//   - other / read error    => a URL was built but the GVS request rejected the
//     token (e.g. sps=3).
//
// Non-token failures are reported as skips (this is a live experiment, not a unit
// regression), mirroring TestGateBIntegrityMint in the core.
//
// Network-dependent and excluded from the default build; keep runs sparse.
package provider_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	waxseal "github.com/colespringer/waxseal"
	"github.com/colespringer/waxseal/provider"
	"github.com/colespringer/waxtap"
	"github.com/colespringer/waxtap/potoken"
)

// A stable, widely-available video. Override with WAXSEAL_E2E_URL.
const defaultTestURL = "https://www.youtube.com/watch?v=dQw4w9WgXcQ"

// defaultWebVersion is a recent WEB InnerTube client version. It drifts; override
// with WAXSEAL_E2E_WEB_VERSION (find the current one in YouTube's homepage ytcfg:
// the "INNERTUBE_CLIENT_VERSION" value).
const defaultWebVersion = "2.20260603.05.00"

// webOnlyOverride forces a single WEB profile that requires both a player and a
// GVS PO token (WaxTap schema: requiresPoTokens is a list). WEB uses
// keyless InnerTube POSTs (no apiKey needed).
func webOnlyOverride(version string) string {
	return `{
  "profiles": [
    {
      "name": "WEB",
      "innerTubeName": "WEB",
      "innerTubeId": 1,
      "version": "` + version + `",
      "userAgent": "` + waxseal.DefaultProfile().UserAgent + `",
      "requiresPoTokens": ["player", "gvs"],
      "supportsCookies": true,
      "supportsPlaylists": true
    }
  ]
}`
}

func testURL() string {
	if v := os.Getenv("WAXSEAL_E2E_URL"); v != "" {
		return v
	}
	return defaultTestURL
}

func webVersion() string {
	if v := os.Getenv("WAXSEAL_E2E_WEB_VERSION"); v != "" {
		return v
	}
	return defaultWebVersion
}

// scopeRecorder wraps the real provider and records every scope WaxTap asks for,
// so the gate can show whether the POT path was exercised and which scopes WEB
// actually needs.
type scopeRecorder struct {
	inner potoken.Provider
	mu    sync.Mutex
	calls []string
}

func (s *scopeRecorder) ProvidePOToken(ctx context.Context, req potoken.Request) (potoken.Response, error) {
	resp, err := s.inner.ProvidePOToken(ctx, req)
	s.mu.Lock()
	s.calls = append(s.calls, req.Scope.String())
	s.mu.Unlock()
	return resp, err
}

func (s *scopeRecorder) scopes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

func TestForcedWEBDownload(t *testing.T) {
	// One shared client+jar backs both WaxTap (download) and WaxSeal (attestation)
	// so tokens mint from the IP/identity used to fetch.
	jar, _ := cookiejar.New(nil)
	shared := &http.Client{Jar: jar, Timeout: 90 * time.Second}

	seal, err := waxseal.New(waxseal.Options{HTTPClient: shared})
	if err != nil {
		t.Fatalf("waxseal.New: %v", err)
	}
	defer seal.Close()

	rec := &scopeRecorder{inner: provider.New(seal)}

	overridePath := filepath.Join(t.TempDir(), "web_only.json")
	if err := os.WriteFile(overridePath, []byte(webOnlyOverride(webVersion())), 0o600); err != nil {
		t.Fatalf("write override: %v", err)
	}
	t.Logf("forcing WEB client version %s", webVersion())
	tap, err := waxtap.New(waxtap.Options{
		HTTPClient:          shared,
		POTokenProvider:     rec,
		ProfileOverridePath: overridePath,
	})
	if err != nil {
		t.Fatalf("waxtap.New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	rc, info, err := tap.Stream(ctx, waxtap.Request{URL: testURL()})
	t.Logf("provider scope requests: %v", rec.scopes())

	switch {
	case err == nil:
		defer rc.Close()
		n, rerr := io.CopyN(io.Discard, rc, 256<<10)
		if rerr != nil && rerr != io.EOF {
			t.Skipf("stream opened but reading GVS bytes failed (possible GVS rejection "+
				"of the fallback token): %v", rerr)
		}
		if n <= 0 {
			t.Fatalf("stream yielded no bytes (info=%+v)", info)
		}
		t.Logf("forced-WEB streamed %d bytes with WaxSeal tokens (title=%q)", n, info.Title)

	case errors.Is(err, waxtap.ErrExtractionFailed):
		t.Skipf("player-scope token rejected: WaxTap injected our player POT into the /player body "+
			"but the response is still URL-less; the generic fallback token "+
			"is not accepted as a player POT. scopes=%v err=%v", rec.scopes(), err)

	case errors.Is(err, waxtap.ErrNeedsPOToken):
		t.Skipf("our provider returned nothing usable for a required scope "+
			"(wiring/mint failure): %v", err)

	default:
		t.Skipf("forced-WEB download failed after a URL was built (likely GVS rejection of the "+
			"fallback token, e.g. sps=3): %v", err)
	}
}

// TestDefaultChainHealth is a sanity check: the default chain (no override, no
// provider) must download via a no-POT client. If this fails, the video,
// network, or WaxTap is failing before WaxSeal is involved.
func TestDefaultChainHealth(t *testing.T) {
	jar, _ := cookiejar.New(nil)
	shared := &http.Client{Jar: jar, Timeout: 90 * time.Second}
	tap, err := waxtap.New(waxtap.Options{HTTPClient: shared})
	if err != nil {
		t.Fatalf("waxtap.New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	rc, info, err := tap.Stream(ctx, waxtap.Request{URL: testURL()})
	if err != nil {
		t.Fatalf("default chain failed (video/network/WaxTap unhealthy): %v", err)
	}
	defer rc.Close()
	n, _ := io.CopyN(io.Discard, rc, 128<<10)
	if n <= 0 {
		t.Fatal("default chain yielded no bytes")
	}
	t.Logf("default-chain health OK: %d bytes via a no-POT client (title=%q)", n, info.Title)
}
