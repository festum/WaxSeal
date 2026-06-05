// Package server is WaxSeal's standalone HTTP daemon. Its wire format is
// compatible with the bgutil POT provider (so existing yt-dlp POT plugins work
// unchanged) but it is implemented independently over the standard library's
// net/http and log/slog; the GPL bgutil project is a behavioral reference only.
//
// It defaults to loopback exposure, treats content_binding as an opaque mint
// identifier, rejects the deprecated visitor_data/data_sync_id fields, and
// requires explicit opt-ins for network-changing per-request egress overrides
// (proxy/source_address/disable_tls_verification) and caller-supplied inline
// interpreter challenges.
package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	waxseal "github.com/colespringer/waxseal"
)

// SecretHeader carries the optional shared secret. Requests must present it
// (constant-time compared) when Options.SharedSecret is set; /ping is exempt so
// container health checks need no credential.
const SecretHeader = "X-WaxSeal-Secret"

// maxRequestBody bounds an inbound /get_pot body. Challenges reference small
// interpreter URLs; inline interpreter bodies are rejected anyway.
const maxRequestBody = 2 << 20

// Client is the subset of *waxseal.Client the server needs, named so handlers are
// testable without standing up the real BotGuard VM.
type Client interface {
	Token(ctx context.Context, req waxseal.Request) (waxseal.Token, error)
	VisitorData(ctx context.Context, egress waxseal.EgressSpec) (string, error)
	PurgeTokens()
	InvalidateMinters()
	MinterKeys() []string
}

// Options configures a Server.
type Options struct {
	Host                       string             // bind address (loopback default)
	Port                       int                // default 4416
	SharedSecret               string             // optional; when set, required on every endpoint but /ping
	AllowRequestEgressOverride bool               // honor per-request proxy/source_address/disable_tls_verification
	DefaultEgress              waxseal.EgressSpec // baseline egress (server's configured proxy/source)
	Version                    string             // reported by /ping
	Logger                     *slog.Logger
	now                        func() time.Time // test hook
}

// Server serves the bgutil-compatible API over a waxseal.Client.
type Server struct {
	client    Client
	opts      Options
	logger    *slog.Logger
	startTime time.Time
	now       func() time.Time
}

// New builds a Server. host/port default to loopback:4416 when unset.
func New(client Client, opts Options) *Server {
	if opts.Host == "" {
		opts.Host = "127.0.0.1"
	}
	if opts.Port == 0 {
		opts.Port = 4416
	}
	if opts.Version == "" {
		opts.Version = "dev"
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.now == nil {
		opts.now = time.Now
	}
	return &Server{client: client, opts: opts, logger: opts.Logger, startTime: opts.now(), now: opts.now}
}

// Addr is the host:port the server binds.
func (s *Server) Addr() string {
	return net.JoinHostPort(s.opts.Host, strconv.Itoa(s.opts.Port))
}

// Handler builds the routed, middleware-wrapped handler (exported for tests).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /get_pot", s.handleGetPot)
	mux.HandleFunc("GET /ping", s.handlePing)
	mux.HandleFunc("POST /invalidate_caches", s.handleInvalidateCaches)
	mux.HandleFunc("POST /invalidate_it", s.handleInvalidateIT)
	mux.HandleFunc("GET /minter_cache", s.handleMinterCache)
	return s.logging(s.auth(mux))
}

// ListenAndServe binds and serves until ctx is cancelled, then drains in-flight
// requests within a bounded grace period (graceful shutdown).
func (s *Server) ListenAndServe(ctx context.Context) error {
	hs := &http.Server{
		Addr:              s.Addr(),
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- hs.ListenAndServe() }()
	s.logger.Info("waxseal server listening",
		"addr", s.Addr(), "auth", s.opts.SharedSecret != "", "egress_override", s.opts.AllowRequestEgressOverride)

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		s.logger.Info("waxseal server shutting down")
		return hs.Shutdown(shutCtx)
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// auth enforces the shared secret on all endpoints except /ping. With no secret
// configured it is a pass-through (the server is loopback by default).
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.opts.SharedSecret != "" && r.URL.Path != "/ping" {
			got := r.Header.Get(SecretHeader)
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.opts.SharedSecret)) != 1 {
				writeError(w, http.StatusUnauthorized, "missing or invalid "+SecretHeader)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// logging records each request with a redacted summary (no tokens; binding and
// proxy are hashed). It captures the status via a small ResponseWriter wrapper.
func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := s.now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		s.logger.Info("request",
			"method", r.Method, "path", r.URL.Path,
			"status", sw.status, "duration_ms", s.now().Sub(start).Milliseconds())
	})
}

// statusWriter remembers the response status for access logging.
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

// redact returns a short, non-reversible tag for a sensitive value (binding,
// proxy) so logs are useful without leaking visitor_data/proxy credentials.
func redact(s string) string {
	if s == "" {
		return ""
	}
	h := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(h[:])[:12]
}
