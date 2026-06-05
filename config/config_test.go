package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefault(t *testing.T) {
	c := Default()
	if c.Host != DefaultHost || c.Port != DefaultPort {
		t.Errorf("defaults = %s:%d", c.Host, c.Port)
	}
	if c.LogLevel != "info" || c.LogFormat != "text" {
		t.Errorf("log defaults = %s/%s", c.LogLevel, c.LogFormat)
	}
}

func writeFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestApplyFilePartialOverlay(t *testing.T) {
	c := Default()
	// Only port + cache_max_ttl present; host must keep its default.
	if err := c.ApplyFile(writeFile(t, `{"port": 9000, "cache_max_ttl": "6h"}`)); err != nil {
		t.Fatalf("ApplyFile: %v", err)
	}
	if c.Host != DefaultHost {
		t.Errorf("host overwritten to %q; absent key should not change it", c.Host)
	}
	if c.Port != 9000 {
		t.Errorf("port = %d, want 9000", c.Port)
	}
	if c.CacheMaxTTL != 6*time.Hour {
		t.Errorf("cache_max_ttl = %s, want 6h", c.CacheMaxTTL)
	}
}

func TestApplyFileUnknownFieldRejected(t *testing.T) {
	c := Default()
	if err := c.ApplyFile(writeFile(t, `{"bogus": 1}`)); err == nil {
		t.Fatal("expected error for unknown config field")
	}
}

func TestApplyFileSecondsDuration(t *testing.T) {
	c := Default()
	if err := c.ApplyFile(writeFile(t, `{"cache_max_ttl": "21600"}`)); err != nil {
		t.Fatalf("ApplyFile: %v", err)
	}
	if c.CacheMaxTTL != 6*time.Hour {
		t.Errorf("21600s = %s, want 6h", c.CacheMaxTTL)
	}
}

func TestEnvOverridesFile(t *testing.T) {
	path := writeFile(t, `{"port": 9000, "host": "0.0.0.0", "disable_innertube": false}`)
	t.Setenv("POT_SERVER_PORT", "7000")
	t.Setenv("DISABLE_INNERTUBE", "true")

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Port != 7000 {
		t.Errorf("port = %d, want 7000 (env over file)", c.Port)
	}
	if c.Host != "0.0.0.0" {
		t.Errorf("host = %q, want 0.0.0.0 (file, no env)", c.Host)
	}
	if !c.DisableInnertube {
		t.Error("disable_innertube should be true (env over file)")
	}
}

func TestEnvProxyPriority(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "https://secure:8080")
	t.Setenv("HTTP_PROXY", "http://plain:8080")
	c := Default()
	c.ApplyEnv()
	if c.Proxy != "https://secure:8080" {
		t.Errorf("proxy = %q, want HTTPS_PROXY to win", c.Proxy)
	}
}

func TestEnvIgnoresMalformed(t *testing.T) {
	t.Setenv("POT_SERVER_PORT", "not-a-number")
	c := Default()
	c.ApplyEnv()
	if c.Port != DefaultPort {
		t.Errorf("malformed port env changed value to %d", c.Port)
	}
}

func TestLoadDefersValidation(t *testing.T) {
	// Load must not validate: a bad lower-priority value (here a file port) has to
	// survive so a higher-priority flag can override it (flags > env > file).
	cfg, err := Load(writeFile(t, `{"port": 70000}`))
	if err != nil {
		t.Fatalf("Load should defer validation, got: %v", err)
	}
	if cfg.Port != 70000 {
		t.Fatalf("port = %d, want 70000 (loaded, unvalidated)", cfg.Port)
	}
	if cfg.Validate() == nil {
		t.Fatal("Validate should reject port 70000")
	}
	// A valid flag overlay (what the server does next) makes it pass.
	cfg.Port = 4416
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid after flag overlay: %v", err)
	}
}

func TestValidate(t *testing.T) {
	bad := []Config{
		{Port: 0, LogLevel: "info", LogFormat: "text"},
		{Port: 70000, LogLevel: "info", LogFormat: "text"},
		{Port: 4416, LogLevel: "loud", LogFormat: "text"},
		{Port: 4416, LogLevel: "info", LogFormat: "yaml"},
		{Port: 4416, LogLevel: "info", LogFormat: "text", Proxy: "://nope"},
	}
	for i, c := range bad {
		if err := c.Validate(); err == nil {
			t.Errorf("case %d: expected validation error", i)
		}
	}
	ok := Default()
	if err := ok.Validate(); err != nil {
		t.Errorf("default config should validate: %v", err)
	}
}
