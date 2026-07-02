package provider_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/colespringer/waxseal/client"
	"github.com/colespringer/waxseal/provider"
	"github.com/colespringer/waxtap/v2/potoken"
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

// TestProvideLogsDaemonWarning checks that a non-empty warning from /get_pot
// reaches a WaxTap-mediated caller through the provider's logger, that no warning
// field logs nothing, and that the default (no WithLogger) discards safely.
func TestProvideLogsDaemonWarning(t *testing.T) {
	logged := func(body map[string]any) (p *provider.Provider, buf *bytes.Buffer, done func()) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(body)
		}))
		buf = &bytes.Buffer{}
		p = provider.New(client.New(srv.URL), provider.WithLogger(slog.New(slog.NewTextHandler(buf, nil))))
		return p, buf, srv.Close
	}

	t.Run("warning surfaces through the logger", func(t *testing.T) {
		p, buf, done := logged(map[string]any{"poToken": "TOK", "warning": "content_binding looks like a URL"})
		defer done()
		if _, err := p.ProvidePOToken(context.Background(), potoken.Request{Scope: potoken.ScopePlayer, VideoID: "https://youtube.com/watch?v=x"}); err != nil {
			t.Fatalf("ProvidePOToken: %v", err)
		}
		if !strings.Contains(buf.String(), "content_binding looks like a URL") {
			t.Errorf("log = %q, want the daemon warning surfaced", buf.String())
		}
	})

	t.Run("no warning logs nothing", func(t *testing.T) {
		p, buf, done := logged(map[string]any{"poToken": "TOK"})
		defer done()
		if _, err := p.ProvidePOToken(context.Background(), potoken.Request{Scope: potoken.ScopeGVS, VisitorData: "VD"}); err != nil {
			t.Fatalf("ProvidePOToken: %v", err)
		}
		if buf.Len() != 0 {
			t.Errorf("log = %q, want silence when the daemon returns no warning", buf.String())
		}
	})

	t.Run("default logger discards a warning without panicking", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"poToken": "TOK", "warning": "content_binding looks like a URL"})
		}))
		defer srv.Close()
		p := provider.New(client.New(srv.URL)) // no WithLogger
		r, err := p.ProvidePOToken(context.Background(), potoken.Request{Scope: potoken.ScopePlayer, VideoID: "v"})
		if err != nil || r.Token != "TOK" {
			t.Fatalf("token=%q err=%v, want TOK with the nil-logger default", r.Token, err)
		}
	})
}

func TestProvidePlayerContextMapping(t *testing.T) {
	var gotVideoID string
	p, done := newProvider(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			VideoID string `json:"video_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotVideoID = req.VideoID
		_ = json.NewEncoder(w).Encode(map[string]any{
			"playability_status":              "OK",
			"player_url":                      "https://www.youtube.com/s/player/abc/base.js",
			"server_abr_streaming_url":        "https://r1.googlevideo.com/videoplayback?n=scram",
			"video_playback_ustreamer_config": "USTREAMER",
			"visitor_data":                    "VD",
			"client_version":                  "2.0",
			"title":                           "Big Buck Bunny",
			"author":                          "Blender",
			"length_seconds":                  634,
			"audio_formats": []map[string]any{{
				"itag": 251, "lmt": "171", "xtags": "X", "mime_type": "audio/webm", "bitrate": 130000,
				"content_length": 1234, "approx_duration_ms": 634000, "audio_sample_rate": 48000,
				"audio_channels": 2, "audio_quality": "AUDIO_QUALITY_MEDIUM",
				"is_drc": true, "audio_track_id": "en.4",
			}},
		})
	})
	defer done()

	pc, err := p.ProvidePlayerContext(context.Background(), "VID")
	if err != nil {
		t.Fatalf("ProvidePlayerContext: %v", err)
	}
	if gotVideoID != "VID" {
		t.Errorf("video_id = %q, want VID", gotVideoID)
	}
	if pc.ServerAbrURL != "https://r1.googlevideo.com/videoplayback?n=scram" || pc.PlayerURL == "" ||
		pc.UstreamerConfig != "USTREAMER" || pc.VisitorData != "VD" || pc.ClientVersion != "2.0" {
		t.Fatalf("context = %+v", pc)
	}
	if pc.Title != "Big Buck Bunny" || pc.Author != "Blender" || pc.LengthSeconds != 634 {
		t.Errorf("metadata: title=%q author=%q len=%d", pc.Title, pc.Author, pc.LengthSeconds)
	}
	if len(pc.AudioFormats) != 1 {
		t.Fatalf("audio formats = %d, want 1", len(pc.AudioFormats))
	}
	f := pc.AudioFormats[0]
	if f.Itag != 251 || f.LMT != "171" || f.XTags != "X" || f.MimeType != "audio/webm" || f.Bitrate != 130000 {
		t.Errorf("format core = %+v", f)
	}
	if f.ContentLength != 1234 || f.ApproxDurationMs != 634000 || f.AudioSampleRate != 48000 ||
		f.AudioChannels != 2 || f.AudioQuality != "AUDIO_QUALITY_MEDIUM" {
		t.Errorf("format detail = %+v", f)
	}
	// These fields are required by SABR setup and must survive both mappings.
	if !f.IsDrc || f.AudioTrackID != "en.4" {
		t.Errorf("DRC/track fields dropped: is_drc=%v audio_track_id=%q", f.IsDrc, f.AudioTrackID)
	}
}

// TestProvidePlayerContextRejects starts from an otherwise-complete context so each
// mutation is the sole reason the provider's stricter SABR validation rejects it.
func TestProvidePlayerContextRejects(t *testing.T) {
	full := func() map[string]any {
		return map[string]any{
			"playability_status":              "OK",
			"player_url":                      "https://www.youtube.com/s/player/abc/base.js",
			"server_abr_streaming_url":        "https://r1.googlevideo.com/videoplayback?n=s",
			"video_playback_ustreamer_config": "U",
			"visitor_data":                    "VD",
			"audio_formats":                   []map[string]any{{"itag": 251}},
		}
	}
	tests := []struct {
		name   string
		mutate func(m map[string]any)
	}{
		{"non-ok status", func(m map[string]any) { m["playability_status"] = "LOGIN_REQUIRED" }},
		{"missing player_url", func(m map[string]any) { delete(m, "player_url") }},
		{"missing visitor_data", func(m map[string]any) { delete(m, "visitor_data") }},
		{"missing ustreamer config", func(m map[string]any) { delete(m, "video_playback_ustreamer_config") }},
		{"no audio formats", func(m map[string]any) { m["audio_formats"] = []map[string]any{} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := full()
			tt.mutate(body)
			p, done := newProvider(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(body)
			})
			defer done()
			if _, err := p.ProvidePlayerContext(context.Background(), "VID"); err == nil {
				t.Fatalf("expected rejection for %q, got nil", tt.name)
			}
		})
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
