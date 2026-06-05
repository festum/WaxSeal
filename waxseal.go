// Package waxseal is a native-Go YouTube PO Token (POT) provider. It runs the
// BotGuard VM inside QuickJS-on-wazero (pure Go, no CGo) and keeps networking,
// descrambling, protobuf handling, caching, and lifecycle control in Go.
//
// The core Client is general: it defines its own Scope so non-WaxTap consumers
// need not import WaxTap. The optional provider/ submodule adapts it to WaxTap's
// potoken.Provider.
package waxseal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"log/slog"

	"github.com/colespringer/waxseal/internal/botguard"
	"github.com/colespringer/waxseal/internal/cache"
	"github.com/colespringer/waxseal/internal/httpx"
	"github.com/colespringer/waxseal/internal/innertube"
	"github.com/colespringer/waxseal/internal/jsassets"
	"github.com/colespringer/waxseal/internal/jsruntime"
	"github.com/colespringer/waxseal/internal/jsruntime/quickjs"
	"github.com/colespringer/waxseal/internal/session"
)

// Scope is the kind of token to mint. The core defines its own Scope so callers
// that are not WaxTap don't transitively import it.
type Scope int

const (
	ScopeNone    Scope = iota // no-op (no token)
	ScopeSession              // bind visitor_data (session / GVS)
	ScopeContent              // bind video_id (content / player)
	ScopeOpaque               // mint Identifier as-is (server/CLI)
)

// endpointModeDefault is youtube.com/api/jnn/v1. It is part of every key.
const endpointModeDefault = "youtube"

// ErrMissingIdentifier is returned when a request resolves to no mint
// identifier (e.g. ScopeSession with an empty VisitorData).
var ErrMissingIdentifier = errors.New("waxseal: no mint identifier for request")

// ErrClosed is returned by a Client used after Close.
var ErrClosed = errors.New("waxseal: client closed")

// EgressSpec selects the network path and yields a stable key. In-process
// callers usually leave Proxy/SourceAddress empty and set ID to label the
// Options.HTTPClient path. Server and CLI callers can use
// Proxy/SourceAddress/DisableTLSVerify when building per-spec transports.
type EgressSpec struct {
	ID               string
	Proxy            string
	SourceAddress    string
	DisableTLSVerify bool
}

// Request is one token request. Identifier overrides the scope-derived value;
// otherwise the identifier is VisitorData (session) or VideoID (content).
type Request struct {
	Scope         Scope
	Identifier    string // explicit override / ScopeOpaque; otherwise derived
	VisitorData   string // authoritative when supplied (WaxTap bootstraps it)
	VideoID       string
	ClientName    string // e.g. "WEB"; part of cache/minter keys and telemetry
	ClientVersion string
	UserAgent     string // selects BrowserProfile by UA; else Options.Profile
	BypassCache   bool   // set true on a Failure/403 hint
	Egress        EgressSpec

	// Challenge source controls for server and standalone use. Challenge accepts
	// raw JSON in any supported challenge shape; nil uses the InnerTube/Create
	// chain. InnertubeContext overrides Options.InnertubeContext for att/get.
	// DisableInnertube overrides Options.DisableInnertube when non-nil.
	Challenge        json.RawMessage
	InnertubeContext json.RawMessage
	DisableInnertube *bool
}

// Token is a minted PO token with its authoritative expiry. Headers/Query are
// usually nil; they let an adapter satisfy future stream-header/query needs.
type Token struct {
	Value     string
	Headers   http.Header
	Query     url.Values
	ExpiresAt time.Time
}

