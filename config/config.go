// Package config builds WaxSeal's server/CLI configuration from defaults, a JSON
// file, environment variables, and CLI flags. The CLI applies flags after Load
// returns, giving precedence flags > env > file > defaults. Each layer updates
// only keys it contains, so unset higher-priority fields leave lower layers
// intact. Config stays free of waxseal core types; the server and CLI translate
// it into waxseal.Options and server.Options.
package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Default host and port keep the daemon on loopback unless configured otherwise.
const (
	DefaultHost = "127.0.0.1"
	DefaultPort = 4416
)

// Config is the resolved configuration shared by the server and CLI.
type Config struct {
	Host string // bind address (loopback default; set :: / 0.0.0.0 to expose)
	Port int

	CacheDir    string        // wazero AOT compilation cache
	CacheMaxTTL time.Duration // caps cached token validity; 0 = uncapped

	Proxy            string // default egress proxy (HTTPS_PROXY/HTTP_PROXY)
	SourceAddress    string // default egress source IP
	DisableTLSVerify bool   // default egress: skip TLS verification (discouraged)
	DisableInnertube bool   // skip InnerTube att/get; go straight to Create

	LogLevel  string // debug | info | warn | error
	LogFormat string // text | json

	// Server-only knobs.
	SharedSecret               string // optional X-WaxSeal-Secret required on requests
	AllowRequestEgressOverride bool   // honor per-request proxy/source_address/TLS overrides
}

// Default returns the baseline configuration before any file/env/flag overlay.
func Default() Config {
	return Config{
		Host:      DefaultHost,
		Port:      DefaultPort,
		LogLevel:  "info",
		LogFormat: "text",
	}
}

// Load applies a file (when provided), then env, over defaults. It does not
// validate because CLI flags are applied afterward; callers validate the final
// config.
func Load(path string) (Config, error) {
	c := Default()
	if path != "" {
		if err := c.ApplyFile(path); err != nil {
			return Config{}, err
		}
	}
	c.ApplyEnv()
	return c, nil
}

// fileSchema mirrors Config with pointers, so a JSON config overlays only the
// keys it contains (absent keys stay nil and leave the prior layer intact).
type fileSchema struct {
	Host                       *string `json:"host"`
	Port                       *int    `json:"port"`
	CacheDir                   *string `json:"cache_dir"`
	CacheMaxTTL                *string `json:"cache_max_ttl"` // duration ("6h") or seconds
	Proxy                      *string `json:"proxy"`
	SourceAddress              *string `json:"source_address"`
	DisableTLSVerify           *bool   `json:"disable_tls_verification"`
	DisableInnertube           *bool   `json:"disable_innertube"`
	LogLevel                   *string `json:"log_level"`
	LogFormat                  *string `json:"log_format"`
	SharedSecret               *string `json:"shared_secret"`
	AllowRequestEgressOverride *bool   `json:"allow_request_egress_override"`
}

// ApplyFile overlays a JSON config file onto c, setting only the keys present.
func (c *Config) ApplyFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("config: read %s: %w", path, err)
	}
	var f fileSchema
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return fmt.Errorf("config: parse %s: %w", path, err)
	}
	setString(&c.Host, f.Host)
	setInt(&c.Port, f.Port)
	setString(&c.CacheDir, f.CacheDir)
	if f.CacheMaxTTL != nil {
		d, err := parseDuration(*f.CacheMaxTTL)
		if err != nil {
			return fmt.Errorf("config: cache_max_ttl: %w", err)
		}
		c.CacheMaxTTL = d
	}
	setString(&c.Proxy, f.Proxy)
	setString(&c.SourceAddress, f.SourceAddress)
	setBool(&c.DisableTLSVerify, f.DisableTLSVerify)
	setBool(&c.DisableInnertube, f.DisableInnertube)
	setString(&c.LogLevel, f.LogLevel)
	setString(&c.LogFormat, f.LogFormat)
	setString(&c.SharedSecret, f.SharedSecret)
	setBool(&c.AllowRequestEgressOverride, f.AllowRequestEgressOverride)
	return nil
}

// ApplyEnv overlays environment variables when present. Invalid numeric,
// duration, or bool values are ignored so the prior layer remains in effect.
func (c *Config) ApplyEnv() {
	if v, ok := os.LookupEnv("POT_SERVER_HOST"); ok {
		c.Host = v
	}
	if v, ok := os.LookupEnv("POT_SERVER_PORT"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			c.Port = n
		}
	}
	if v, ok := os.LookupEnv("CACHE_DIR"); ok {
		c.CacheDir = v
	}
	if v, ok := os.LookupEnv("CACHE_MAX_TTL"); ok {
		if d, err := parseDuration(v); err == nil {
			c.CacheMaxTTL = d
		}
	}
	// HTTPS_PROXY takes priority over HTTP_PROXY (bgutil parity).
	if v, ok := lookupAny("HTTPS_PROXY", "https_proxy"); ok {
		c.Proxy = v
	} else if v, ok := lookupAny("HTTP_PROXY", "http_proxy"); ok {
		c.Proxy = v
	}
	if v, ok := os.LookupEnv("DISABLE_INNERTUBE"); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			c.DisableInnertube = b
		}
	}
	if v, ok := os.LookupEnv("LOG_LEVEL"); ok {
		c.LogLevel = v
	}
	if v, ok := os.LookupEnv("LOG_FORMAT"); ok {
		c.LogFormat = v
	}
	if v, ok := os.LookupEnv("POT_SERVER_SECRET"); ok {
		c.SharedSecret = v
	}
}

// Validate checks the resolved config for usable values.
func (c *Config) Validate() error {
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("config: invalid port %d", c.Port)
	}
	switch strings.ToLower(c.LogLevel) {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("config: invalid log level %q", c.LogLevel)
	}
	switch strings.ToLower(c.LogFormat) {
	case "text", "json":
	default:
		return fmt.Errorf("config: invalid log format %q", c.LogFormat)
	}
	if c.Proxy != "" {
		if _, err := url.Parse(c.Proxy); err != nil {
			return fmt.Errorf("config: invalid proxy URL %q: %w", c.Proxy, err)
		}
	}
	return nil
}

func setString(dst *string, v *string) {
	if v != nil {
		*dst = *v
	}
}
func setInt(dst *int, v *int) {
	if v != nil {
		*dst = *v
	}
}
func setBool(dst *bool, v *bool) {
	if v != nil {
		*dst = *v
	}
}

func lookupAny(keys ...string) (string, bool) {
	for _, k := range keys {
		if v, ok := os.LookupEnv(k); ok && v != "" {
			return v, true
		}
	}
	return "", false
}

// parseDuration accepts a Go duration ("6h", "30m") or a bare integer of seconds.
func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	return 0, fmt.Errorf("invalid duration %q (want e.g. 6h or 21600)", s)
}
