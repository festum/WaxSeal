package waxseal

import (
	"net/http"
	"testing"
)

func TestDerivedID(t *testing.T) {
	if id := (EgressSpec{}).DerivedID(); id != "" {
		t.Errorf("empty spec should derive empty ID, got %q", id)
	}
	a := EgressSpec{Proxy: "http://p:8080"}.DerivedID()
	b := EgressSpec{Proxy: "http://p:8080"}.DerivedID()
	if a == "" || a != b {
		t.Errorf("same spec should derive the same non-empty ID: %q vs %q", a, b)
	}
	if (EgressSpec{Proxy: "http://p:8080"}).DerivedID() == (EgressSpec{SourceAddress: "10.0.0.1"}).DerivedID() {
		t.Error("different egress fields must derive different IDs")
	}
	if (EgressSpec{DisableTLSVerify: true}).DerivedID() == "" {
		t.Error("TLS-verify-off is an egress difference and must derive a non-empty ID")
	}
}

func TestBuildEgressTransport(t *testing.T) {
	rt, err := BuildEgressTransport(EgressSpec{})
	if err != nil {
		t.Fatalf("default: %v", err)
	}
	if _, ok := rt.(*http.Transport); !ok {
		t.Fatalf("want *http.Transport, got %T", rt)
	}

	if _, err := BuildEgressTransport(EgressSpec{Proxy: "://bad"}); err == nil {
		t.Error("expected error for invalid proxy URL")
	}
	if _, err := BuildEgressTransport(EgressSpec{SourceAddress: "not-an-ip"}); err == nil {
		t.Error("expected error for invalid source address")
	}

	tr, err := BuildEgressTransport(EgressSpec{Proxy: "http://proxy.example:8080", SourceAddress: "127.0.0.1", DisableTLSVerify: true})
	if err != nil {
		t.Fatalf("full spec: %v", err)
	}
	ht := tr.(*http.Transport)
	if ht.Proxy == nil {
		t.Error("proxy not configured")
	}
	if ht.TLSClientConfig == nil || !ht.TLSClientConfig.InsecureSkipVerify {
		t.Error("DisableTLSVerify not applied")
	}
}

func TestEgressCacheReusesAndEvicts(t *testing.T) {
	var builds int
	build := func(EgressSpec) (http.RoundTripper, error) {
		builds++
		return &http.Transport{}, nil
	}
	c := newEgressCache(2, 0)

	a1, _ := c.getOrBuild(EgressSpec{ID: "a"}, build)
	a2, _ := c.getOrBuild(EgressSpec{ID: "a"}, build) // cache hit
	if a1 != a2 {
		t.Error("same ID should return the same client")
	}
	if builds != 1 {
		t.Fatalf("built %d times for one ID, want 1", builds)
	}

	_, _ = c.getOrBuild(EgressSpec{ID: "b"}, build)
	_, _ = c.getOrBuild(EgressSpec{ID: "c"}, build) // evicts LRU ("a")
	if len(c.byKey) != 2 {
		t.Fatalf("cache size %d, want 2 (bounded)", len(c.byKey))
	}
	if _, ok := c.byKey["a"]; ok {
		t.Error("LRU 'a' should have been evicted")
	}
}

func TestEgressCacheBuildErrorNotCached(t *testing.T) {
	c := newEgressCache(4, 0)
	failing := func(EgressSpec) (http.RoundTripper, error) { return nil, errTest }
	if _, err := c.getOrBuild(EgressSpec{ID: "x"}, failing); err == nil {
		t.Fatal("expected build error")
	}
	if len(c.byKey) != 0 {
		t.Fatal("a failed build must not be cached")
	}
}

var errTest = &testErr{}

type testErr struct{}

func (*testErr) Error() string { return "build failed" }
