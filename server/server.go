// Package server is the WaxSeal HTTP PO-token service: a bgutil-wire /get_pot
// daemon backed by the real-browser minter (internal/browser + internal/minter).
// One Chromium hosts N isolated browser contexts (one guest identity per tenant),
// selected by per-tenant API keys; with no keys it is keyless single-tenant.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/colespringer/waxseal/internal/browser"
	"github.com/colespringer/waxseal/internal/minter"
)

// Config configures a Server. The zero value is usable: keyless, loopback, a
// stable landing video, headless.
type Config struct {
	Addr       string            // listen address (default 127.0.0.1:4416)
	Video      string            // landing video for each tenant session (default a stable id)
	Headful    bool              // run headful (needs a display/Xvfb)
	TenantKeys map[string]string // api key -> tenant label; nil = keyless single tenant
	Logger     *slog.Logger      // nil discards
}

// Server is the running HTTP service over a real-browser minter.
type Server struct {
	tenants *minter.Tenants
	log     *slog.Logger
	srv     *http.Server
}

// requestProcessTimeout bounds how long one request can hold the per-tenant page
// mutex. It allows the full cold-start retry sequence while preventing a hung
// request from holding the mutex indefinitely.
const requestProcessTimeout = 3 * time.Minute

// New launches the shared Chromium and builds the service. It does not attest
// until Warm or the first request. Shutdown tears the browser down.
func New(cfg Config) (*Server, error) {
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:4416"
	}
	if cfg.Video == "" {
		cfg.Video = browser.DefaultVideo
	}
	opts := browser.Options{
		Headful:     cfg.Headful,
		NormalizeUA: !cfg.Headful, // headless: rewrite HeadlessChrome -> Chrome
		Logger:      log,
	}
	pool, err := browser.LaunchPool(opts)
	if err != nil {
		return nil, err
	}
	s := &Server{
		tenants: minter.NewTenants(pool, cfg.Video, cfg.TenantKeys, opts),
		log:     log,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/get_pot", s.handleGetPot)
	mux.HandleFunc("/player-context", s.handlePlayerContext)
	mux.HandleFunc("/ping", s.handlePing)
	mux.HandleFunc("/session", s.handleSession)
	mux.HandleFunc("/metrics", s.handleMetrics)
	s.srv = &http.Server{Addr: cfg.Addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	return s, nil
}

// Warm attests the tenant the API key selects (pass "" for keyless), so startup
// fails loudly if the browser/IP can't attest.
func (s *Server) Warm(ctx context.Context, apiKey string) error {
	return s.tenants.WarmOne(ctx, apiKey)
}

// Addr is the configured listen address.
func (s *Server) Addr() string { return s.srv.Addr }

// ListenAndServe runs the HTTP server until Shutdown.
func (s *Server) ListenAndServe() error { return s.srv.ListenAndServe() }

// Shutdown drains in-flight requests, then tears down the browser.
func (s *Server) Shutdown(ctx context.Context) error {
	err := s.srv.Shutdown(ctx)
	s.tenants.Close()
	return err
}

// apiKey extracts the tenant key from the header (preferred) or a query param.
func apiKey(r *http.Request) string {
	if k := r.Header.Get("X-API-Key"); k != "" {
		return k
	}
	if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(a, "Bearer "))
	}
	return r.URL.Query().Get("key")
}

// tenant resolves the request's Minter, writing 401 and returning ok=false on an
// unknown key.
func (s *Server) tenant(w http.ResponseWriter, r *http.Request) (*minter.Minter, string, bool) {
	m, label, err := s.tenants.Minter(apiKey(r))
	if err != nil {
		writeErr(w, http.StatusUnauthorized, CodeUnauthorized, "invalid or missing API key")
		return nil, "", false
	}
	return m, label, true
}

