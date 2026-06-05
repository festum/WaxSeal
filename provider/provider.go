// Package provider adapts a *waxseal.Client to WaxTap's potoken.Provider so
// WaxTap can mint PO tokens on demand, including re-minting after a 403 hint.
// It is a separate Go module so the WaxSeal core, server, and CLI stay free of
// the WaxTap dependency. Only code that wires the two together imports this
// package and, transitively, WaxTap.
//
// It maps ScopeGVS (googlevideo stream URLs) to a WaxSeal session token bound to
// the WaxTap-supplied visitor_data, and ScopePlayer (the /player request body,
// since WaxTap v1.1.0 injects serviceIntegrityDimensions.poToken) to a content
// token bound to the video_id. ScopeSubtitles returns a typed
// ErrUnsupportedScope because WaxSeal does not currently serve subtitle tokens.
package provider

import (
	"context"
	"errors"
	"fmt"

	waxseal "github.com/colespringer/waxseal"
	"github.com/colespringer/waxtap/potoken"
)

// ErrUnsupportedScope is returned for scopes WaxSeal cannot serve through the
// WaxTap stream resolver (everything but ScopeGVS today). It is typed so callers
// can branch on it rather than treating it as a mint failure.
var ErrUnsupportedScope = errors.New("waxseal/provider: unsupported PO-token scope")

// tokenProvider is the slice of *waxseal.Client this adapter needs, named so the
// scope mapping is unit-testable without standing up the real VM.
type tokenProvider interface {
	Token(ctx context.Context, req waxseal.Request) (waxseal.Token, error)
}

// Provider implements potoken.Provider over a WaxSeal client.
type Provider struct {
	client tokenProvider
}

var _ potoken.Provider = (*Provider)(nil)

// New returns a potoken.Provider backed by c. WaxBin hands the same *http.Client
// (egress and cookie jar) to WaxTap and to the waxseal.Client behind c, so
// tokens mint from the identity used to download.
func New(c *waxseal.Client) *Provider {
	return &Provider{client: c}
}

// ProvidePOToken mints (or serves from cache) a token for req, minted with the
// exact UA WaxTap will send for this scope (req.UserAgent); a 403 hint
// (req.Failure) forces a cache-bypassing re-mint. ScopeGVS binds to the
// authoritative req.VisitorData (stream URL); ScopePlayer binds to req.VideoID
// (/player body). ScopeNone is a no-op; ScopeSubtitles returns ErrUnsupportedScope.
func (p *Provider) ProvidePOToken(ctx context.Context, req potoken.Request) (potoken.Response, error) {
	wreq := waxseal.Request{
		ClientName:    req.ClientName,
		ClientVersion: req.ClientVersion,
		UserAgent:     req.UserAgent, // profile and GVS/player UA from one value
		BypassCache:   req.Failure != nil,
	}
	switch req.Scope {
	case potoken.ScopeNone:
		return potoken.Response{}, nil
	case potoken.ScopeGVS:
		// GVS stream URLs bind to the session (visitor_data).
		wreq.Scope = waxseal.ScopeSession
		wreq.VisitorData = req.VisitorData
	case potoken.ScopePlayer:
		// The /player request POT binds to the content (video_id). The exact
		// player binding should be rechecked against the integrity minter; the
		// fallback token is binding-agnostic, so current tests do not depend on
		// this choice.
		wreq.Scope = waxseal.ScopeContent
		wreq.VideoID = req.VideoID
	default: // ScopeSubtitles or unknown
		return potoken.Response{}, fmt.Errorf("%w: %s", ErrUnsupportedScope, req.Scope)
	}

	tok, err := p.client.Token(ctx, wreq)
	if err != nil {
		return potoken.Response{}, err
	}
	return potoken.Response{
		Token:     tok.Value,
		Headers:   tok.Headers,
		Query:     tok.Query,
		ExpiresAt: tok.ExpiresAt,
	}, nil
}
