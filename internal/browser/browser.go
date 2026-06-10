package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/colespringer/waxseal/internal/botguard"
	"github.com/colespringer/waxseal/internal/httpx"
	"github.com/colespringer/waxseal/internal/innertube"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

// DefaultVideo is the landing video the browser parks on to capture the page
// identity (visitor_data, client version, signatureTimestamp). It is Blender's
// "Big Buck Bunny", a Creative Commons open movie. Keep every video id in this
// codebase non-copyrighted.
const DefaultVideo = "aqz-KE-bpKQ"

// Options configure a browser Session. The zero value is usable: it auto-detects
// a system Chromium, runs headless=new, and discards logs.
type Options struct {
	ChromeBin   string        // explicit Chromium binary; "" auto-detects (WAXSEAL_CHROME_BIN, then well-known paths)
	Headful     bool          // run headful (needs a display/Xvfb); default is headless=new
	NormalizeUA bool          // headless only: rewrite navigator.userAgent HeadlessChrome->Chrome so the UA is not a headless tell (UA-CH already matches a real Chromium)
	Logger      *slog.Logger  // nil discards
	NavTimeout  time.Duration // watch-page navigation budget (default 45s)
}

// Identity is the captured real-browser session: the page's visitor_data, client
// version, user agent, and signatureTimestamp. A consumer adopts it so its
// downloads share the browser's origin.
type Identity struct {
	WatchURL      string `json:"watch_url"`
	VisitorData   string `json:"visitor_data"`
	ClientVersion string `json:"client_version"`
	APIKey        string `json:"api_key,omitempty"`
	UserAgent     string `json:"user_agent"`
	Webdriver     bool   `json:"navigator_webdriver"` // must be false; true means an automation artifact leaked
	Cookies       int    `json:"cookie_count"`
	STS           int    `json:"signature_timestamp"` // from base.js; required or /player returns UNPLAYABLE
}

// Session is one launched Chromium plus a page parked on a youtube.com watch page,
// with the bundle injected and a Go HTTP client seeded from the page's cookies, so
// att/get and GenerateIT egress with the same cookies and IP as the browser.
type Session struct {
	browser *rod.Browser
	page    *rod.Page
	dispose func() // tears down what this Session owns: the whole browser (Launch) or just its context (Pool)
	id      Identity
	client  *httpx.Client // egresses with the browser's cookies
	log     *slog.Logger

	// One attestation installs a warm minter that mints many identifiers: a player
	// token bound to a video_id, or a GVS token bound to a visitor_data.
	attestKind     string // "", "integrity", or "fallback"
	fallbackToken  string // set on the fallback path (no per-identifier minter)
	lifetimeSecs   int
	tokenExpiresAt time.Time // when tokens from this attestation expire (attest time + lifetime)
}

// Launch starts a dedicated Chromium for one Session, parks a page on videoID's
// watch page, captures the identity, injects the bundle, and builds the Go HTTP
// client. The caller must Close the returned Session. For multiple isolated
// identities on one browser (multi-tenant), use LaunchPool and Pool.NewSession.
func Launch(ctx context.Context, videoID string, opts Options) (*Session, error) {
	opts = withDefaults(opts)
	browser, l, profile, err := launchChromium(opts)
	if err != nil {
		return nil, err
	}
	teardown := func() {
		_ = browser.Close()
		l.Kill()
		_ = os.RemoveAll(profile)
	}
	s, err := setupSession(ctx, browser, videoID, opts)
	if err != nil {
		teardown()
		return nil, err
	}
	s.dispose = teardown
	return s, nil
}

func withDefaults(opts Options) Options {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.NavTimeout <= 0 {
		opts.NavTimeout = 45 * time.Second
	}
	return opts
}

