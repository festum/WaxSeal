package provider

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"

	waxseal "github.com/colespringer/waxseal"
	"github.com/colespringer/waxtap/potoken"
)

type fakeClient struct {
	called bool
	gotReq waxseal.Request
	tok    waxseal.Token
	err    error
}

func (f *fakeClient) Token(ctx context.Context, req waxseal.Request) (waxseal.Token, error) {
	f.called = true
	f.gotReq = req
	return f.tok, f.err
}

func TestGVSMapsToSessionToken(t *testing.T) {
	exp := time.Now().Add(time.Hour)
	fc := &fakeClient{tok: waxseal.Token{Value: "POTOKEN", ExpiresAt: exp}}
	p := &Provider{client: fc}

	resp, err := p.ProvidePOToken(context.Background(), potoken.Request{
		Scope:         potoken.ScopeGVS,
		VisitorData:   "VISITOR_DATA",
		ClientName:    "WEB",
		ClientVersion: "2.x",
		UserAgent:     "Mozilla/5.0 ... Chrome/131 ...",
	})
	if err != nil {
		t.Fatalf("ProvidePOToken: %v", err)
	}
	if resp.Token != "POTOKEN" || !resp.ExpiresAt.Equal(exp) {
		t.Fatalf("response = %+v", resp)
	}
	if !fc.called {
		t.Fatal("client.Token was not called")
	}
	got := fc.gotReq
	if got.Scope != waxseal.ScopeSession {
		t.Fatalf("scope = %v, want ScopeSession", got.Scope)
	}
	if got.VisitorData != "VISITOR_DATA" {
		t.Fatalf("visitorData = %q", got.VisitorData)
	}
	if got.ClientName != "WEB" || got.ClientVersion != "2.x" {
		t.Fatalf("client name/version not passed through: %+v", got)
	}
	if got.UserAgent != "Mozilla/5.0 ... Chrome/131 ..." {
		t.Fatalf("userAgent = %q", got.UserAgent)
	}
	if got.BypassCache {
		t.Fatal("BypassCache should be false without a Failure")
	}
}

func TestFailureSetsBypassCache(t *testing.T) {
	fc := &fakeClient{tok: waxseal.Token{Value: "T"}}
	p := &Provider{client: fc}

	_, err := p.ProvidePOToken(context.Background(), potoken.Request{
		Scope:       potoken.ScopeGVS,
		VisitorData: "VD",
		Failure:     &potoken.HTTPFailure{StatusCode: 403, Status: "403 Forbidden"},
	})
	if err != nil {
		t.Fatalf("ProvidePOToken: %v", err)
	}
	if !fc.gotReq.BypassCache {
		t.Fatal("a 403 Failure must set BypassCache=true (re-mint)")
	}
}

func TestScopeNoneIsNoop(t *testing.T) {
	fc := &fakeClient{}
	p := &Provider{client: fc}

	resp, err := p.ProvidePOToken(context.Background(), potoken.Request{Scope: potoken.ScopeNone})
	if err != nil {
		t.Fatalf("ScopeNone: %v", err)
	}
	if resp.Token != "" {
		t.Fatalf("ScopeNone should yield an empty response, got %+v", resp)
	}
	if fc.called {
		t.Fatal("ScopeNone must not call the client")
	}
}

func TestPlayerMapsToContentToken(t *testing.T) {
	fc := &fakeClient{tok: waxseal.Token{Value: "PLAYER_POT"}}
	p := &Provider{client: fc}

	resp, err := p.ProvidePOToken(context.Background(), potoken.Request{
		Scope:       potoken.ScopePlayer,
		VideoID:     "VID123",
		VisitorData: "VD",
		UserAgent:   "Mozilla/5.0 ... Chrome/131 ...",
	})
	if err != nil {
		t.Fatalf("ProvidePOToken: %v", err)
	}
	if resp.Token != "PLAYER_POT" {
		t.Fatalf("token = %q", resp.Token)
	}
	got := fc.gotReq
	if got.Scope != waxseal.ScopeContent {
		t.Fatalf("scope = %v, want ScopeContent (video-bound player token)", got.Scope)
	}
	if got.VideoID != "VID123" {
		t.Fatalf("videoID = %q", got.VideoID)
	}
	if got.UserAgent != "Mozilla/5.0 ... Chrome/131 ..." {
		t.Fatalf("userAgent = %q", got.UserAgent)
	}
}

func TestUnsupportedScopes(t *testing.T) {
	for _, scope := range []potoken.Scope{potoken.ScopeSubtitles} {
		fc := &fakeClient{}
		p := &Provider{client: fc}
		_, err := p.ProvidePOToken(context.Background(), potoken.Request{Scope: scope, VideoID: "v"})
		if !errors.Is(err, ErrUnsupportedScope) {
			t.Fatalf("scope %v: want ErrUnsupportedScope, got %v", scope, err)
		}
		if fc.called {
			t.Fatalf("scope %v: client must not be called", scope)
		}
	}
}

func TestClientErrorPropagates(t *testing.T) {
	fc := &fakeClient{err: waxseal.ErrUnsupportedClient}
	p := &Provider{client: fc}

	_, err := p.ProvidePOToken(context.Background(), potoken.Request{
		Scope:       potoken.ScopeGVS,
		VisitorData: "VD",
		UserAgent:   "curl/8",
	})
	if !errors.Is(err, waxseal.ErrUnsupportedClient) {
		t.Fatalf("want ErrUnsupportedClient propagated, got %v", err)
	}
}

func TestResponsePreservesHeadersAndQuery(t *testing.T) {
	hdr := http.Header{"X-Test": {"1"}}
	q := url.Values{"pot": {"abc"}}
	fc := &fakeClient{tok: waxseal.Token{Value: "T", Headers: hdr, Query: q}}
	p := &Provider{client: fc}

	resp, err := p.ProvidePOToken(context.Background(), potoken.Request{Scope: potoken.ScopeGVS, VisitorData: "VD"})
	if err != nil {
		t.Fatalf("ProvidePOToken: %v", err)
	}
	if resp.Headers.Get("X-Test") != "1" || resp.Query.Get("pot") != "abc" {
		t.Fatalf("headers/query not preserved: %+v", resp)
	}
}
