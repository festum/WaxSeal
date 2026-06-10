package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseTenantKeys(t *testing.T) {
	if got := ParseTenantKeys("  "); got != nil {
		t.Errorf("empty input = %v, want nil (keyless)", got)
	}
	m := ParseTenantKeys("alice=KEYA, bob=KEYB")
	if len(m) != 2 || m["KEYA"] != "alice" || m["KEYB"] != "bob" {
		t.Errorf("labelled keys = %v", m)
	}
	bare := ParseTenantKeys("RAWKEY")
	if lbl := bare["RAWKEY"]; lbl == "" || lbl == "RAWKEY" {
		t.Errorf("bare key label = %q, want a generated label that is not the key", lbl)
	}
}

func TestAPIKeyExtraction(t *testing.T) {
	header := httptest.NewRequest(http.MethodGet, "/", nil)
	header.Header.Set("X-API-Key", "H")
	if got := apiKey(header); got != "H" {
		t.Errorf("X-API-Key = %q, want H", got)
	}
	bearer := httptest.NewRequest(http.MethodGet, "/", nil)
	bearer.Header.Set("Authorization", "Bearer B")
	if got := apiKey(bearer); got != "B" {
		t.Errorf("Bearer = %q, want B", got)
	}
	query := httptest.NewRequest(http.MethodGet, "/?key=Q", nil)
	if got := apiKey(query); got != "Q" {
		t.Errorf("query key = %q, want Q", got)
	}
	if got := apiKey(httptest.NewRequest(http.MethodGet, "/", nil)); got != "" {
		t.Errorf("no key = %q, want empty", got)
	}
}