func (s *Server) handleGetPot(w http.ResponseWriter, r *http.Request) {
	m, label, ok := s.tenant(w, r)
	if !ok {
		return
	}
	var req struct {
		ContentBinding string `json:"content_binding"`
		Scope          string `json:"scope"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, CodeInvalidRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.ContentBinding == "" {
		writeErr(w, http.StatusBadRequest, CodeInvalidRequest, "content_binding is required (the video_id for player, or the visitor_data for gvs)")
		return
	}
	scope, ok := normalizeScope(req.Scope)
	if !ok {
		writeErr(w, http.StatusBadRequest, CodeInvalidRequest, `scope must be "player", "gvs", or omitted`)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestProcessTimeout)
	defer cancel()
	res, cached, err := m.Mint(ctx, scope, req.ContentBinding)
	if err != nil {
		writeErr(w, http.StatusBadGateway, CodeMintFailed, "mint failed: "+err.Error())
		return
	}
	// Use the token's real expiry (fixed at attest time, preserved through the
	// cache) rather than now+lifetime; otherwise a cache hit overstates expiry by
	// the token's age.
	expires := res.ExpiresAt
	if expires.IsZero() {
		expires = time.Now().Add(6 * time.Hour)
	}
	if cached {
		w.Header().Set("X-POT-Cache", "hit")
	} else {
		w.Header().Set("X-POT-Cache", "miss")
	}
	s.log.Info("minted", "tenant", label, "binding_len", len(req.ContentBinding), "scope", scope, "kind", res.Kind, "token_len", res.TokenLen, "cached", cached)
	writeJSON(w, http.StatusOK, map[string]any{
		"poToken":        res.Token,
		"contentBinding": req.ContentBinding,
		"expiresAt":      expires.UTC().Format(time.RFC3339),
	})
}

// handlePlayerContext returns the attested browser's /player streaming context for
// a video_id: the serverAbrStreamingUrl (status-1 graded by the browser's
// provenance, carrying a SCRAMBLED throttling nonce the consumer descrambles), the
// ustreamer config, the visitor_data to bind a GVS PO token to, the client version,
// and the audio formats (each with the itag+lmt+xtags triple needed to select a
// coherent format). This hands the consumer everything needed to stream WEB SABR
// audio from its own Go client; WaxSeal does no SABR/streaming itself.
func (s *Server) handlePlayerContext(w http.ResponseWriter, r *http.Request) {
	m, label, ok := s.tenant(w, r)
	if !ok {
		return
	}
	videoID, ok := playerContextVideoID(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestProcessTimeout)
	defer cancel()
	pc, err := m.PlayerContext(ctx, videoID)
	if err != nil {
		// Give the terminal and timeout cases their own codes so a caller can skip
		// retrying an unplayable video (422) and tell a slow browser (504) from a
		// broken one (502).
		switch {
		case errors.Is(err, browser.ErrUnplayable):
			// Preserve the playabilityStatus so clients do not need to parse the
			// human-readable error.
			status := ""
			if ue, ok := errors.AsType[*browser.UnplayableError](err); ok {
				status = ue.Status
			}
			writeErrDetails(w, http.StatusUnprocessableEntity, CodeVideoUnavailable, err.Error(), status)
		case errors.Is(err, context.DeadlineExceeded):
			writeErr(w, http.StatusGatewayTimeout, CodeTimeout, "player-context timed out")
		default:
			writeErr(w, http.StatusBadGateway, CodePlayerContextFailed, "player-context failed: "+err.Error())
		}
		return
	}
	s.log.Info("player-context handed out", "tenant", label, "video_id_len", len(videoID),
		"status", pc.Status, "abr_url_len", len(pc.ServerAbrStreamingURL), "audio_formats", len(pc.AudioFormats))
	writeJSON(w, http.StatusOK, pc)
}

// videoIDPattern is a cheap sanity check on a video id: base64url characters,
// bounded length. It rejects obvious junk (an overlong id, or path/quote/space
// characters) with a 400 instead of burning the whole browser poll budget on it.
// Real ids are 11 chars; the looser bound avoids hard-coding that.
var videoIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// playerContextVideoID reads the video_id from the JSON body or, for a bodyless
// request, the query param. An empty body decodes to io.EOF (not a JSON error), so it
// falls through to the query form rather than 422-ing. It writes the error response
// and returns ok=false on a malformed body, or a missing or malformed id.
func playerContextVideoID(w http.ResponseWriter, r *http.Request) (string, bool) {
	var req struct {
		VideoID string `json:"video_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, CodeInvalidRequest, "invalid JSON: "+err.Error())
		return "", false
	}
	if req.VideoID == "" {
		req.VideoID = r.URL.Query().Get("video_id")
	}
	if req.VideoID == "" {
		writeErr(w, http.StatusBadRequest, CodeInvalidRequest, "video_id is required")
		return "", false
	}
	if !videoIDPattern.MatchString(req.VideoID) {
		msg := "video_id must be 1-64 chars of [A-Za-z0-9_-]"
		if strings.Contains(req.VideoID, "://") {
			msg = "video_id must be a bare id, not a URL"
		}
		writeErr(w, http.StatusBadRequest, CodeInvalidRequest, msg)
		return "", false
	}
	return req.VideoID, true
}

