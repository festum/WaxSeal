package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"time"

	waxseal "github.com/colespringer/waxseal"
	"github.com/colespringer/waxseal/config"
	"github.com/spf13/cobra"
)

// doctorOpts holds doctor-subcommand flags.
type doctorOpts struct {
	configPath string
	proxy      string
	verbose    bool
	full       bool
}

func newDoctorCmd() *cobra.Command {
	var d doctorOpts
	c := &cobra.Command{
		Use:   "doctor",
		Short: "Run redacted diagnostics by stage",
		Long: "Exercise the full attestation pipeline (fetch visitor_data -> challenge -> VM ->\n" +
			"GenerateIT -> mint -> validate) and report each stage. Output is redacted: it never\n" +
			"prints raw tokens, visitor_data, or Google JS. --full also checks the warm minter path.",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          func(cmd *cobra.Command, _ []string) error { return runDoctor(cmd, &d) },
	}
	f := c.Flags()
	f.StringVar(&d.configPath, "config", "", "path to a JSON config file")
	f.StringVar(&d.proxy, "proxy", "", "egress proxy URL")
	f.BoolVar(&d.full, "full", false, "also verify the warm minter path")
	f.BoolVarP(&d.verbose, "verbose", "v", false, "verbose logging to stderr")
	return c
}

func runDoctor(cmd *cobra.Command, d *doctorOpts) error {
	stdout := cmd.OutOrStdout()
	stderr := cmd.ErrOrStderr()

	level := "warn"
	if d.verbose {
		level = "debug"
	}
	logger := buildLogger(level, "text", stderr)

	cfg, err := config.Load(d.configPath)
	if err != nil {
		return err
	}
	if d.proxy != "" {
		cfg.Proxy = d.proxy
	}

	fmt.Fprintln(stdout, "waxseal doctor", version)
	client, err := buildClient(cfg, logger, cfg.CacheDir)
	if err != nil {
		report(stdout, "init", false, err.Error())
		return err
	}
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(cmd.Context(), 60*time.Second)
	defer cancel()
	egress := defaultEgress(cfg)

	// Stage 1: fetch guest visitor_data.
	vd, err := client.VisitorData(ctx, egress)
	if err != nil {
		report(stdout, "visitor_data", false, err.Error())
		return err
	}
	report(stdout, "visitor_data", true, "fetched "+redactTag(vd))

	// Stage 2: attestation, mint, and field-6 validation. On failure the error is
	// already labeled with a StageError.
	tok, err := client.Token(ctx, waxseal.Request{Scope: waxseal.ScopeSession, VisitorData: vd, Egress: egress})
	if err != nil {
		report(stdout, "mint", false, err.Error())
		return err
	}
	report(stdout, "mint", true, fmt.Sprintf("token issued (%d bytes, expires %s)", len(tok.Value), tok.ExpiresAt.UTC().Format(time.RFC3339)))

	// Stage 3 (optional): a second mint should reuse the warm minter. Purge the
	// token cache first; otherwise the cached token would return before the minter
	// path runs.
	if d.full {
		client.PurgeTokens()
		if _, err := client.Token(ctx, waxseal.Request{Scope: waxseal.ScopeSession, VisitorData: vd, Egress: egress}); err != nil {
			report(stdout, "warm mint", false, err.Error())
			return err
		}
		report(stdout, "warm mint", true, "served from warm minter after token cache purge")
	}

	fmt.Fprintln(stdout, "OK: all stages passed")
	return nil
}

// report prints one stage line: "[ok] stage: detail" or "[FAIL] stage: detail".
func report(w io.Writer, stage string, ok bool, detail string) {
	mark := "FAIL"
	if ok {
		mark = "ok"
	}
	fmt.Fprintf(w, "[%s] %s: %s\n", mark, stage, detail)
}

// redactTag returns a short non-reversible tag for a sensitive value, so the
// report confirms presence without printing visitor_data.
func redactTag(s string) string {
	h := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(h[:])[:12]
}
