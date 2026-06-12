package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/colespringer/waxseal/internal/browser"
	"github.com/spf13/cobra"
)

// playerContextOpts holds player-context-subcommand flags.
type playerContextOpts struct {
	video   string
	headful bool
	verbose bool
}

// newPlayerContextCmd is the one-shot counterpart to the daemon's /player-context
// endpoint (like generate/doctor: it launches its own browser). For a warm daemon,
// POST /player-context or use client.PlayerContext.
func newPlayerContextCmd() *cobra.Command {
	var o playerContextOpts
	c := &cobra.Command{
		Use:   "player-context",
		Short: "Launch a browser, attest, and print a video's status-1 streaming context",
		Long: "Diagnostics/one-shot: launch a real Chromium, attest, point the player at\n" +
			"--video, and print its status-1 streaming context (server_abr_streaming_url +\n" +
			"player_url + visitor_data + audio formats) as JSON. For a warm daemon, POST\n" +
			"/player-context instead; a fresh browser per call is slow.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runPlayerContext(cmd, &o) },
	}
	f := c.Flags()
	f.StringVar(&o.video, "video", "", "video_id to fetch the streaming context for (required)")
	f.BoolVar(&o.headful, "headful", false, "run headful (needs a display/Xvfb)")
	f.BoolVarP(&o.verbose, "verbose", "v", false, "verbose logging to stderr")
	return c
}

func runPlayerContext(cmd *cobra.Command, o *playerContextOpts) error {
	stdout, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
	if o.video == "" {
		return &usageError{msg: "--video (the video_id) is required"}
	}
	if looksLikeURL(o.video) {
		return &usageError{msg: "--video expects a video ID (e.g. dQw4w9WgXcQ), not a URL"}
	}
	level := "warn"
	if o.verbose {
		level = "info"
	}
	logger := buildLogger(level, stderr)

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	// Park the session on the target video, then read its own status-1 context.
	sess, err := browser.Launch(ctx, o.video, browser.Options{Headful: o.headful, NormalizeUA: !o.headful, Logger: logger})
	if err != nil {
		return fmt.Errorf("browser launch/identity: %w", err)
	}
	defer sess.Close()

	if err := sess.Attest(ctx); err != nil {
		return fmt.Errorf("attestation: %w", err)
	}
	pc, err := sess.PlayerContext(ctx, o.video)
	if err != nil {
		return fmt.Errorf("player-context: %w", err)
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(pc)
}