// normalizeScope canonicalizes the cache scope for /get_pot. It trims whitespace
// and accepts names case-insensitively. Empty scope and "pot" map to the generic
// scope. The content_binding, not the scope, determines the token type.
func normalizeScope(raw string) (string, bool) {
	switch s := strings.ToLower(strings.TrimSpace(raw)); s {
	case "", "pot":
		return "pot", true
	case "player", "gvs":
		return s, true
	default:
		return "", false
	}
}

func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	m, label, ok := s.tenant(w, r)
	if !ok {
		return
	}
	id, err := m.Identity(r.Context())
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "tenant": label, "error": err.Error()})
		return
	}
	kind, _ := m.AttestKind(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tenant": label, "attest": kind, "identity": id})
}

// sessionCookie is one youtube.com cookie in a shape a consumer can rebuild an
// http.Cookie / cookie jar from.
type sessionCookie struct {
	Name     string `json:"name"`
	Value    string `json:"value"`
	Domain   string `json:"domain"`
	Path     string `json:"path"`
	Secure   bool   `json:"secure"`
	HTTPOnly bool   `json:"http_only"`
}

// handleSession hands out the tenant context's coherent {visitor_data, cookies}
// pair so a consumer can present the same anonymous identity the GVS PO token is
// bound to. Adopting it keeps the token, session, and egress IP coherent, which
// is necessary but does not by itself drive STREAM_PROTECTION status: a fully
// coherent GVS session (matching token + session + IP) still streams under
// status 2. The session is anonymous (no Google login).
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	m, label, ok := s.tenant(w, r)
	if !ok {
		return
	}
	id, err := m.Identity(r.Context())
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, CodeNoSession, "no session: "+err.Error())
		return
	}
	raw, err := m.Cookies(r.Context())
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, CodeNoSession, "no cookies: "+err.Error())
		return
	}
	cookies := make([]sessionCookie, 0, len(raw))
	pairs := make([]string, 0, len(raw))
	for _, c := range raw {
		cookies = append(cookies, sessionCookie{
			Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path,
			Secure: c.Secure, HTTPOnly: c.HttpOnly,
		})
		pairs = append(pairs, c.Name+"="+c.Value)
	}
	s.log.Info("session handed out", "tenant", label, "visitor_data_len", len(id.VisitorData), "cookies", len(cookies))
	writeJSON(w, http.StatusOK, map[string]any{
		"visitor_data":   id.VisitorData,
		"user_agent":     id.UserAgent,
		"client_version": id.ClientVersion,
		"cookies":        cookies,
		"cookie_header":  strings.Join(pairs, "; "),
	})
}

// handleMetrics is unauthenticated ops data: per-tenant counters + state, no
// tokens/cookies/keys.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.tenants.MetricsSnapshot())
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

const (
	// CodeUnauthorized indicates a missing or invalid API key.
	CodeUnauthorized = "unauthorized"
	// CodeInvalidRequest indicates malformed input or a missing required field.
	CodeInvalidRequest = "invalid-request"
	// CodeMintFailed indicates that the daemon could not mint a token.
	CodeMintFailed = "mint-failed"
	// CodeVideoUnavailable indicates a terminal playabilityStatus.
	CodeVideoUnavailable = "video-unavailable"
	// CodeTimeout indicates that the player-context deadline elapsed.
	CodeTimeout = "timeout"
	// CodePlayerContextFailed indicates a non-terminal player-context failure.
	CodePlayerContextFailed = "player-context-failed"
	// CodeNoSession indicates that no attested session or cookies are available.
	CodeNoSession = "no-session"
)

// errEnvelope is the JSON error response shared by the API endpoints.
type errEnvelope struct {
	Error   string `json:"error"`
	Code    string `json:"code"`
	Details string `json:"details,omitempty"`
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errEnvelope{Error: msg, Code: code})
}

func writeErrDetails(w http.ResponseWriter, status int, code, msg, details string) {
	writeJSON(w, status, errEnvelope{Error: msg, Code: code, Details: details})
}

// ParseTenantKeys parses "label1=key1,label2=key2" (or bare "key") into a
// key->label map. Empty input is keyless single-tenant mode.
func ParseTenantKeys(s string) map[string]string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	out := map[string]string{}
	for i, pair := range strings.Split(s, ",") {
		if pair = strings.TrimSpace(pair); pair == "" {
			continue
		}
		before, after, found := strings.Cut(pair, "=")
		key, label := strings.TrimSpace(before), ""
		if found {
			label, key = strings.TrimSpace(before), strings.TrimSpace(after)
		} else {
			label = "t" + strconv.Itoa(i+1) // don't echo the key as a label
		}
		if key != "" {
			out[key] = label
		}
	}
	return out
}
