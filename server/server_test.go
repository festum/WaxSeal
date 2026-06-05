package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	waxseal "github.com/colespringer/waxseal"
	"github.com/colespringer/waxseal/internal/botguard"
	"github.com/colespringer/waxseal/internal/httpx"
)

// fakeClient records the last Token request and returns canned results.
type fakeClient struct {
	lastReq             waxseal.Request
	tokenErr            error
	vd                  string
	vdErr               error
	purged, invalidated int
	keys                []string
}

func (f *fakeClient) Token(_ context.Context, req waxseal.Request) (waxseal.Token, error) {
	f.lastReq = req
	if f.tokenErr != nil {
		return waxseal.Token{}, f.tokenErr
	}
	return waxseal.Token{Value: "POTOKEN", ExpiresAt: time.Unix(1_900_000_000, 0)}, nil
}
func (f *fakeClient) VisitorData(context.Context, waxseal.EgressSpec) (string, error) {
	return f.vd, f.vdErr
}
func (f *fakeClient) PurgeTokens()         { f.purged++ }
func (f *fakeClient) InvalidateMinters()   { f.invalidated++ }
func (f *fakeClient) MinterKeys() []string { return f.keys }
func (f *fakeClient) WriteMetrics(w io.Writer) error {
	_, err := io.WriteString(w, "# waxseal metrics\nwaxseal_snapshots_total 0\n")
	return err
}

func do(t *testing.T, h http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestGetPotSuccess(t *testing.T) {
	fc := &fakeClient{}
	h := New(fc, Options{}).Handler()

	w := do(t, h, "POST", "/get_pot", `{"content_binding":"vid123"}`, nil)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var resp potResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.PoToken != "POTOKEN" || resp.ContentBinding != "vid123" {
		t.Fatalf("resp = %+v", resp)
	}
	if _, err := time.Parse(time.RFC3339, resp.ExpiresAt); err != nil {
		t.Fatalf("expiresAt not RFC3339: %q", resp.ExpiresAt)
	}
	if fc.lastReq.Scope != waxseal.ScopeOpaque || fc.lastReq.Identifier != "vid123" {
		t.Fatalf("request mapped wrong: %+v", fc.lastReq)
	}
}

func TestGetPotDeprecatedFields(t *testing.T) {
	h := New(&fakeClient{}, Options{}).Handler()
	for _, field := range []string{"visitor_data", "data_sync_id"} {
		w := do(t, h, "POST", "/get_pot", fmt.Sprintf(`{%q:"x","content_binding":"v"}`, field), nil)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", field, w.Code)
		}
		if !strings.Contains(w.Body.String(), "deprecated") {
			t.Errorf("%s: body = %s", field, w.Body)
		}
	}
}

func TestGetPotBypassCacheThreads(t *testing.T) {
	fc := &fakeClient{}
	h := New(fc, Options{}).Handler()
	do(t, h, "POST", "/get_pot", `{"content_binding":"v","bypass_cache":true}`, nil)
	if !fc.lastReq.BypassCache {
		t.Fatal("bypass_cache not threaded into the request")
	}
}

func TestGetPotEmptyBindingSourcesVisitorData(t *testing.T) {
	fc := &fakeClient{vd: "GENERATED_VD"}
	h := New(fc, Options{}).Handler()
	w := do(t, h, "POST", "/get_pot", `{}`, nil)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var resp potResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.ContentBinding != "GENERATED_VD" {
		t.Fatalf("binding = %q, want the sourced visitor_data", resp.ContentBinding)
	}
	if fc.lastReq.Identifier != "GENERATED_VD" {
		t.Fatalf("identifier = %q", fc.lastReq.Identifier)
	}
}

func TestEgressOverrideGating(t *testing.T) {
	body := `{"content_binding":"v","proxy":"http://p:8080","source_address":"10.0.0.1"}`

	t.Run("ignored by default", func(t *testing.T) {
		fc := &fakeClient{}
		do(t, New(fc, Options{}).Handler(), "POST", "/get_pot", body, nil)
		if fc.lastReq.Egress.Proxy != "" || fc.lastReq.Egress.ID != "" {
			t.Fatalf("egress override leaked through: %+v", fc.lastReq.Egress)
		}
	})

	t.Run("honored when allowed", func(t *testing.T) {
		fc := &fakeClient{}
		do(t, New(fc, Options{AllowRequestEgressOverride: true}).Handler(), "POST", "/get_pot", body, nil)
		if fc.lastReq.Egress.Proxy != "http://p:8080" || fc.lastReq.Egress.SourceAddress != "10.0.0.1" {
			t.Fatalf("egress override not applied: %+v", fc.lastReq.Egress)
		}
		if fc.lastReq.Egress.ID == "" {
			t.Fatal("egress ID should be derived from the spec")
		}
	})
}