// Options configure a Client.
type Options struct {
	// HTTPClient is the in-process network path. Reuse WaxTap's client
	// (jar/proxy/IP) so tokens mint from the same identity used to download.
	// It needs a cookie jar because Create and GenerateIT share cookies. nil
	// creates a default client with a jar.
	HTTPClient *http.Client

	// EgressTransport lets WaxSeal build transports for per-request egress
	// changes. The server and CLI use BuildEgressTransport. When set, Client
	// caches an *httpx.Client with its own jar per EgressSpec. When nil, all
	// requests use HTTPClient and the proxy/source/TLS fields on EgressSpec are
	// ignored.
	EgressTransport func(EgressSpec) (http.RoundTripper, error)

	Profile  BrowserProfile   // default browser identity (Chrome-on-Windows, close to WaxTap WEB)
	Profiles []BrowserProfile // known profiles a Request.UserAgent resolves to

	Logger *slog.Logger

	// Challenge sourcing defaults (per-request Request fields override these).
	DisableInnertube bool            // skip InnerTube att/get, go straight to Create
	InnertubeContext json.RawMessage // context sent to att/get; nil uses a default

	// Caching / lifecycle tuning.
	CacheMaxEntries     int           // token cache size (default 256)
	CacheMaxTTL         time.Duration // caps cached validity, never extends it (0 = uncapped)
	CacheContentTokens  bool          // cache ScopeContent tokens (default off; cheap to re-mint)
	DefaultTTL          time.Duration // used when GenerateIT omits a lifetime (default 1h)
	SnapshotConcurrency int           // bound concurrent ~910ms snapshots (default GOMAXPROCS/2)
	Discovery           bool          // keep the shim's API-DRIFT probe trap on (dev/doctor)
	CompilationCacheDir string        // wazero AOT cache dir (skip recompile on restart)
	Watchdog            time.Duration // per VM-call deadline (default 20s)

	now    func() time.Time // test hook
	engine jsruntime.Engine // test hook: inject a fake engine (skips quickjs)
}

// Client is the orchestration core: profile resolution + token cache + the warm
// minter session over the QuickJS-on-wazero engine.
type Client struct {
	opts     Options
	profile  BrowserProfile
	profiles []BrowserProfile
	http     *httpx.Client
	egress   *egressCache // per-egress clients when EgressTransport is set
	engine   jsruntime.Engine
	manager  *session.Manager
	cache    *cache.Memory[Token]
	logger   *slog.Logger

	closed bool
}

// New builds a Client, compiling the embedded wasm once (cached on disk if
// CompilationCacheDir is set).
func New(opts Options) (*Client, error) {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Profile.UserAgent == "" {
		opts.Profile = DefaultProfile()
	}
	if opts.HTTPClient == nil {
		jar, _ := cookiejar.New(nil)
		opts.HTTPClient = &http.Client{Timeout: 30 * time.Second, Jar: jar}
	}
	if opts.Watchdog <= 0 {
		opts.Watchdog = 20 * time.Second
	}
	if opts.now == nil {
		opts.now = time.Now
	}

	eng := opts.engine
	if eng == nil {
		qjs, err := quickjs.NewEngine(context.Background(), jsassets.QJSWasm, quickjs.Options{
			PreloadBundle:       jsassets.BGBundle,
			Watchdog:            opts.Watchdog,
			Stderr:              slogWriter{opts.Logger},
			CompilationCacheDir: opts.CompilationCacheDir,
		})
		if err != nil {
			return nil, err
		}
		eng = qjs
	}

	mgr := session.New(eng, session.Options{
		SnapshotConcurrency: opts.SnapshotConcurrency,
		DefaultTTL:          opts.DefaultTTL,
		MaxTTL:              opts.CacheMaxTTL,
		Discovery:           opts.Discovery,
		Logger:              opts.Logger,
	})

	return &Client{
		opts:     opts,
		profile:  opts.Profile,
		profiles: opts.Profiles,
		http:     httpx.New(opts.HTTPClient),
		egress:   newEgressCache(defaultEgressCacheSize, opts.HTTPClient.Timeout),
		engine:   eng,
		manager:  mgr,
		cache:    cache.New[Token](opts.CacheMaxEntries),
		logger:   opts.Logger,
	}, nil
}

