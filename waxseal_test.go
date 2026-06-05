package waxseal

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colespringer/waxseal/internal/jsruntime"
)

// Test fakes.

type fakeEngine struct{ created atomic.Int32 }

func (e *fakeEngine) NewRuntime(ctx context.Context) (jsruntime.Runtime, error) {
	e.created.Add(1)
	return &fakeRuntime{}, nil
}
func (e *fakeEngine) Close(ctx context.Context) error { return nil }

type fakeRuntime struct{}

func (r *fakeRuntime) Eval(ctx context.Context, src string) (json.RawMessage, error) {
	return json.RawMessage("null"), nil
}
func (r *fakeRuntime) Call(ctx context.Context, name string, args ...any) (json.RawMessage, error) {
	switch name {
	case "runBotguard":
		return json.Marshal("BOTGUARD_RESPONSE")
	case "newMinter":
		return json.RawMessage("true"), nil
	case "mint":
		id, _ := args[0].(string)
		return json.Marshal(validToken("mint-" + id))
	}
	return json.RawMessage("null"), nil
}
func (r *fakeRuntime) SetWatchdog(time.Duration)       {}
func (r *fakeRuntime) Poisoned() bool                  { return false }
func (r *fakeRuntime) Close(ctx context.Context) error { return nil }

type fakeTransport struct {
	createCount atomic.Int32
	genIT       func() string
}

func (f *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	mk := func(body string) *http.Response {
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
			Request:    req,
		}
	}
	switch {
	case strings.HasSuffix(req.URL.Path, "/Create"):
		f.createCount.Add(1)
		return mk(`[["v",["VAR=1;"],[],0,"PROGRAM","globalName"]]`), nil
	case strings.HasSuffix(req.URL.Path, "/GenerateIT"):
		return mk(f.genIT()), nil
	}
	return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header), Request: req}, nil
}

