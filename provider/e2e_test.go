//go:build e2e

package provider_test

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"testing"
	"time"

	"github.com/colespringer/waxseal/client"
	"github.com/colespringer/waxseal/provider"
	waxtap "github.com/colespringer/waxtap"
)

// TestForcedWEBDownloadHTTP is the realistic WaxBin-style integration: the WaxTap
// library mints PO tokens from a *running WaxSeal daemon over HTTP* and adopts
// its coherent {visitor_data, cookies} session, then downloads WEB SABR audio.
//
// Gated on WAXSEAL_URL (a running daemon, e.g. `waxseal server` →
// http://127.0.0.1:4416). Optionally WAXSEAL_KEY for a multi-tenant daemon.
// Manual / not in CI: it needs a real Chromium (the daemon) + network.
func TestForcedWEBDownloadHTTP(t *testing.T) {
	base := os.Getenv("WAXSEAL_URL")
	if base == "" {
		t.Skip("set WAXSEAL_URL to a running waxseal daemon (e.g. http://127.0.0.1:4416)")
	}
	p := provider.New(client.New(base, client.WithAPIKey(os.Getenv("WAXSEAL_KEY"))))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// Adopt WaxSeal's coherent guest session (visitor_data + cookies).
	sess, err := p.Session(ctx)
	if err != nil {
		t.Fatalf("session handoff: %v", err)
	}
	if sess.VisitorData == "" {
		t.Fatalf("daemon returned an empty visitor_data")
	}
	t.Logf("adopted session: visitor_data=%d chars, %d cookies", len(sess.VisitorData), len(sess.Cookies))

	jar, _ := cookiejar.New(nil)
	tap, err := waxtap.New(waxtap.Options{
		HTTPClient:      &http.Client{Jar: jar, Timeout: 90 * time.Second},
		POTokenProvider: p,     // mint PO tokens from WaxSeal over HTTP
		Session:         sess,  // adopt the coherent identity verbatim
		Client:          "WEB", // a uniform client chain is required for session adoption
	})
	if err != nil {
		t.Fatalf("waxtap.New: %v", err)
	}

	// Big Buck Bunny (Blender, Creative Commons); never a copyrighted work.
	rc, info, err := tap.Stream(ctx, waxtap.Request{URL: "https://www.youtube.com/watch?v=aqz-KE-bpKQ"})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer rc.Close()
	n, rerr := io.CopyN(io.Discard, rc, 256<<10)
	if rerr != nil && rerr != io.EOF {
		t.Fatalf("read stream: %v", rerr)
	}
	if n <= 0 {
		t.Fatalf("stream yielded no bytes (info=%+v)", info)
	}
	t.Logf("SUCCESS: streamed %d bytes via WaxSeal-over-HTTP (title=%q, contentLength=%d)", n, info.Title, info.ContentLength)
}
