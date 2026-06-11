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

func TestPlayerContext(t *testing.T) {
	var gotVideoID, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/player-context" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		gotKey = r.Header.Get("X-API-Key")
		var req struct {
			VideoID string `json:"video_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotVideoID = req.VideoID
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":                   "OK",
			"player_url":               "https://www.youtube.com/s/player/abc/base.js",
			"server_abr_streaming_url": "https://r1.googlevideo.com/videoplayback?n=scram",
			"visitor_data":             "VD",
			"client_version":           "2.0",
			"audio_formats": []map[string]any{
				{"itag": 251, "lmt": "1719185012384481", "mime_type": "audio/webm", "bitrate": 130000, "content_length": 1234, "audio_quality": "AUDIO_QUALITY_MEDIUM"},
			},
		})
	}))
	defer srv.Close()

	c := client.New(srv.URL, client.WithAPIKey("K"))
	pc, err := c.PlayerContext(context.Background(), "VID")
	if err != nil {
		t.Fatalf("PlayerContext: %v", err)
	}
	if gotVideoID != "VID" || gotKey != "K" {
		t.Errorf("video_id=%q key=%q", gotVideoID, gotKey)
	}
	if pc.ServerAbrStreamingURL != "https://r1.googlevideo.com/videoplayback?n=scram" || pc.PlayerURL == "" {
		t.Errorf("context = %+v", pc)
	}
	if len(pc.AudioFormats) != 1 || pc.AudioFormats[0].Itag != 251 || pc.AudioFormats[0].MimeType != "audio/webm" {
		t.Errorf("audio formats = %+v", pc.AudioFormats)
	}
}

func TestPlayerContextEmptyVideoIDAndHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer srv.Close()
	c := client.New(srv.URL)
	if _, err := c.PlayerContext(context.Background(), ""); err == nil {
		t.Error("empty video_id should error before any HTTP call")
	}
	if _, err := c.PlayerContext(context.Background(), "VID"); err == nil {
		t.Error("a non-200 from /player-context should error")
	}
}
