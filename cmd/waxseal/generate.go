package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/colespringer/waxseal/internal/browser"
	"github.com/spf13/cobra"
)

// genOpts holds generate-mode flags.
type genOpts struct {
	contentBinding string
	video          string
	headful        bool
	verbose        bool
}

func bindGenerateFlags(cmd *cobra.Command, g *genOpts) {
	f := cmd.Flags()
	f.StringVarP(&g.contentBinding, "content-binding", "c", "", "binding to mint (video_id for player, visitor_data for gvs)")
	f.StringVar(&g.video, "video", browser.DefaultVideo, "landing video for the browser session")
	f.BoolVar(&g.headful, "headful", false, "run headful (needs a display/Xvfb)")
	f.BoolVarP(&g.verbose, "verbose", "v", false, "verbose logging to stderr")
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

// runGenerate launches a browser, attests, mints one token, and prints JSON on
// the last stdout line. On failure it prints "{}" and returns a nonzero exit.
// (One-shot launches a fresh browser each call; for yt-dlp use `waxseal server`.)
func runGenerate(cmd *cobra.Command, g *genOpts) error {
	stdout, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
	if g.contentBinding == "" {
		fmt.Fprintln(stdout, "{}")
		fmt.Fprintln(stderr, "content-binding (-c) is required")
		return fmt.Errorf("missing content-binding")
	}
	level := "error"
	if g.verbose {
		level = "info"
	}
	logger := buildLogger(level, stderr)

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	sess, err := browser.Launch(ctx, g.video, browser.Options{Headful: g.headful, NormalizeUA: !g.headful, Logger: logger})
	if err != nil {
		fmt.Fprintln(stdout, "{}")
		logger.Error("launch", "err", err)
		return err
	}
	defer sess.Close()

	res, err := sess.Mint(ctx, g.contentBinding)
	if err != nil {
		fmt.Fprintln(stdout, "{}")
		logger.Error("mint", "err", err)
		return err
	}
	expires := time.Now().Add(6 * time.Hour)
	if res.Lifetime > 0 {
		expires = time.Now().Add(time.Duration(res.Lifetime) * time.Second)
	}
	return json.NewEncoder(stdout).Encode(map[string]any{
		"poToken":        res.Token,
		"contentBinding": g.contentBinding,
		"expiresAt":      expires.UTC().Format(time.RFC3339),
	})
}
