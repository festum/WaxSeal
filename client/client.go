// Package client is a Go client for the WaxSeal HTTP PO-token daemon. Any
// application can use it to mint PO tokens (/get_pot) and adopt the coherent
// guest session (/session), independent of WaxTap. The WaxTap potoken.Provider
// adapter lives in waxseal/provider and is a thin wrapper over this client; other
// consumers can use this client directly or build their own adapter.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client talks to a WaxSeal daemon over HTTP.
type Client struct {
	baseURL string
	apiKey  string
	hc      *http.Client
}

// Token is a minted PO token and its expiry.
type Token struct {
	Value     string
	ExpiresAt time.Time // zero == unknown
}

// Session is WaxSeal's coherent guest identity: the visitor_data and youtube.com
// cookies a consumer adopts so attestation, token binding, and the download are
// one browser-as-origin session.
type Session struct {
	VisitorData   string
	UserAgent     string
	ClientVersion string
	Cookies       []*http.Cookie
}

// Option configures a Client.
type Option func(*Client)

// WithAPIKey sends X-API-Key on every request (for a multi-tenant WaxSeal).
func WithAPIKey(key string) Option { return func(c *Client) { c.apiKey = key } }

// WithHTTPClient overrides the default HTTP client.
func WithHTTPClient(hc *http.Client) Option { return func(c *Client) { c.hc = hc } }

// New returns a client for the WaxSeal daemon at baseURL
// (e.g. "http://127.0.0.1:4416").
func New(baseURL string, opts ...Option) *Client {
	c := &Client{baseURL: strings.TrimRight(baseURL, "/"), hc: http.DefaultClient}
	for _, o := range opts {
		o(c)
	}
	return c
}

// POToken mints a token bound to contentBinding: a video_id for a player token, or
// a visitor_data for a GVS token. scope is an optional discriminator ("player",
// "gvs", ...); "" lets the daemon use a generic key.
func (c *Client) POToken(ctx context.Context, contentBinding, scope string) (Token, error) {
	if contentBinding == "" {
		return Token{}, errors.New("waxseal/client: content_binding is required")
	}
	payload := map[string]string{"content_binding": contentBinding}
	if scope != "" {
		payload["scope"] = scope
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/get_pot", bytes.NewReader(body))
	if err != nil {
		return Token{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.auth(req)
	resp, err := c.hc.Do(req)
	if err != nil {
		return Token{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Token{}, c.statusErr("/get_pot", resp)
	}
	var out struct {
		POToken   string    `json:"poToken"`
		ExpiresAt time.Time `json:"expiresAt"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Token{}, fmt.Errorf("waxseal/client: decode /get_pot: %w", err)
	}
	if out.POToken == "" {
		return Token{}, errors.New("waxseal/client: /get_pot returned an empty poToken")
	}
	return Token{Value: out.POToken, ExpiresAt: out.ExpiresAt}, nil
}

// Session fetches the coherent {visitor_data, cookies} handoff.
func (c *Client) Session(ctx context.Context) (*Session, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/session", nil)
	if err != nil {
		return nil, err
	}
	c.auth(req)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, c.statusErr("/session", resp)
	}
	var out struct {
		VisitorData   string `json:"visitor_data"`
		UserAgent     string `json:"user_agent"`
		ClientVersion string `json:"client_version"`
		Cookies       []struct {
			Name     string `json:"name"`
			Value    string `json:"value"`
			Domain   string `json:"domain"`
			Path     string `json:"path"`
			Secure   bool   `json:"secure"`
			HTTPOnly bool   `json:"http_only"`
		} `json:"cookies"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("waxseal/client: decode /session: %w", err)
	}
	cookies := make([]*http.Cookie, 0, len(out.Cookies))
	for _, ck := range out.Cookies {
		cookies = append(cookies, &http.Cookie{
			Name: ck.Name, Value: ck.Value, Domain: ck.Domain, Path: ck.Path,
			Secure: ck.Secure, HttpOnly: ck.HTTPOnly,
		})
	}
	return &Session{VisitorData: out.VisitorData, UserAgent: out.UserAgent, ClientVersion: out.ClientVersion, Cookies: cookies}, nil
}

func (c *Client) auth(req *http.Request) {
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}
}

func (c *Client) statusErr(path string, resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
	return fmt.Errorf("waxseal/client: %s %s: %s", path, resp.Status, bytes.TrimSpace(b))
}
