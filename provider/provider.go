// Package provider adapts a WaxSeal client (waxseal/client) to WaxTap's
// potoken.Provider, so an application embedding the WaxTap library mints PO
// tokens from a WaxSeal daemon.
//
// This is the only WaxTap-coupled piece, kept in a separate Go module so the
// WaxSeal core/server/CLI stay WaxTap-free. The HTTP work is generic and lives in
// waxseal/client; any application can use that client directly, or write its own
// adapter for a different PO-token contract. This package is just the scope
// mapping for WaxTap's interface.
package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/colespringer/waxseal/client"
	"github.com/colespringer/waxtap/potoken"
)

// ErrUnsupportedScope is returned for scopes WaxSeal does not serve (currently
// only ScopeSubtitles). Typed so callers can branch on it.
var ErrUnsupportedScope = errors.New("waxseal/provider: unsupported PO-token scope")

// Provider adapts a *client.Client to potoken.Provider.
type Provider struct {
	c *client.Client
}

var (
	_ potoken.Provider              = (*Provider)(nil)
	_ potoken.PlayerContextProvider = (*Provider)(nil)
)

// New wraps a WaxSeal client as a WaxTap potoken.Provider. Configure auth/HTTP on
// the client (client.WithAPIKey, client.WithHTTPClient).
func New(c *client.Client) *Provider { return &Provider{c: c} }

// ProvidePOToken maps the WaxTap scope to a content_binding and mints via the
// client. ScopeGVS binds visitor_data, ScopePlayer binds video_id; ScopeNone is a
// no-op; ScopeSubtitles returns ErrUnsupportedScope.
func (p *Provider) ProvidePOToken(ctx context.Context, req potoken.Request) (potoken.Response, error) {
	var binding, scope string
	switch req.Scope {
	case potoken.ScopeNone:
		return potoken.Response{}, nil
	case potoken.ScopeGVS:
		binding, scope = req.VisitorData, "gvs"
	case potoken.ScopePlayer:
		binding, scope = req.VideoID, "player"
	default: // ScopeSubtitles or unknown
		return potoken.Response{}, fmt.Errorf("%w: %s", ErrUnsupportedScope, req.Scope)
	}
	tok, err := p.c.POToken(ctx, binding, scope)
	if err != nil {
		return potoken.Response{}, err
	}
	return potoken.Response{Token: tok.Value, ExpiresAt: tok.ExpiresAt}, nil
}

// Session fetches WaxSeal's coherent guest session as a *potoken.Session, ready
// for WaxTap's Options.Session.
func (p *Provider) Session(ctx context.Context) (*potoken.Session, error) {
	s, err := p.c.Session(ctx)
	if err != nil {
		return nil, err
	}
	return &potoken.Session{VisitorData: s.VisitorData, Cookies: s.Cookies}, nil
}

// ProvidePlayerContext fetches videoID's attested WEB /player streaming context
// and maps it to the context used by WaxTap's optional WEB SABR audio path. It
// rejects incomplete contexts before SABR setup so WaxTap can use its normal
// fallback path.
func (p *Provider) ProvidePlayerContext(ctx context.Context, videoID string) (potoken.PlayerContext, error) {
	pc, err := p.c.PlayerContext(ctx, videoID)
	if err != nil {
		return potoken.PlayerContext{}, err
	}
	if pc.Status != "" && !strings.EqualFold(pc.Status, "OK") {
		return potoken.PlayerContext{}, fmt.Errorf("waxseal/provider: player-context returned status %q", pc.Status)
	}
	if pc.ServerAbrStreamingURL == "" || pc.PlayerURL == "" || pc.VisitorData == "" || pc.VideoPlaybackUstreamerConfig == "" || len(pc.AudioFormats) == 0 {
		return potoken.PlayerContext{}, fmt.Errorf("waxseal/provider: player-context missing server_abr_streaming_url, player_url, visitor_data, video_playback_ustreamer_config, or audio_formats")
	}

	formats := make([]potoken.PlayerContextFormat, 0, len(pc.AudioFormats))
	for _, f := range pc.AudioFormats {
		formats = append(formats, potoken.PlayerContextFormat{
			Itag:             f.Itag,
			LMT:              f.LMT,
			XTags:            f.XTags,
			MimeType:         f.MimeType,
			Bitrate:          f.Bitrate,
			AudioQuality:     f.AudioQuality,
			AudioChannels:    f.AudioChannels,
			AudioSampleRate:  f.AudioSampleRate,
			ContentLength:    f.ContentLength,
			ApproxDurationMs: int64(f.ApproxDurationMs),
			IsDrc:            f.IsDrc,
			AudioTrackID:     f.AudioTrackID,
		})
	}
	return potoken.PlayerContext{
		ServerAbrURL:    pc.ServerAbrStreamingURL,
		PlayerURL:       pc.PlayerURL,
		UstreamerConfig: pc.VideoPlaybackUstreamerConfig,
		VisitorData:     pc.VisitorData,
		ClientVersion:   pc.ClientVersion,
		Title:           pc.Title,
		Author:          pc.Author,
		LengthSeconds:   pc.LengthSeconds,
		AudioFormats:    formats,
	}, nil
}
