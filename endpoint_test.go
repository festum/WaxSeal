package waxseal

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

type recordingRoundTripper struct {
	mu         sync.Mutex
	createHost string
	genITHost  string
}

func (rt *recordingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	body := ""
	switch {
	case strings.HasSuffix(req.URL.Path, "/Create"):
		rt.mu.Lock()
		rt.createHost = req.URL.Host
		rt.mu.Unlock()
		body = `[["v",["VAR=1;"],[],0,"PROGRAM","globalName"]]`
	case strings.HasSuffix(req.URL.Path, "/GenerateIT"):
		rt.mu.Lock()
		rt.genITHost = req.URL.Host
		rt.mu.Unlock()
		body = fallbackGenIT()
	default:
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header), Request: req}, nil
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}, nil
}

func (rt *recordingRoundTripper) hosts() (string, string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.createHost, rt.genITHost
}

func TestClientEndpointModeInvalid(t *testing.T) {
	if _, err := New(Options{engine: &fakeEngine{}, EndpointMode: "bogus"}); err == nil {
		t.Fatal("expected New to reject an invalid endpoint mode")
	}
}

func TestClientEndpointModeRoutesToHost(t *testing.T) {
	disable := true
	for _, tc := range []struct{ mode, wantHost string }{
		{"", "www.youtube.com"},
		{"youtube", "www.youtube.com"},
		{"googleapis", "jnn-pa.googleapis.com"},
	} {
		rt := &recordingRoundTripper{}
		c, err := New(Options{
			HTTPClient:   &http.Client{Transport: rt},
			engine:       &fakeEngine{},
			EndpointMode: tc.mode,
		})
		if err != nil {
			t.Fatalf("New(%q): %v", tc.mode, err)
		}
		_, err = c.Token(context.Background(), Request{
			Scope:            ScopeSession,
			VisitorData:      sampleVisitorData,
			DisableInnertube: &disable, // straight to Create
		})
		_ = c.Close()
		if err != nil {
			t.Fatalf("Token(%q): %v", tc.mode, err)
		}
		createHost, genITHost := rt.hosts()
		if createHost != tc.wantHost {
			t.Errorf("mode %q: Create host = %q, want %q", tc.mode, createHost, tc.wantHost)
		}
		if genITHost != tc.wantHost {
			t.Errorf("mode %q: GenerateIT host = %q, want %q", tc.mode, genITHost, tc.wantHost)
		}
	}
}

// TestClientEndpointModeInKey confirms the endpoint mode is part of the
// minter/cache key, so switching modes does not collide cache entries.
func TestClientEndpointModeInKey(t *testing.T) {
	mk := func(mode string) *Client {
		c, err := New(Options{engine: &fakeEngine{}, EndpointMode: mode})
		if err != nil {
			t.Fatalf("New(%q): %v", mode, err)
		}
		return c
	}
	cYT := mk("youtube")
	cGA := mk("googleapis")
	req := Request{Scope: ScopeSession, VisitorData: sampleVisitorData}
	p := cYT.profile.normalized()
	if cYT.minterKey(req, p) == cGA.minterKey(req, p) {
		t.Fatal("minter keys for different endpoint modes must differ")
	}
}
