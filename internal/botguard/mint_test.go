package botguard

import (
	"encoding/json"
	"errors"
	"testing"
)

// A GenerateIT response with a null integrity token but a present fallback token
// is a successful attestation; parseGenerateIT must still read arr[3].
func TestParseGenerateITFallbackOnly(t *testing.T) {
	raw := mustJSON(t, []any{nil, 43200, nil, "websafeFallbackToken"})
	res, err := parseGenerateIT(raw)
	if err != nil {
		t.Fatalf("fallback-only should succeed: %v", err)
	}
	if res.HasIntegrity() {
		t.Fatal("did not expect an integrity token")
	}
	if !res.HasFallback() || res.FallbackToken != "websafeFallbackToken" {
		t.Fatalf("fallback not parsed: %+v", res)
	}
	if res.LifetimeSecs != 43200 {
		t.Fatalf("lifetime = %d, want 43200", res.LifetimeSecs)
	}
}

func TestParseGenerateITIntegrity(t *testing.T) {
	raw := mustJSON(t, []any{"integrity", 3600, 1800, "fallback"})
	res, err := parseGenerateIT(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !res.HasIntegrity() || res.IntegrityToken != "integrity" {
		t.Fatalf("integrity not parsed: %+v", res)
	}
	if res.RefreshThreshold != 1800 {
		t.Fatalf("refreshThreshold = %d, want 1800", res.RefreshThreshold)
	}
	if res.FallbackToken != "fallback" {
		t.Fatalf("fallback = %q", res.FallbackToken)
	}
}

func TestParseGenerateITNeitherTokenErrors(t *testing.T) {
	raw := mustJSON(t, []any{nil, 43200, nil, ""})
	_, err := parseGenerateIT(raw)
	if err == nil {
		t.Fatal("expected error when neither integrity nor fallback present")
	}
	var se *StageError
	if !errors.As(err, &se) || se.Stage != StageGenerateIT {
		t.Fatalf("want StageGenerateIT, got %v", err)
	}
}

func TestParseGenerateITBadShape(t *testing.T) {
	if _, err := parseGenerateIT([]byte(`"not an array"`)); err == nil {
		t.Fatal("expected error for non-array response")
	}
	if _, err := parseGenerateIT([]byte(`[null]`)); err == nil {
		t.Fatal("expected error for too-short response")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