func validToken(payload string) string {
	raw := append([]byte{0x32, byte(len(payload))}, []byte(payload)...)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func fallbackGenIT() string { return fmt.Sprintf(`[null,3600,null,%q]`, validToken("fallback")) }
func integrityGenIT() string {
	return fmt.Sprintf(`[%q,3600,1800,%q]`, "INTEGRITY", validToken("fallback"))
}

func newTestClient(t *testing.T, tr *fakeTransport) (*Client, *fakeEngine) {
	t.Helper()
	eng := &fakeEngine{}
	c, err := New(Options{
		HTTPClient: &http.Client{Transport: tr},
		engine:     eng,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, eng
}

const sampleVisitorData = "Cgs4bFZSaUotYTYtQSiJnvu8BjIKCgJERRIEEgAgFw=="

// Client behavior.

func TestClientFallbackTokenAndCache(t *testing.T) {
	tr := &fakeTransport{genIT: fallbackGenIT}
	c, _ := newTestClient(t, tr)

	tok, err := c.Token(context.Background(), Request{Scope: ScopeSession, VisitorData: sampleVisitorData})
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok.Value == "" {
		t.Fatal("empty token value")
	}
	if tok.ExpiresAt.IsZero() {
		t.Fatal("token has no expiry")
	}

	// Second call hits the cache; no new Create.
	tok2, err := c.Token(context.Background(), Request{Scope: ScopeSession, VisitorData: sampleVisitorData})
	if err != nil {
		t.Fatalf("Token (cached): %v", err)
	}
	if tok2.Value != tok.Value {
		t.Fatal("cache returned a different token")
	}
	if got := tr.createCount.Load(); got != 1 {
		t.Fatalf("Create called %d times, want 1 (cache hit)", got)
	}
}

func TestClientBypassCacheReattests(t *testing.T) {
	tr := &fakeTransport{genIT: fallbackGenIT}
	c, _ := newTestClient(t, tr)

	base := Request{Scope: ScopeSession, VisitorData: sampleVisitorData}
	if _, err := c.Token(context.Background(), base); err != nil {
		t.Fatalf("Token: %v", err)
	}
	bypass := base
	bypass.BypassCache = true
	if _, err := c.Token(context.Background(), bypass); err != nil {
		t.Fatalf("Token (bypass): %v", err)
	}
	if got := tr.createCount.Load(); got != 2 {
		t.Fatalf("Create called %d times, want 2 (BypassCache re-attests)", got)
	}
}

func TestClientScopeNoneNoop(t *testing.T) {
	tr := &fakeTransport{genIT: fallbackGenIT}
	c, _ := newTestClient(t, tr)

	tok, err := c.Token(context.Background(), Request{Scope: ScopeNone})
	if err != nil {
		t.Fatalf("ScopeNone: %v", err)
	}
	if tok.Value != "" {
		t.Fatal("ScopeNone should yield an empty token")
	}
	if got := tr.createCount.Load(); got != 0 {
		t.Fatalf("ScopeNone attested %d times, want 0", got)
	}
}

func TestClientMissingIdentifier(t *testing.T) {
	tr := &fakeTransport{genIT: fallbackGenIT}
	c, _ := newTestClient(t, tr)

	_, err := c.Token(context.Background(), Request{Scope: ScopeSession}) // no VisitorData
	if !errors.Is(err, ErrMissingIdentifier) {
		t.Fatalf("want ErrMissingIdentifier, got %v", err)
	}
}

func TestClientUnsupportedClient(t *testing.T) {
	tr := &fakeTransport{genIT: fallbackGenIT}
	c, _ := newTestClient(t, tr)

	_, err := c.Token(context.Background(), Request{
		Scope:       ScopeSession,
		VisitorData: sampleVisitorData,
		UserAgent:   "curl/8.4.0", // not WebKit-family
	})
	if !errors.Is(err, ErrUnsupportedClient) {
		t.Fatalf("want ErrUnsupportedClient, got %v", err)
	}
}

func TestClientSessionTokenConvenience(t *testing.T) {
	tr := &fakeTransport{genIT: fallbackGenIT}
	c, _ := newTestClient(t, tr)

	tok, err := c.SessionToken(context.Background(), sampleVisitorData)
	if err != nil {
		t.Fatalf("SessionToken: %v", err)
	}
	if tok.Value == "" {
		t.Fatal("empty token")
	}
}

func TestClientEgressTransportPerSpec(t *testing.T) {
	tr := &fakeTransport{genIT: fallbackGenIT}
	var mu sync.Mutex
	builds := map[string]int{}
	c, err := New(Options{
		EgressTransport: func(s EgressSpec) (http.RoundTripper, error) {
			mu.Lock()
			builds[s.ID]++
			mu.Unlock()
			return tr, nil
		},
		engine: &fakeEngine{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	ctx := context.Background()
	// Two requests on the same egress reuse one built transport.
	for _, id := range []string{"id1", "id2"} {
		if _, err := c.Token(ctx, Request{Scope: ScopeOpaque, Identifier: id, Egress: EgressSpec{ID: "p1"}}); err != nil {
			t.Fatalf("token %s: %v", id, err)
		}
	}
	// A different egress builds its own.
	if _, err := c.Token(ctx, Request{Scope: ScopeOpaque, Identifier: "z", Egress: EgressSpec{ID: "p2"}}); err != nil {
		t.Fatalf("token p2: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if builds["p1"] != 1 {
		t.Errorf("egress p1 built %d times, want 1 (cached)", builds["p1"])
	}
	if builds["p2"] != 1 {
		t.Errorf("egress p2 built %d times, want 1", builds["p2"])
	}
}

func TestClientCallerChallengeSkipsFetch(t *testing.T) {
	tr := &fakeTransport{genIT: fallbackGenIT}
	c, _ := newTestClient(t, tr)

	// An inline-JS array challenge needs neither att/get nor Create.
	challenge := json.RawMessage(`["v",["INLINE=1;"],[],0,"PROG","gn"]`)
	_, err := c.Token(context.Background(), Request{Scope: ScopeOpaque, Identifier: "vid", Challenge: challenge})
	if err != nil {
		t.Fatalf("Token with caller challenge: %v", err)
	}
	if got := tr.createCount.Load(); got != 0 {
		t.Fatalf("Create called %d times despite a caller-provided challenge", got)
	}
}

func TestClientNullChallengeAbsent(t *testing.T) {
	tr := &fakeTransport{genIT: fallbackGenIT}
	c, _ := newTestClient(t, tr)

	// "challenge": null is how some bgutil-compatible clients serialize an
	// omitted field. It should fall through to the InnerTube/Create chain instead
	// of failing to parse.
	if _, err := c.Token(context.Background(), Request{Scope: ScopeOpaque, Identifier: "vid", Challenge: json.RawMessage("null")}); err != nil {
		t.Fatalf("null challenge should be treated as absent, got: %v", err)
	}
	if got := tr.createCount.Load(); got != 1 {
		t.Fatalf("Create hit %d times, want 1 (fell through)", got)
	}

	// Malformed challenges still fail.
	if _, err := c.Token(context.Background(), Request{Scope: ScopeOpaque, Identifier: "vid2", Challenge: json.RawMessage(`"not-a-challenge"`)}); err == nil {
		t.Fatal("a malformed challenge should still error")
	}
}

func TestClientEgressEmptyIDDistinctProxies(t *testing.T) {
	tr := &fakeTransport{genIT: fallbackGenIT}
	var mu sync.Mutex
	built := map[string]int{}
	c, err := New(Options{
		EgressTransport: func(s EgressSpec) (http.RoundTripper, error) {
			mu.Lock()
			built[s.Proxy]++
			mu.Unlock()
			return tr, nil
		},
		engine: &fakeEngine{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	ctx := context.Background()
	// Two requests differ only by proxy, both with an empty Egress.ID. Distinct
	// proxies need distinct transports and minters.
	for _, proxy := range []string{"http://a:8080", "http://b:8080"} {
		if _, err := c.Token(ctx, Request{Scope: ScopeOpaque, Identifier: "x", Egress: EgressSpec{Proxy: proxy}}); err != nil {
			t.Fatalf("token via %s: %v", proxy, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if built["http://a:8080"] != 1 || built["http://b:8080"] != 1 {
		t.Fatalf("distinct empty-ID proxies collided on the transport cache: %v", built)
	}
	if got := len(c.MinterKeys()); got != 2 {
		t.Fatalf("minter keys = %d, want 2 (distinct egresses must not share a warm minter)", got)
	}
}

func TestClientManagementSurface(t *testing.T) {
	tr := &fakeTransport{genIT: integrityGenIT}
	c, _ := newTestClient(t, tr)

	req := Request{Scope: ScopeSession, VisitorData: sampleVisitorData}
	if _, err := c.Token(context.Background(), req); err != nil {
		t.Fatalf("Token: %v", err)
	}
	if got := len(c.MinterKeys()); got != 1 {
		t.Fatalf("MinterKeys len = %d, want 1", got)
	}

	// A clean slate must force a fresh attestation on the next call.
	c.PurgeTokens()
	c.InvalidateMinters()
	if got := len(c.MinterKeys()); got != 0 {
		t.Fatalf("MinterKeys len = %d after invalidate, want 0", got)
	}
	if _, err := c.Token(context.Background(), req); err != nil {
		t.Fatalf("Token after invalidate: %v", err)
	}
	if got := tr.createCount.Load(); got != 2 {
		t.Fatalf("Create called %d times, want 2 (re-attest after invalidate+purge)", got)
	}
}

func TestClientClosed(t *testing.T) {
	tr := &fakeTransport{genIT: fallbackGenIT}
	c, _ := newTestClient(t, tr)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := c.Token(context.Background(), Request{Scope: ScopeSession, VisitorData: sampleVisitorData}); !errors.Is(err, ErrClosed) {
		t.Fatalf("want ErrClosed, got %v", err)
	}
}
