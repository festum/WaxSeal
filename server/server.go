// Package server is the WaxSeal HTTP PO-token service: a bgutil-wire /get_pot
// daemon backed by the real-browser minter (internal/browser + internal/minter).
// One Chromium hosts N isolated browser contexts (one guest identity per tenant),
// selected by per-tenant API keys; with no keys it is keyless single-tenant.
package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
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
	mux.HandleFunc("/ping", s.handlePing)
	mux.HandleFunc("/session", s.handleSession)
	mux.HandleFunc("/metrics", s.handleMetrics)
	s.srv = &http.Server{Addr: cfg.Addr, Handler: mux}
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
		writeErr(w, http.StatusUnauthorized, "invalid or missing API key")
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
		writeErr(w, http.StatusUnprocessableEntity, "invalid JSON: "+err.Error())
		return
	}
	if req.ContentBinding == "" {
		writeErr(w, http.StatusBadRequest, "content_binding is required (the video_id for player, or the visitor_data for gvs)")
		return
	}
	scope := req.Scope
	if scope == "" {
		scope = "pot" // the binding distinguishes player (video_id) vs gvs (visitor_data)
	}
	res, cached, err := m.Mint(r.Context(), scope, req.ContentBinding)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "mint failed: "+err.Error())
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
// pair so a consumer can adopt the browser-as-origin (the only way GVS reaches
// STREAM_PROTECTION status 1). The session is anonymous (no Google login).
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	m, label, ok := s.tenant(w, r)
	if !ok {
		return
	}
	id, err := m.Identity(r.Context())
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "no session: "+err.Error())
		return
	}
	raw, err := m.Cookies(r.Context())
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "no cookies: "+err.Error())
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

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
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
