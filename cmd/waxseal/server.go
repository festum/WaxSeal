package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	waxseal "github.com/colespringer/waxseal"
	"github.com/colespringer/waxseal/config"
	"github.com/colespringer/waxseal/server"
	"github.com/spf13/cobra"
)

// serverOpts holds server-subcommand flags.
type serverOpts struct {
	host                string
	port                int
	configPath          string
	verbose             bool
	allowEgressOverride bool
	cacheDir            string
	persistTokens       bool
	diskBackend         string
	endpointMode        string
}

func newServerCmd() *cobra.Command {
	var s serverOpts
	c := &cobra.Command{
		Use:   "server",
		Short: "Run the bgutil-compatible HTTP daemon",
		Long: "Run the HTTP daemon. It defaults to loopback (127.0.0.1:4416); set --host :: or\n" +
			"0.0.0.0 (or POT_SERVER_HOST) to expose it on a private network. Use a shared\n" +
			"secret (POT_SERVER_SECRET) when exposing it. It drains in-flight requests on SIGTERM/SIGINT.",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          func(cmd *cobra.Command, _ []string) error { return runServer(cmd, &s) },
	}
	bindServerFlags(c, &s)
	return c
}

// bindServerFlags registers the server-subcommand flags on cmd. It is separated
// from newServerCmd so tests can build a command and exercise applyServerFlags.
func bindServerFlags(c *cobra.Command, s *serverOpts) {
	f := c.Flags()
	f.StringVar(&s.host, "host", "", "bind address (default 127.0.0.1; set :: or 0.0.0.0 to expose)")
	f.IntVar(&s.port, "port", 0, "listen port (default 4416)")
	f.StringVar(&s.configPath, "config", "", "path to a JSON config file")
	f.BoolVarP(&s.verbose, "verbose", "v", false, "verbose (debug) logging")
	f.BoolVar(&s.allowEgressOverride, "allow-request-egress-override", false,
		"honor per-request proxy/source_address/disable_tls_verification; off by default")
	f.StringVar(&s.cacheDir, "cache-dir", "", "directory for the wazero AOT cache and persistent store (default per-user cache)")
	f.BoolVar(&s.persistTokens, "persist-tokens", false, "persist minted tokens to disk across restarts (off by default; tokens are sensitive)")
	f.StringVar(&s.diskBackend, "disk-backend", "", "persistent store backend: bbolt (default) or json")
	f.StringVar(&s.endpointMode, "endpoint-mode", "", "WAA attestation host: youtube (default) or googleapis")
}

// applyServerFlags overlays explicitly-set flags onto cfg (highest precedence,
// flags > env > file). It honors --flag=false for booleans by gating on
// Flags().Changed rather than the value, so an explicit disable wins over a true
// from env/file.
func applyServerFlags(cmd *cobra.Command, s *serverOpts, cfg *config.Config) {
	changed := cmd.Flags().Changed
	if changed("host") {
		cfg.Host = s.host
	}
	if changed("port") {
		cfg.Port = s.port
	}
	if s.verbose {
		cfg.LogLevel = "debug"
	}
	if changed("cache-dir") {
		cfg.CacheDir = s.cacheDir
	}
	if changed("persist-tokens") {
		cfg.PersistTokens = s.persistTokens
	}
	if changed("disk-backend") {
		cfg.DiskBackend = s.diskBackend
	}
	if changed("endpoint-mode") {
		cfg.EndpointMode = s.endpointMode
	}
}

func runServer(cmd *cobra.Command, s *serverOpts) error {
	cfg, err := config.Load(s.configPath)
	if err != nil {
		return err
	}
	applyServerFlags(cmd, s, &cfg)
	if err := cfg.Validate(); err != nil {
		return err
	}
	cfg.CacheMaxTTL = serverCacheTTL(cfg.CacheMaxTTL)

	logger := buildLogger(cfg.LogLevel, cfg.LogFormat, os.Stdout)
	// The daemon is long-running and single-process, so it uses the resolved
	// cache directory for breaker persistence by default.
	client, err := buildClient(cfg, logger, compilationCacheDir(cfg.CacheDir), buildClientOpts{})
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	egress := defaultEgress(cfg)
	// Pre-warm the default egress so the first request skips the cold snapshot.
	client.Prewarm(waxseal.Request{Scope: waxseal.ScopeOpaque, Egress: egress})

	srv := server.New(client, server.Options{
		Host:                       cfg.Host,
		Port:                       cfg.Port,
		SharedSecret:               cfg.SharedSecret,
		AllowRequestEgressOverride: cfg.AllowRequestEgressOverride || s.allowEgressOverride,
		DefaultEgress:              egress,
		Version:                    version,
		Logger:                     logger,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return srv.ListenAndServe(ctx)
}
