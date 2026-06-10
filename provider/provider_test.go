package provider_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/colespringer/waxseal/client"
	"github.com/colespringer/waxseal/provider"
	"github.com/colespringer/waxtap/potoken"
)

func newProvider(h http.HandlerFunc) (*provider.Provider, func()) {
	srv := httptest.NewServer(h)
	return provider.New(client.New(srv.URL)), srv.Close
}

func TestProvideScopeMapping(t *testing.T) {
	var gotBinding, gotScope string
	p, done := newProvider(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ContentBinding string `json:"content_binding"`
			Scope          string `json:"scope"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotBinding, gotScope = req.ContentBinding, req.Scope
		_ = json.NewEncoder(w).Encode(map[string]any{"poToken": "TOK-" + req.Scope})
	})
	defer done()
	ctx := context.Background()

	r, err := p.ProvidePOToken(ctx, potoken.Request{Scope: potoken.ScopeGVS, VisitorData: "VD"})
	if err != nil || r.Token != "TOK-gvs" || gotBinding != "VD" || gotScope != "gvs" {
		t.Fatalf("gvs: token=%q binding=%q scope=%q err=%v", r.Token, gotBinding, gotScope, err)
	}
	r, err = p.ProvidePOToken(ctx, potoken.Request{Scope: potoken.ScopePlayer, VideoID: "VID"})
	if err != nil || r.Token != "TOK-player" || gotBinding != "VID" || gotScope != "player" {
		t.Errorf("player: token=%q binding=%q scope=%q err=%v", r.Token, gotBinding, gotScope, err)
	}
}

func TestProvideNoneAndUnsupported(t *testing.T) {
	called := false
	p, done := newProvider(func(http.ResponseWriter, *http.Request) { called = true })
	defer done()
	ctx := context.Background()

	if r, err := p.ProvidePOToken(ctx, potoken.Request{Scope: potoken.ScopeNone}); err != nil || r.Token != "" {
		t.Errorf("none: token=%q err=%v", r.Token, err)
	}
	if called {
		t.Error("ScopeNone must not call the daemon")
	}
	if _, err := p.ProvidePOToken(ctx, potoken.Request{Scope: potoken.ScopeSubtitles, VideoID: "v"}); !errors.Is(err, provider.ErrUnsupportedScope) {
		t.Errorf("subtitles err = %v, want ErrUnsupportedScope", err)
	}
}

func TestSessionAdapts(t *testing.T) {
	p, done := newProvider(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"visitor_data": "VD",
			"cookies":      []map[string]any{{"name": "YSC", "value": "a", "secure": true, "http_only": true}},
		})
	})
	defer done()

	s, err := p.Session(context.Background())
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if s.VisitorData != "VD" || len(s.Cookies) != 1 || s.Cookies[0].Name != "YSC" {
		t.Fatalf("session = %+v", s)
	}
}
