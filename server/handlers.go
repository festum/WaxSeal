package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	waxseal "github.com/colespringer/waxseal"
	"github.com/colespringer/waxseal/internal/botguard"
	"github.com/colespringer/waxseal/internal/httpx"
)

// potRequest is the /get_pot request body (bgutil-compatible field names).
type potRequest struct {
	ContentBinding         string          `json:"content_binding"`
	Proxy                  string          `json:"proxy"`
	BypassCache            bool            `json:"bypass_cache"`
	Challenge              json.RawMessage `json:"challenge"`
	InnertubeContext       json.RawMessage `json:"innertube_context"`
	DisableInnertube       *bool           `json:"disable_innertube"`
	DisableTLSVerification bool            `json:"disable_tls_verification"`
	SourceAddress          string          `json:"source_address"`
}

// potResponse is the /get_pot response body (camelCase, RFC3339 expiry).
type potResponse struct {
	PoToken        string `json:"poToken"`
	ContentBinding string `json:"contentBinding"`
	ExpiresAt      string `json:"expiresAt"`
}

// handleGetPot mints (or serves from cache) an opaque PO token. content_binding
// is an opaque mint identifier; when omitted the server sources a guest
// visitor_data and binds to that (returning it as contentBinding).
func (s *Server) handleGetPot(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "request body too large or unreadable")
		return
	}

	// Deprecated-field rejection runs before typed parsing (bgutil parity).
	if field := deprecatedField(body); field != "" {
		writeError(w, http.StatusBadRequest, field+" is deprecated, use content_binding instead")
		return
	}

	var req potRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid JSON: "+err.Error())
		return
	}

	// Inline interpreter challenges run caller-provided JS inside the VM. Keep
	// them behind shared-secret auth; URL challenges remain allowed because
	// ResolveInterpreter restricts them to the Google host allowlist.
	if len(req.Challenge) > 0 && !s.trustCallers() {
		if ch, perr := botguard.ParseProvidedChallenge(req.Challenge); perr == nil && ch.InterpreterJS != "" {
			writeError(w, http.StatusBadRequest, "inline interpreter challenges require authentication")
			return
		}
	}

	egress := s.egressFor(req)
	binding := req.ContentBinding
	if binding == "" {
		// Caller omitted the binding: fetch guest visitor_data before minting.
		vd, err := s.client.VisitorData(r.Context(), egress)
		if err != nil {
			s.writeMintError(w, err)
			return
		}
		binding = vd
	}

	tok, err := s.client.Token(r.Context(), waxseal.Request{
		Scope:            waxseal.ScopeOpaque,
		Identifier:       binding,
		BypassCache:      req.BypassCache,
		Egress:           egress,
		Challenge:        req.Challenge,
		InnertubeContext: req.InnertubeContext,
		DisableInnertube: req.DisableInnertube,
	})
	if err != nil {
		s.writeMintError(w, err)
		return
	}

	expires := tok.ExpiresAt
	if expires.IsZero() {
		expires = s.now().Add(time.Hour)
	}
	s.logger.Debug("minted pot",
		"binding", redact(binding), "egress", egress.ID, "proxy", redact(req.Proxy), "expires", expires)
	writeJSON(w, http.StatusOK, potResponse{
		PoToken:        tok.Value,
		ContentBinding: binding,
		ExpiresAt:      expires.UTC().Format(time.RFC3339),
	})
}

// egressFor builds the EgressSpec for a request. Per-request proxy/source/TLS
// overrides are honored only when explicitly allowed; otherwise they are ignored
// (logged) and the server's default egress is used.
func (s *Server) egressFor(req potRequest) waxseal.EgressSpec {
	spec := s.opts.DefaultEgress
	requested := req.Proxy != "" || req.SourceAddress != "" || req.DisableTLSVerification
	switch {
	case requested && s.opts.AllowRequestEgressOverride:
		// A request that names its own egress fully replaces the server default.
		spec = waxseal.EgressSpec{Proxy: req.Proxy, SourceAddress: req.SourceAddress, DisableTLSVerify: req.DisableTLSVerification}
	case requested:
		s.logger.Warn("ignoring per-request egress override (start with --allow-request-egress-override to honor it)",
			"proxy", redact(req.Proxy), "source_address", redact(req.SourceAddress))
	}
	spec.ID = spec.DerivedID()
	return spec
}

// trustCallers reports whether callers are trusted enough to supply inline
// interpreter JS: only when the server authenticates them with a shared secret.
func (s *Server) trustCallers() bool {
	return s.opts.SharedSecret != ""
}

// handlePing reports uptime and version. It never mints and needs no auth, so it
// is suitable for health checks.
func (s *Server) handlePing(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"server_uptime": int64(s.now().Sub(s.startTime).Seconds()),
		"version":       s.opts.Version,
	})
}

// handleInvalidateCaches drops cached tokens (warm minters keep serving).
func (s *Server) handleInvalidateCaches(w http.ResponseWriter, _ *http.Request) {
	s.client.PurgeTokens()
	w.WriteHeader(http.StatusNoContent)
}

// handleInvalidateIT evicts warm minter sessions so the next request re-attests.
func (s *Server) handleInvalidateIT(w http.ResponseWriter, _ *http.Request) {
	s.client.InvalidateMinters()
	w.WriteHeader(http.StatusNoContent)
}

// handleMinterCache returns the warm-minter keys as a raw JSON array of strings.
func (s *Server) handleMinterCache(w http.ResponseWriter, _ *http.Request) {
	keys := s.client.MinterKeys()
	if keys == nil {
		keys = []string{}
	}
	writeJSON(w, http.StatusOK, keys)
}

// handleMetrics renders WaxSeal's counters in Prometheus text exposition format.
// Metrics contain non-sensitive operational counts, but they stay behind the
// shared secret like every endpoint except /ping.
func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if err := s.client.WriteMetrics(w); err != nil {
		s.logger.Error("write metrics failed", "err", err)
	}
}

// deprecatedField reports the first deprecated top-level key present, else "".
// It uses exact (case-sensitive) key matching, matching bgutil.
func deprecatedField(body []byte) string {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return ""
	}
	for _, k := range []string{"data_sync_id", "visitor_data"} {
		if _, ok := obj[k]; ok {
			return k
		}
	}
	return ""
}

// writeMintError maps mint failures to stage-specific messages without tokens or
// payloads. An open breaker is a transient 503; other failures are 500.
func (s *Server) writeMintError(w http.ResponseWriter, err error) {
	s.logger.Error("token generation failed", "err", err)
	msg := "token generation failed"
	var se *botguard.StageError
	if errors.As(err, &se) {
		msg = string(se.Stage) + ": token generation failed"
	}
	status := http.StatusInternalServerError
	if errors.Is(err, httpx.ErrBreakerOpen) {
		status, msg = http.StatusServiceUnavailable, "attestation cooling down; retry later"
	}
	writeError(w, status, msg)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
