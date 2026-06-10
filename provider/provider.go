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

var _ potoken.Provider = (*Provider)(nil)

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