// launchChromium starts a Chromium and connects rod to it, returning the browser,
// its launcher, and the temp profile dir. The caller owns teardown.
func launchChromium(opts Options) (*rod.Browser, *launcher.Launcher, string, error) {
	bin := opts.ChromeBin
	if bin == "" {
		b, err := DetectChrome()
		if err != nil {
			return nil, nil, "", err
		}
		bin = b
	}
	opts.Logger.Info("waxseal: launching chromium", "bin", bin, "headful", opts.Headful)

	// Snap-confined Chromium can only write a user-data-dir under $HOME, not /tmp.
	profileDir, err := os.MkdirTemp(homeTmpBase(), ".waxseal-")
	if err != nil {
		return nil, nil, "", fmt.Errorf("waxseal: temp profile: %w", err)
	}
	l := launcher.New().
		Bin(bin).
		Leakless(false). // avoid go-rod's leakless helper download; Close() kills the browser
		Set("user-data-dir", profileDir).
		Set("no-sandbox").            // snap confinement provides isolation; experiment-only
		Set("disable-dev-shm-usage"). // WSL2 /dev/shm is small
		Set("disable-gpu").
		Set("mute-audio").
		// Without these, go-rod leaves navigator.webdriver === true, which no real
		// browser sets. This removes the automation flag; it does not alter the
		// genuine fingerprint.
		Delete("enable-automation").
		Set("disable-blink-features", "AutomationControlled")
	if opts.Headful {
		l = l.Headless(false)
	} else {
		l = l.Headless(false).Set("headless", "new")
	}

	controlURL, err := l.Launch()
	if err != nil {
		l.Kill()
		_ = os.RemoveAll(profileDir)
		return nil, nil, "", fmt.Errorf("waxseal: launch chromium: %w", err)
	}
	// NoDefaultDevice: otherwise go-rod overrides the UA with a hardcoded stale
	// Chrome/114 Mac string that contradicts the real Linux navigator. Disabling it
	// lets the genuine identity through. Headful reports a clean Chrome; headless
	// leaks HeadlessChrome, which NormalizeUA rewrites.
	browser := rod.New().ControlURL(controlURL).NoDefaultDevice()
	if err := browser.Connect(); err != nil {
		l.Kill()
		_ = os.RemoveAll(profileDir)
		return nil, nil, "", fmt.Errorf("waxseal: connect cdp: %w", err)
	}
	return browser, l, profileDir, nil
}

// setupSession parks a page in browser (the main browser, or an incognito context
// for a tenant), navigates to videoID's watch page, captures the identity, injects
// the bundle, and builds the HTTP client. dispose is left for the caller to set.
// On error the caller is responsible for teardown.
func setupSession(ctx context.Context, browser *rod.Browser, videoID string, opts Options) (_ *Session, err error) {
	s := &Session{browser: browser, log: opts.Logger}

	page, err := browser.Page(proto.TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		return nil, fmt.Errorf("waxseal: new page: %w", err)
	}
	s.page = page.Context(ctx)

	// Bypass CSP so the injected bundle's new Function(interpreter) can run on the
	// youtube.com origin (which otherwise forbids unsafe-eval).
	if err = (proto.PageSetBypassCSP{Enabled: true}).Call(s.page); err != nil {
		return nil, fmt.Errorf("waxseal: bypass csp: %w", err)
	}

	if opts.NormalizeUA {
		if err = s.normalizeUA(ctx); err != nil {
			return nil, err
		}
	}

	watchURL := "https://www.youtube.com/watch?v=" + url.QueryEscape(videoID)
	navCtx, cancel := context.WithTimeout(ctx, opts.NavTimeout)
	defer cancel()
	if err = s.page.Context(navCtx).Navigate(watchURL); err != nil {
		return nil, fmt.Errorf("waxseal: navigate watch page: %w", err)
	}
	if err = s.page.Context(navCtx).WaitLoad(); err != nil {
		return nil, fmt.Errorf("waxseal: wait load: %w", err)
	}

	if err = s.captureIdentity(navCtx, watchURL); err != nil {
		return nil, err
	}
	// signatureTimestamp is mandatory: a /player request without it returns
	// UNPLAYABLE regardless of the token, so it must be captured before any consume.
	if err = s.captureSTS(navCtx); err != nil {
		return nil, err
	}
	opts.Logger.Info("waxseal: identity",
		"visitor_data_len", len(s.id.VisitorData),
		"client_version", s.id.ClientVersion,
		"webdriver", s.id.Webdriver,
		"cookies", s.id.Cookies,
		"sts", s.id.STS)
	if s.id.Webdriver {
		return nil, fmt.Errorf("waxseal: navigator.webdriver is true; automation artifact leaked")
	}

	// Inject the bundle (defines runBotguard/newMinter/mint on globalThis).
	if _, err = s.page.Eval(`(src) => { (0, eval)(src); return true }`, browserBundle); err != nil {
		return nil, fmt.Errorf("waxseal: inject bundle: %w", err)
	}

	if err = s.buildCoherentClient(); err != nil {
		return nil, err
	}
	return s, nil
}