// normalizeEgress derives ID from the egress-affecting fields when WaxSeal owns
// the transport. That keeps the transport, warm-minter, and token caches aligned
// even if the caller omits ID. In-process callers use ID only as a label, so
// those specs are left unchanged.
func (c *Client) normalizeEgress(spec EgressSpec) EgressSpec {
	if c.opts.EgressTransport != nil && spec.ID == "" {
		spec.ID = spec.DerivedID()
	}
	return spec
}

// clientFor returns the request's *httpx.Client. With EgressTransport set, it
// caches a transport and cookie jar per EgressSpec. Otherwise it returns the
// shared in-process client.
func (c *Client) clientFor(spec EgressSpec) (*httpx.Client, error) {
	if c.opts.EgressTransport == nil {
		return c.http, nil
	}
	hc, err := c.egress.getOrBuild(spec, c.opts.EgressTransport)
	if err != nil {
		return nil, err
	}
	hc.Logger = c.logger
	return hc, nil
}

// Token mints (or serves from cache) a PO token for req.
func (c *Client) Token(ctx context.Context, req Request) (Token, error) {
	if c.closed {
		return Token{}, ErrClosed
	}
	if req.Scope == ScopeNone {
		return Token{}, nil
	}
	req.Egress = c.normalizeEgress(req.Egress)

	profile, err := resolveProfile(req.UserAgent, c.profile, c.profiles)
	if err != nil {
		return Token{}, err
	}
	identifier, err := identifierFor(req)
	if err != nil {
		return Token{}, err
	}

	if !req.BypassCache {
		// Prefer an integrity token. Fallback tokens use separate cache keys
		// because their validity differs.
		for _, kind := range []session.TokenKind{session.KindIntegrity, session.KindFallback} {
			if tok, ok := c.cache.Get(c.cacheKey(req, profile, identifier, kind)); ok {
				return tok, nil
			}
		}
	}

	egressClient, err := c.clientFor(req.Egress)
	if err != nil {
		return Token{}, err
	}
	challenge, err := c.parseChallenge(req.Challenge)
	if err != nil {
		return Token{}, err
	}

	res, err := c.manager.Token(ctx, session.Request{
		Key:              c.minterKey(req, profile),
		Identifier:       identifier,
		ProfileJSON:      profile.shimJSON(),
		AttestationUA:    profile.AttestationUA,
		Client:           egressClient,
		ForceNew:         req.BypassCache,
		Challenge:        challenge,
		InnertubeContext: c.innertubeContext(req),
		DisableInnertube: c.disableInnertube(req),
	})
	if err != nil {
		return Token{}, err
	}

	tok := Token{Value: res.Token, ExpiresAt: res.ExpiresAt}
	if c.shouldCache(req.Scope, res.Kind) {
		c.cache.Set(c.cacheKey(req, profile, identifier, res.Kind), tok, res.ExpiresAt)
	}
	return tok, nil
}

// SessionToken is a convenience for a GVS/session token bound to visitorData
// using the default profile/egress. The full API is Token.
func (c *Client) SessionToken(ctx context.Context, visitorData string) (Token, error) {
	return c.Token(ctx, Request{Scope: ScopeSession, VisitorData: visitorData})
}

// ContentToken is a convenience for a content token bound to videoID using the
// default profile/egress. The full API is Token.
func (c *Client) ContentToken(ctx context.Context, videoID string) (Token, error) {
	return c.Token(ctx, Request{Scope: ScopeContent, VideoID: videoID})
}

// VisitorData fetches fresh guest visitor_data from InnerTube for standalone
// callers. The in-process WaxTap path supplies visitor_data from its own session.
func (c *Client) VisitorData(ctx context.Context, egress EgressSpec) (string, error) {
	if c.closed {
		return "", ErrClosed
	}
	egressClient, err := c.clientFor(egress)
	if err != nil {
		return "", err
	}
	return innertube.GenerateVisitorData(ctx, egressClient, c.profile.normalized().AttestationUA)
}

