package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	waxseal "github.com/colespringer/waxseal"
	"github.com/colespringer/waxseal/config"
	"github.com/spf13/cobra"
)

// genOpts holds generate-mode flags.
type genOpts struct {
	contentBinding   string
	proxy            string
	sourceAddress    string
	bypassCache      bool
	disableTLS       bool
	disableInnertube bool
	configPath       string
	verbose          bool

	visitorData string // deprecated
	dataSyncID  string // deprecated
}

// bindGenerateFlags registers generate-mode flags on cmd. It is called for both
// the root command and the get-pot alias.
func bindGenerateFlags(cmd *cobra.Command, g *genOpts) {
	f := cmd.Flags()
	f.StringVarP(&g.contentBinding, "content-binding", "c", "", "opaque content binding to mint (video ID or visitor data)")
	f.StringVar(&g.proxy, "proxy", "", "egress proxy URL")
	f.StringVar(&g.sourceAddress, "source-address", "", "egress source IP to bind")
	f.BoolVar(&g.bypassCache, "bypass-cache", false, "force a fresh token (skip token cache)")
	f.BoolVar(&g.disableTLS, "disable-tls-verification", false, "skip TLS verification on egress (discouraged)")
	f.BoolVar(&g.disableInnertube, "disable-innertube", false, "skip InnerTube att/get; use the Create endpoint directly")
	f.StringVar(&g.configPath, "config", "", "path to a JSON config file")
	f.BoolVarP(&g.verbose, "verbose", "v", false, "verbose logging to stderr")

	f.StringVar(&g.visitorData, "visitor-data", "", "deprecated; use --content-binding")
	f.StringVar(&g.dataSyncID, "data-sync-id", "", "deprecated; use --content-binding")
	_ = f.MarkHidden("visitor-data")
	_ = f.MarkHidden("data-sync-id")
}

// newGetPotCmd is an explicit alias for the default generate command.
func newGetPotCmd() *cobra.Command {
	var g genOpts
	c := &cobra.Command{
		Use:           "get-pot",
		Short:         "Generate a PO token (alias for the default command)",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          func(cmd *cobra.Command, _ []string) error { return runGenerate(cmd, &g) },
	}
	bindGenerateFlags(c, &g)
	return c
}

// runGenerate mints one token and prints JSON on the last stdout line. On
// failure it prints "{}" and returns a nonzero exit.
func runGenerate(cmd *cobra.Command, g *genOpts) error {
	stdout := cmd.OutOrStdout()
	stderr := cmd.ErrOrStderr()

	// Deprecated parameters are a usage error (no token, no "{}"), matching bgutil.
	if g.visitorData != "" || g.dataSyncID != "" {
		fmt.Fprintln(stderr, "visitor_data and data_sync_id are deprecated; use --content-binding")
		return fmt.Errorf("deprecated parameter")
	}

	level := "error"
	if g.verbose {
		level = "debug"
	}
	logger := buildLogger(level, "text", stderr)

	cfg, err := config.Load(g.configPath)
	if err != nil {
		fmt.Fprintln(stdout, "{}")
		logger.Error("config", "err", err)
		return err
	}

	// One-shot CLI calls use disk persistence only when CACHE_DIR is configured;
	// this avoids competing for a store lock across concurrent invocations.
	client, err := buildClient(cfg, logger, cfg.CacheDir, buildClientOpts{})
	if err != nil {
		fmt.Fprintln(stdout, "{}")
		logger.Error("init", "err", err)
		return err
	}
	defer client.Close()

	egress := waxseal.EgressSpec{
		Proxy:            firstNonEmpty(g.proxy, cfg.Proxy),
		SourceAddress:    firstNonEmpty(g.sourceAddress, cfg.SourceAddress),
		DisableTLSVerify: g.disableTLS || cfg.DisableTLSVerify,
	}
	egress.ID = egress.DerivedID()

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	binding := g.contentBinding
	// Only a binding sourced here is known to be visitor_data and safe to use as
	// the att/get session anchor.
	var sessionVisitorData string
	if binding == "" {
		vd, err := client.VisitorData(ctx, egress)
		if err != nil {
			fmt.Fprintln(stdout, "{}")
			logger.Error("visitor_data", "err", err)
			return err
		}
		binding = vd
		sessionVisitorData = vd
	}

	tok, err := client.Token(ctx, waxseal.Request{
		Scope:            waxseal.ScopeOpaque,
		Identifier:       binding,
		VisitorData:      sessionVisitorData,
		Egress:           egress,
		BypassCache:      g.bypassCache,
		DisableInnertube: boolPtr(g.disableInnertube || cfg.DisableInnertube),
	})
	if err != nil {
		fmt.Fprintln(stdout, "{}")
		logger.Error("generate failed", "err", err)
		return err
	}

	expires := tok.ExpiresAt
	if expires.IsZero() {
		expires = time.Now().Add(time.Hour)
	}
	enc := json.NewEncoder(stdout)
	if err := enc.Encode(map[string]any{
		"poToken":        tok.Value,
		"contentBinding": binding,
		"expiresAt":      expires.UTC().Format(time.RFC3339),
	}); err != nil {
		return err
	}
	return nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