func TestInlineChallengeGating(t *testing.T) {
	inline := `{"content_binding":"v","challenge":["x",["INLINE_JS=1;"],[],0,"P","g"]}`
	urlCh := `{"content_binding":"v","challenge":{"interpreterUrl":{"privateDoNotAccessOrElseTrustedResourceUrlWrappedValue":"//www.google.com/x.js"},"program":"P","globalName":"g"}}`

	t.Run("inline rejected without auth", func(t *testing.T) {
		w := do(t, New(&fakeClient{}, Options{}).Handler(), "POST", "/get_pot", inline, nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status %d, want 400", w.Code)
		}
	})
	t.Run("url challenge allowed without auth", func(t *testing.T) {
		w := do(t, New(&fakeClient{}, Options{}).Handler(), "POST", "/get_pot", urlCh, nil)
		if w.Code != 200 {
			t.Fatalf("status %d: %s", w.Code, w.Body)
		}
	})
	t.Run("inline allowed with auth", func(t *testing.T) {
		h := New(&fakeClient{}, Options{SharedSecret: "s3cret"}).Handler()
		w := do(t, h, "POST", "/get_pot", inline, map[string]string{SecretHeader: "s3cret"})
		if w.Code != 200 {
			t.Fatalf("status %d: %s", w.Code, w.Body)
		}
	})
}

func TestSharedSecret(t *testing.T) {
	h := New(&fakeClient{}, Options{SharedSecret: "s3cret"}).Handler()

	if w := do(t, h, "POST", "/get_pot", `{"content_binding":"v"}`, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("no secret: status %d, want 401", w.Code)
	}
	if w := do(t, h, "POST", "/get_pot", `{"content_binding":"v"}`, map[string]string{SecretHeader: "wrong"}); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong secret: status %d, want 401", w.Code)
	}
	if w := do(t, h, "POST", "/get_pot", `{"content_binding":"v"}`, map[string]string{SecretHeader: "s3cret"}); w.Code != 200 {
		t.Fatalf("valid secret: status %d: %s", w.Code, w.Body)
	}
	// /ping is exempt from auth (health check).
	if w := do(t, h, "GET", "/ping", "", nil); w.Code != 200 {
		t.Fatalf("/ping should be unauthenticated, got %d", w.Code)
	}
}

func TestPing(t *testing.T) {
	h := New(&fakeClient{}, Options{Version: "1.2.3"}).Handler()
	w := do(t, h, "GET", "/ping", "", nil)
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	var resp struct {
		ServerUptime int64  `json:"server_uptime"`
		Version      string `json:"version"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Version != "1.2.3" {
		t.Errorf("version = %q", resp.Version)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	h := New(&fakeClient{}, Options{}).Handler()
	w := do(t, h, "GET", "/metrics", "", nil)
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want Prometheus text", ct)
	}
	if !strings.Contains(w.Body.String(), "waxseal_snapshots_total") {
		t.Errorf("metrics body missing a known series:\n%s", w.Body.String())
	}
}

func TestInvalidateAndMinterCache(t *testing.T) {
	fc := &fakeClient{keys: []string{"k1", "k2"}}
	h := New(fc, Options{}).Handler()

	if w := do(t, h, "POST", "/invalidate_caches", "", nil); w.Code != http.StatusNoContent || fc.purged != 1 {
		t.Fatalf("invalidate_caches: status %d, purged %d", w.Code, fc.purged)
	}
	if w := do(t, h, "POST", "/invalidate_it", "", nil); w.Code != http.StatusNoContent || fc.invalidated != 1 {
		t.Fatalf("invalidate_it: status %d, invalidated %d", w.Code, fc.invalidated)
	}

	w := do(t, h, "GET", "/minter_cache", "", nil)
	if w.Code != 200 {
		t.Fatalf("minter_cache: status %d", w.Code)
	}
	var keys []string
	if err := json.Unmarshal(w.Body.Bytes(), &keys); err != nil {
		t.Fatalf("minter_cache not a JSON array: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("keys = %v", keys)
	}
}

func TestMinterCacheEmptyIsArray(t *testing.T) {
	h := New(&fakeClient{keys: nil}, Options{}).Handler()
	w := do(t, h, "GET", "/minter_cache", "", nil)
	if got := strings.TrimSpace(w.Body.String()); got != "[]" {
		t.Fatalf("empty minter cache = %q, want []", got)
	}
}

func TestMintErrorStatuses(t *testing.T) {
	t.Run("generic 500 with stage", func(t *testing.T) {
		fc := &fakeClient{tokenErr: &botguard.StageError{Stage: botguard.StageGenerateIT, Err: fmt.Errorf("boom")}}
		w := do(t, New(fc, Options{}).Handler(), "POST", "/get_pot", `{"content_binding":"v"}`, nil)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status %d, want 500", w.Code)
		}
		if !strings.Contains(w.Body.String(), "generateit") {
			t.Errorf("missing stage tag: %s", w.Body)
		}
	})
	t.Run("breaker open 503", func(t *testing.T) {
		fc := &fakeClient{tokenErr: fmt.Errorf("session: %w", httpx.ErrBreakerOpen)}
		w := do(t, New(fc, Options{}).Handler(), "POST", "/get_pot", `{"content_binding":"v"}`, nil)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status %d, want 503", w.Code)
		}
	})
}

func TestMethodAndPathRouting(t *testing.T) {
	h := New(&fakeClient{}, Options{}).Handler()
	if w := do(t, h, "GET", "/get_pot", "", nil); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /get_pot: status %d, want 405", w.Code)
	}
	if w := do(t, h, "GET", "/nope", "", nil); w.Code != http.StatusNotFound {
		t.Errorf("unknown path: status %d, want 404", w.Code)
	}
}

func TestInvalidJSON(t *testing.T) {
	h := New(&fakeClient{}, Options{}).Handler()
	w := do(t, h, "POST", "/get_pot", `{not json`, nil)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422", w.Code)
	}
}