// Prewarm builds the warm minter for req in the background so the first request
// can skip the cold snapshot. It is best-effort.
func (c *Client) Prewarm(req Request) {
	if c.closed {
		return
	}
	profile, err := resolveProfile(req.UserAgent, c.profile, c.profiles)
	if err != nil {
		return
	}
	req.Egress = c.normalizeEgress(req.Egress)
	egressClient, err := c.clientFor(req.Egress)
	if err != nil {
		return
	}
	identifier, _ := identifierFor(req) // empty is fine for the fallback path
	c.manager.Prewarm(session.Request{
		Key:              c.minterKey(req, profile),
		Identifier:       identifier,
		ProfileJSON:      profile.shimJSON(),
		AttestationUA:    profile.AttestationUA,
		Client:           egressClient,
		InnertubeContext: c.innertubeContext(req),
		DisableInnertube: c.disableInnertube(req),
	})
}

// PurgeTokens empties the token cache (the server's invalidate_caches). Warm
// minters keep serving; only previously minted/cached tokens are dropped.
func (c *Client) PurgeTokens() {
	c.cache.Purge()
}

// InvalidateMinters evicts every warm minter session (the server's invalidate_it)
// so the next request re-attests with a fresh BotGuard snapshot.
func (c *Client) InvalidateMinters() {
	c.manager.InvalidateAll()
}

// MinterKeys lists the current warm-minter keys (the server's minter_cache).
func (c *Client) MinterKeys() []string {
	return c.manager.Keys()
}

// Close releases the warm runtimes and the shared engine.
func (c *Client) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	_ = c.manager.Close()
	c.egress.closeAll()
	return c.engine.Close(context.Background())
}

// parseChallenge converts a caller-provided raw challenge into the internal form.
// Empty bytes and JSON null both mean "absent", matching clients that serialize
// omitted optional fields as null.
func (c *Client) parseChallenge(raw json.RawMessage) (*botguard.Challenge, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	return botguard.ParseProvidedChallenge(raw)
}

// innertubeContext returns the per-request att/get context or the client default.
func (c *Client) innertubeContext(req Request) json.RawMessage {
	if len(req.InnertubeContext) > 0 {
		return req.InnertubeContext
	}
	return c.opts.InnertubeContext
}

// disableInnertube resolves the per-request InnerTube toggle over the default.
func (c *Client) disableInnertube(req Request) bool {
	if req.DisableInnertube != nil {
		return *req.DisableInnertube
	}
	return c.opts.DisableInnertube
}

func identifierFor(req Request) (string, error) {
	if req.Identifier != "" {
		return req.Identifier, nil
	}
	switch req.Scope {
	case ScopeSession:
		if req.VisitorData != "" {
			return req.VisitorData, nil
		}
	case ScopeContent:
		if req.VideoID != "" {
			return req.VideoID, nil
		}
	}
	return "", ErrMissingIdentifier
}

func (c *Client) shouldCache(scope Scope, _ session.TokenKind) bool {
	switch scope {
	case ScopeSession, ScopeOpaque:
		return true
	case ScopeContent:
		return c.opts.CacheContentTokens
	}
	return false
}

// minterKey is the warm-minter key: the token key minus identifier and kind.
func (c *Client) minterKey(req Request, p BrowserProfile) string {
	return strings.Join([]string{
		req.Egress.ID, endpointModeDefault, p.Hash(), req.ClientName, req.ClientVersion,
	}, "|")
}

// cacheKey is the full token key including the token kind.
func (c *Client) cacheKey(req Request, p BrowserProfile, identifier string, kind session.TokenKind) string {
	return strings.Join([]string{
		string(kind), scopeString(req.Scope), identifier,
		req.ClientName, req.ClientVersion, req.Egress.ID, endpointModeDefault, p.Hash(),
	}, "|")
}

func scopeString(s Scope) string {
	switch s {
	case ScopeSession:
		return "session"
	case ScopeContent:
		return "content"
	case ScopeOpaque:
		return "opaque"
	}
	return "none"
}

// slogWriter forwards VM console output (shim probes, timer errors) to the
// logger at debug level.
type slogWriter struct{ l *slog.Logger }

func (w slogWriter) Write(p []byte) (int, error) {
	w.l.Debug("vm", "line", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
