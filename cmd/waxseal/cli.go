// Command waxseal is the WaxSeal CLI. With no subcommand it runs "generate"
// mode, compatible with bgutil's script provider: JSON is printed on the last
// stdout line. The server subcommand runs the bgutil-compatible HTTP daemon, and
// doctor runs redacted diagnostics by stage. Configuration precedence is
// flags > env > file > defaults.
package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	waxseal "github.com/colespringer/waxseal"
	"github.com/colespringer/waxseal/config"
)

// version is overridable at build time with -ldflags "-X main.version=...".
var version = "dev"

// buildLogger builds a slog logger at the given level and format, writing to w
// (stderr for the CLI, stdout for the daemon).
func buildLogger(level, format string, w io.Writer) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	if strings.ToLower(format) == "json" {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

// buildClient constructs a waxseal.Client that owns transports, so egress flags
// affect the network path. It always uses the wazero compilation cache.
// persistDir, when non-empty, also enables the disk store for breaker cooldowns
// and, when cfg.PersistTokens is set, cached tokens.
func buildClient(cfg config.Config, logger *slog.Logger, persistDir string) (*waxseal.Client, error) {
	return waxseal.New(waxseal.Options{
		EgressTransport:     waxseal.BuildEgressTransport,
		Logger:              logger,
		DisableInnertube:    cfg.DisableInnertube,
		CacheMaxTTL:         cfg.CacheMaxTTL,
		CompilationCacheDir: compilationCacheDir(cfg.CacheDir),
		CacheDir:            persistDir,
		PersistTokens:       cfg.PersistTokens,
		DiskBackend:         cfg.DiskBackend,
		EndpointMode:        cfg.EndpointMode,
	})
}

// compilationCacheDir returns the configured CACHE_DIR, or a per-user cache path
// when one is available.
func compilationCacheDir(configured string) string {
	if configured != "" {
		return configured
	}
	if base, err := os.UserCacheDir(); err == nil {
		return filepath.Join(base, "waxseal")
	}
	return ""
}

// defaultEgress builds the baseline egress from resolved config (proxy/source/TLS).
func defaultEgress(cfg config.Config) waxseal.EgressSpec {
	spec := waxseal.EgressSpec{Proxy: cfg.Proxy, SourceAddress: cfg.SourceAddress, DisableTLSVerify: cfg.DisableTLSVerify}
	spec.ID = spec.DerivedID()
	return spec
}

// serverCacheTTL applies a conservative cap to the opaque server cache when none
// is configured. A content_binding may be a per-player video token.
func serverCacheTTL(d time.Duration) time.Duration {
	if d <= 0 {
		return 6 * time.Hour
	}
	return d
}

func boolPtr(b bool) *bool { return &b }