// Pool owns one Chromium and hands out isolated incognito-context Sessions: one
// guest identity (visitor_data, cookies, storage) per tenant, all sharing one
// browser and egress IP. Per-context egress is a future seam; residential
// self-hosting uses the one host IP.
type Pool struct {
	browser  *rod.Browser
	launcher *launcher.Launcher
	profile  string
	opts     Options
}

// LaunchPool starts the shared Chromium. Close it to tear everything down.
func LaunchPool(opts Options) (*Pool, error) {
	opts = withDefaults(opts)
	browser, l, profile, err := launchChromium(opts)
	if err != nil {
		return nil, err
	}
	return &Pool{browser: browser, launcher: l, profile: profile, opts: opts}, nil
}

// NewSession creates a fresh isolated browser context and parks a Session in it.
// Closing the Session disposes just that context, not the shared browser.
func (p *Pool) NewSession(ctx context.Context, videoID string) (*Session, error) {
	incog, err := p.browser.Incognito()
	if err != nil {
		return nil, fmt.Errorf("waxseal: new browser context: %w", err)
	}
	cid := incog.BrowserContextID
	dispose := func() { _ = proto.TargetDisposeBrowserContext{BrowserContextID: cid}.Call(p.browser) }
	s, err := setupSession(ctx, incog, videoID, p.opts)
	if err != nil {
		dispose()
		return nil, err
	}
	s.dispose = dispose
	return s, nil
}

// Close tears down the shared browser and removes the temp profile.
func (p *Pool) Close() {
	if p == nil {
		return
	}
	if p.browser != nil {
		_ = p.browser.Close()
	}
	if p.launcher != nil {
		p.launcher.Kill()
	}
	if p.profile != "" {
		_ = os.RemoveAll(p.profile)
	}
}

