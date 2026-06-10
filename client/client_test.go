package client_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/colespringer/waxseal/client"
)

func TestPOToken(t *testing.T) {
	var gotBinding, gotScope, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/get_pot" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		gotKey = r.Header.Get("X-API-Key")
		var req struct {
			ContentBinding string `json:"content_binding"`
			Scope          string `json:"scope"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotBinding, gotScope = req.ContentBinding, req.Scope
		_ = json.NewEncoder(w).Encode(map[string]any{"poToken": "TOK", "expiresAt": "2030-01-01T00:00:00Z"})
	}))
	defer srv.Close()

	c := client.New(srv.URL, client.WithAPIKey("K"))
	tok, err := c.POToken(context.Background(), "VID", "player")
	if err != nil || tok.Value != "TOK" {
		t.Fatalf("token = %+v, err = %v", tok, err)
	}
	if gotBinding != "VID" || gotScope != "player" || gotKey != "K" {
		t.Errorf("binding=%q scope=%q key=%q", gotBinding, gotScope, gotKey)
	}
	if tok.ExpiresAt.IsZero() {
		t.Error("expiresAt not parsed")
	}
}

func TestPOTokenEmptyBindingAndHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream boom", http.StatusBadGateway)
	}))
	defer srv.Close()
	c := client.New(srv.URL)
	if _, err := c.POToken(context.Background(), "", "gvs"); err == nil {
		t.Error("empty content_binding should error before any HTTP call")
	}
	if _, err := c.POToken(context.Background(), "VD", "gvs"); err == nil {
		t.Error("a non-200 from /get_pot should error")
	}
}

func TestSession(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"visitor_data":   "VD",
			"user_agent":     "UA",
			"client_version": "CV",
			"cookies": []map[string]any{
				{"name": "YSC", "value": "abc", "domain": ".youtube.com", "path": "/", "secure": true, "http_only": true},
			},
		})
	}))
	defer srv.Close()

	s, err := client.New(srv.URL).Session(context.Background())
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if s.VisitorData != "VD" || s.UserAgent != "UA" || s.ClientVersion != "CV" || len(s.Cookies) != 1 {
		t.Fatalf("session = %+v", s)
	}
	if ck := s.Cookies[0]; ck.Name != "YSC" || ck.Value != "abc" || !ck.Secure || !ck.HttpOnly {
		t.Errorf("cookie = %+v", ck)
	}
}
