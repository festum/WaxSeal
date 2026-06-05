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
	f := c.Flags()
	f.StringVar(&s.host, "host", "", "bind address (default 127.0.0.1; set :: or 0.0.0.0 to expose)")
	f.IntVar(&s.port, "port", 0, "listen port (default 4416)")
	f.StringVar(&s.configPath, "config", "", "path to a JSON config file")
	f.BoolVarP(&s.verbose, "verbose", "v", false, "verbose (debug) logging")
	f.BoolVar(&s.allowEgressOverride, "allow-request-egress-override", false,
		"honor per-request proxy/source_address/disable_tls_verification; off by default")
	return c
}

func runServer(cmd *cobra.Command, s *serverOpts) error {
	cfg, err := config.Load(s.configPath)
	if err != nil {
		return err
	}
	// Overlay explicitly-changed flags (highest precedence).
	if cmd.Flags().Changed("host") {
		cfg.Host = s.host
	}
	if cmd.Flags().Changed("port") {
		cfg.Port = s.port
	}
	if s.verbose {
		cfg.LogLevel = "debug"
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	cfg.CacheMaxTTL = serverCacheTTL(cfg.CacheMaxTTL)

	logger := buildLogger(cfg.LogLevel, cfg.LogFormat, os.Stdout)
	client, err := buildClient(cfg, logger)
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