// captureIdentity polls ytcfg (it is populated after the SPA boots) for the real
// visitor_data / client version / api key, and records navigator.userAgent and
// navigator.webdriver.
func (s *Session) captureIdentity(ctx context.Context, watchURL string) error {
	const js = `() => {
		const c = (typeof ytcfg !== 'undefined' && ytcfg) ? ytcfg : (window.ytcfg || null);
		const ctxData = c && c.get ? c.get('INNERTUBE_CONTEXT') : null;
		return JSON.stringify({
			vd:   (c && c.get && (c.get('VISITOR_DATA') || (ctxData && ctxData.client && ctxData.client.visitorData))) || "",
			cv:   (c && c.get && c.get('INNERTUBE_CLIENT_VERSION')) || "",
			key:  (c && c.get && c.get('INNERTUBE_API_KEY')) || "",
			ua:   navigator.userAgent || "",
			wd:   navigator.webdriver === true,
		});
	}`
	deadline := time.Now().Add(30 * time.Second)
	var ident struct {
		VD, CV, Key, UA string
		WD              bool
	}
	for {
		obj, err := s.page.Context(ctx).Eval(js)
		if err == nil {
			var raw struct {
				VD  string `json:"vd"`
				CV  string `json:"cv"`
				Key string `json:"key"`
				UA  string `json:"ua"`
				WD  bool   `json:"wd"`
			}
			if jerr := json.Unmarshal([]byte(obj.Value.Str()), &raw); jerr == nil && raw.VD != "" {
				ident.VD, ident.CV, ident.Key, ident.UA, ident.WD = raw.VD, raw.CV, raw.Key, raw.UA, raw.WD
				break
			}
		}
		if time.Now().After(deadline) {
			if ident.UA == "" {
				return fmt.Errorf("waxseal: ytcfg visitor_data not available before deadline")
			}
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	cookies, err := s.page.Context(ctx).Cookies([]string{"https://www.youtube.com"})
	if err != nil {
		return fmt.Errorf("waxseal: read cookies: %w", err)
	}
	s.id = Identity{
		WatchURL:      watchURL,
		VisitorData:   ident.VD,
		ClientVersion: ident.CV,
		APIKey:        ident.Key,
		UserAgent:     ident.UA,
		Webdriver:     ident.WD,
		Cookies:       len(cookies),
	}
	return nil
}

// normalizeUA rewrites navigator.userAgent HeadlessChrome->Chrome (keeping UA-CH
// coherent) via a CDP override applied before navigation, so the BotGuard snapshot
// sees a non-headless UA string. The browser is genuinely Chromium on Linux; only
// the "Headless" marker is removed.
func (s *Session) normalizeUA(ctx context.Context) error {
	obj, err := s.page.Context(ctx).Eval(`() => navigator.userAgent`)
	if err != nil {
		return fmt.Errorf("waxseal: read ua for normalize: %w", err)
	}
	realUA := obj.Value.Str()
	fixed := strings.Replace(realUA, "HeadlessChrome", "Chrome", 1)
	major := "149"
	if m := regexp.MustCompile(`Chrome/(\d+)`).FindStringSubmatch(fixed); m != nil {
		major = m[1]
	}
	full := major + ".0.0.0"
	md := &proto.EmulationUserAgentMetadata{
		Brands: []*proto.EmulationUserAgentBrandVersion{
			{Brand: "Chromium", Version: major},
			{Brand: "Not)A;Brand", Version: "24"},
		},
		FullVersionList: []*proto.EmulationUserAgentBrandVersion{
			{Brand: "Chromium", Version: full},
			{Brand: "Not)A;Brand", Version: "24.0.0.0"},
		},
		Platform:        "Linux",
		PlatformVersion: "",
		Architecture:    "x86",
		Bitness:         "64",
		Mobile:          false,
		FullVersion:     full,
	}
	if err := (proto.NetworkSetUserAgentOverride{UserAgent: fixed, UserAgentMetadata: md}).Call(s.page); err != nil {
		return fmt.Errorf("waxseal: ua override: %w", err)
	}
	s.log.Info("waxseal: normalized UA (HeadlessChrome->Chrome)")
	return nil
}

// captureSTS extracts signatureTimestamp from the player base.js (via an in-page
// fetch, so it uses the page's own session). Without it /player is UNPLAYABLE.
func (s *Session) captureSTS(ctx context.Context) error {
	const js = `async () => {
		const c = (typeof ytcfg !== 'undefined' && ytcfg) ? ytcfg : window.ytcfg;
		const playerUrl = (c && c.get && c.get('PLAYER_JS_URL')) || '';
		if (!playerUrl) return 0;
		try {
			const r = await fetch(new URL(playerUrl, location.origin).href, { credentials: "include" });
			const txt = await r.text();
			const m = txt.match(/signatureTimestamp:(\d+)/) || txt.match(/sts:(\d+)/);
			return m ? parseInt(m[1], 10) : 0;
		} catch (e) { return 0; }
	}`
	obj, err := s.page.Context(ctx).Eval(js)
	if err != nil {
		return fmt.Errorf("waxseal: capture sts: %w", err)
	}
	s.id.STS = obj.Value.Int()
	if s.id.STS == 0 {
		s.log.Warn("waxseal: signatureTimestamp not found; /player will likely be UNPLAYABLE")
	}
	return nil
}

// buildCoherentClient seeds a Go cookie jar from the browser's youtube.com
// cookies so the Go-side att/get + GenerateIT calls carry the same session as the
// page (the egress IP matches automatically, since it is the same host).
func (s *Session) buildCoherentClient() error {
	cookies, err := s.page.Cookies([]string{"https://www.youtube.com"})
	if err != nil {
		return fmt.Errorf("waxseal: cookies for jar: %w", err)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return err
	}
	yt, _ := url.Parse("https://www.youtube.com")
	hc := make([]*http.Cookie, 0, len(cookies))
	for _, c := range cookies {
		hc = append(hc, &http.Cookie{Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path})
	}
	jar.SetCookies(yt, hc)
	s.client = httpx.New(&http.Client{Jar: jar, Timeout: 60 * time.Second})
	return nil
}

// Identity returns the captured real session identity.
func (s *Session) Identity() Identity { return s.id }

// BrowserCookies returns the browser's youtube.com cookies so a consumer can seed
// its own jar and adopt the same session. Loading youtube.com with these returns
// the browser's visitor_data, so attestation, token binding, and the stream share
// one session.
//
// It reads at the browser level (Storage.getCookies) rather than the page level
// (Network.getCookies): a page-level read goes empty once the page leaves
// youtube.com (e.g. a warm minter parked after attest), while the Storage store
// returns the live cookies regardless of page state.
func (s *Session) BrowserCookies() []*http.Cookie {
	cs, err := s.browser.GetCookies()
	if err != nil {
		return nil
	}
	out := make([]*http.Cookie, 0, len(cs))
	for _, c := range cs {
		if !strings.Contains(c.Domain, "youtube.com") {
			continue
		}
		out = append(out, &http.Cookie{
			Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path,
			Secure: c.Secure, HttpOnly: c.HTTPOnly,
		})
	}
	return out
}

// Page exposes the underlying rod.Page (the minter's crash watcher attaches to
// its target-crashed / detached events).
func (s *Session) Page() *rod.Page { return s.page }

// MintResult is one mint outcome: the token, whether it came from the integrity
// or fallback path, and its binding.
type MintResult struct {
	Kind       string    `json:"kind"` // "integrity" | "fallback"
	Token      string    `json:"-"`    // never logged/serialized raw
	TokenLen   int       `json:"token_len"`
	Identifier string    `json:"identifier"` // video_id (player) or visitor_data (gvs)
	Lifetime   int       `json:"lifetime_secs"`
	ExpiresAt  time.Time `json:"-"` // absolute expiry (attest time + lifetime); zero if unknown
}

// Attest runs the expensive once-per-session attestation: att/get challenge (Go)
// -> runBotguard (browser) -> GenerateIT (Go). On success it installs a warm
// per-identifier minter (integrity path) or records the single websafe fallback
// token. Idempotent: later Mint calls reuse the one attestation.
func (s *Session) Attest(ctx context.Context) error {
	if s.attestKind != "" {
		return nil
	}
	ua := s.id.UserAgent

	ictx := innertube.GuestContext(s.id.VisitorData, s.id.ClientVersion)
	ch, err := innertube.GetChallenge(ctx, s.client, ua, ictx)
	if err != nil {
		return fmt.Errorf("waxseal: challenge: %w", err)
	}
	s.log.Info("waxseal: challenge resolved", "interp_len", len(ch.InterpreterJS), "global", ch.GlobalName)

	obj, err := s.page.Context(ctx).Eval(
		`(interp, prog, name) => runBotguard(interp, prog, name)`,
		ch.InterpreterJS, ch.Program, ch.GlobalName,
	)
	if err != nil {
		return fmt.Errorf("waxseal: runBotguard: %w", err)
	}
	botguardResponse := obj.Value.Str()
	if botguardResponse == "" {
		return fmt.Errorf("waxseal: empty botguardResponse")
	}
	s.log.Info("waxseal: snapshot ok", "botguard_response_len", len(botguardResponse))

	it, err := botguard.GenerateIT(ctx, s.client, ua, botguardResponse, botguard.DefaultEndpoint)
	if err != nil {
		return fmt.Errorf("waxseal: GenerateIT: %w", err)
	}
	s.lifetimeSecs = it.LifetimeSecs
	if it.LifetimeSecs > 0 {
		// Tokens from this attestation expire when the attestation does (its
		// lifetime measured from attest time), regardless of when an individual
		// token is later minted off the warm minter.
		s.tokenExpiresAt = time.Now().Add(time.Duration(it.LifetimeSecs) * time.Second)
	}

	if !it.HasIntegrity() {
		if !it.HasFallback() {
			return fmt.Errorf("waxseal: GenerateIT returned no token")
		}
		if _, verr := botguard.ValidatePOToken(it.FallbackToken); verr != nil {
			return fmt.Errorf("waxseal: fallback failed field-6 validation: %w", verr)
		}
		s.attestKind = "fallback"
		s.fallbackToken = it.FallbackToken
		s.log.Warn("waxseal: only a fallback token was granted (no integrity); IP/session not granting integrity right now")
		return nil
	}
	if _, err = s.page.Context(ctx).Eval(`(tok) => newMinter(tok)`, it.IntegrityToken); err != nil {
		return fmt.Errorf("waxseal: newMinter: %w", err)
	}
	s.attestKind = "integrity"
	s.log.Info("waxseal: INTEGRITY attestation installed; warm minter ready", "lifetime_secs", it.LifetimeSecs)
	return nil
}

// Mint produces a token bound to identifier (video_id for the player scope,
// visitor_data for GVS) off the single attestation. The integrity path mints a
// fresh per-identifier token in-browser; the fallback path returns Google's
// single websafe token (identifier-independent). Both are field-6 validated.
func (s *Session) Mint(ctx context.Context, identifier string) (MintResult, error) {
	if err := s.Attest(ctx); err != nil {
		return MintResult{}, err
	}
	if s.attestKind == "fallback" {
		return MintResult{Kind: "fallback", Token: s.fallbackToken, TokenLen: len(s.fallbackToken), Identifier: identifier, Lifetime: s.lifetimeSecs, ExpiresAt: s.tokenExpiresAt}, nil
	}
	mintObj, err := s.page.Context(ctx).Eval(`(id) => mint(id)`, identifier)
	if err != nil {
		return MintResult{}, fmt.Errorf("waxseal: mint: %w", err)
	}
	token := mintObj.Value.Str()
	if token == "" {
		return MintResult{}, fmt.Errorf("waxseal: empty minted token")
	}
	if _, err = botguard.ValidatePOToken(token); err != nil {
		return MintResult{}, fmt.Errorf("waxseal: minted token failed field-6 validation: %w", err)
	}
	s.log.Info("waxseal: integrity token minted", "len", len(token), "identifier_len", len(identifier))
	return MintResult{Kind: "integrity", Token: token, TokenLen: len(token), Identifier: identifier, Lifetime: s.lifetimeSecs, ExpiresAt: s.tokenExpiresAt}, nil
}

// AttestKind reports the attestation outcome ("integrity" or "fallback") after
// Attest/Mint; "" before attestation.
func (s *Session) AttestKind() string { return s.attestKind }

// Close tears down what this Session owns (the whole browser for Launch, or just
// its incognito context for Pool.NewSession) via the dispose closure. Idempotent.
func (s *Session) Close() {
	if s == nil || s.dispose == nil {
		return
	}
	d := s.dispose
	s.dispose = nil
	d()
}

// DetectChrome resolves a Chromium binary: WAXSEAL_CHROME_BIN, then well-known
// system paths (the pinned snap Chromium 149 on this host).
func DetectChrome() (string, error) {
	if b := os.Getenv("WAXSEAL_CHROME_BIN"); b != "" {
		return b, nil
	}
	for _, p := range []string{
		"/usr/bin/chromium-browser",
		"/usr/bin/chromium",
		"/snap/bin/chromium",
		"/usr/bin/google-chrome",
		"/usr/bin/google-chrome-stable",
	} {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("waxseal: no Chromium found; set WAXSEAL_CHROME_BIN")
}

// homeTmpBase returns a $HOME-rooted base dir for the user-data-dir, because
// snap-confined Chromium cannot open a profile under /tmp.
func homeTmpBase() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	return os.TempDir()
}
