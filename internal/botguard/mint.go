package botguard

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"github.com/colespringer/waxseal/internal/httpx"
	"github.com/colespringer/waxseal/internal/jsruntime"
)

// GenerateITResult preserves the integrity and fallback tokens independently.
// A response with no integrityToken but a valid field-6 fallbackToken is still a
// successful attestation: the integrity token unlocks the warm per-identifier
// minter, while the websafe fallback is a single field-6-valid token returned
// directly by Google. LifetimeSecs and RefreshThreshold are authoritative for
// validity; CacheMaxTTL can only cap them, never extend them.
type GenerateITResult struct {
	IntegrityToken   string // arr[0]
	LifetimeSecs     int    // arr[1]
	RefreshThreshold int    // arr[2]
	FallbackToken    string // arr[3], websafe fallback PO token
}

// HasIntegrity reports whether the warm-minter path is available.
func (r *GenerateITResult) HasIntegrity() bool { return r != nil && r.IntegrityToken != "" }

// HasFallback reports whether a directly-usable fallback token was issued.
func (r *GenerateITResult) HasFallback() bool { return r != nil && r.FallbackToken != "" }

// Snapshot runs the BotGuard VM on the runtime (the only JS we run) and returns
// the botguardResponse. The runtime must have the bg_bundle preloaded.
// profileJSON, when non-empty, is the coherent BrowserProfile the shim renders
// (navigator/screen/timezone/UA-CH); empty leaves the shim's loaded default.
func Snapshot(ctx context.Context, rt jsruntime.Runtime, ch *Challenge, profileJSON json.RawMessage) (string, error) {
	args := []any{ch.InterpreterJS, ch.Program, ch.GlobalName}
	if len(profileJSON) > 0 {
		args = append(args, profileJSON)
	}
	out, err := rt.Call(ctx, "runBotguard", args...)
	if err != nil {
		return "", stageErr(StageVM, "runBotguard: %w", err)
	}
	var resp string
	if err := json.Unmarshal(out, &resp); err != nil || resp == "" {
		return "", stageErr(StageVM, "empty botguardResponse")
	}
	return resp, nil
}

// GenerateIT posts the botguardResponse and parses the result tuple. All HTTP is
// done in Go. A response carrying only the fallback token (arr[0] null) is
// success; the caller decides how to use it. Only a response with neither token
// is an error.
func GenerateIT(ctx context.Context, client *httpx.Client, userAgent, botguardResponse string, ep Endpoint) (*GenerateITResult, error) {
	ep = ep.orDefault()
	body, _ := json.Marshal([]string{RequestKey, botguardResponse})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.GenerateITURL, bytes.NewReader(body))
	if err != nil {
		return nil, stageErr(StageGenerateIT, "build request: %w", err)
	}
	setProtoHeaders(req, userAgent)

	raw, err := client.DoJSON(req, maxChallengeBody)
	if err != nil {
		return nil, stageErr(StageGenerateIT, "%w", err)
	}
	return parseGenerateIT(raw)
}

// parseGenerateIT decodes the [integrityToken, lifetime, refreshThreshold,
// fallbackToken] tuple, treating fallback-only as a successful attestation.
func parseGenerateIT(raw []byte) (*GenerateITResult, error) {
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil || len(arr) < 2 {
		return nil, stageErr(StageGenerateIT, "unexpected response shape")
	}
	it := &GenerateITResult{}
	_ = json.Unmarshal(arr[0], &it.IntegrityToken)
	_ = json.Unmarshal(arr[1], &it.LifetimeSecs)
	if len(arr) >= 3 {
		_ = json.Unmarshal(arr[2], &it.RefreshThreshold)
	}
	if len(arr) >= 4 {
		_ = json.Unmarshal(arr[3], &it.FallbackToken)
	}
	if !it.HasIntegrity() && !it.HasFallback() {
		return nil, stageErr(StageGenerateIT, "no integrity or fallback token")
	}
	return it, nil
}

// InstallMinter creates the WebPoMinter on the runtime from the integrity token.
// It must run on the same runtime as Snapshot (webPoSignalOutput[0] is a live JS
// closure on that runtime).
func InstallMinter(ctx context.Context, rt jsruntime.Runtime, integrityToken string) error {
	if _, err := rt.Call(ctx, "newMinter", integrityToken); err != nil {
		return stageErr(StageMint, "newMinter: %w", err)
	}
	return nil
}

// Mint mints a websafe-base64 token bound to identifier and validates field 6.
func Mint(ctx context.Context, rt jsruntime.Runtime, identifier string) (string, error) {
	out, err := rt.Call(ctx, "mint", identifier)
	if err != nil {
		return "", stageErr(StageMint, "mint: %w", err)
	}
	var token string
	if err := json.Unmarshal(out, &token); err != nil || token == "" {
		return "", stageErr(StageMint, "empty token")
	}
	if _, err := ValidatePOToken(token); err != nil {
		return "", stageErr(StageValidate, "%w", err)
	}
	return token, nil
}
