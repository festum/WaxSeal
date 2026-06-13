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

// newPlayerContextCmd is the one-shot counterpart to the daemon's
// /player-context endpoint. It launches a new browser for each call.
func newPlayerContextCmd() *cobra.Command {
	var o playerContextOpts
	c := &cobra.Command{
		Use:   "player-context [video_id]",
		Short: "Launch a browser, attest, and print the status-1 streaming context for a video",
		Long: "Launch a real Chromium browser, attest, load the given video, and print its\n" +
			"status-1 streaming context as JSON. Pass the video ID as the positional\n" +
			"argument or with --video. The response includes the SABR URL, player\n" +
			"URL, visitor data, and audio formats. For a warm daemon, POST to\n" +
			"/player-context instead.",
		Example: "  waxseal player-context aqz-KE-bpKQ\n" +
			"  waxseal player-context --video aqz-KE-bpKQ --headful",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return runPlayerContext(cmd, &o, args) },
	}
	f := c.Flags()
	f.StringVar(&o.video, "video", "", "video ID to fetch (optional when passed as a positional argument)")
	f.BoolVar(&o.headful, "headful", false, "run headful (needs a display/Xvfb)")
	f.BoolVarP(&o.verbose, "verbose", "v", false, "verbose logging to stderr")
	return c
}

// resolvePlayerContextVideoID returns the video ID supplied by either the
// --video flag or the positional argument.
func resolvePlayerContextVideoID(flag string, args []string) (string, error) {
	video := flag
	if len(args) == 1 {
		if video != "" {
			return "", &usageError{msg: "specify the video ID once, either as the positional argument or with --video"}
		}
		video = args[0]
	}
	switch {
	case video == "":
		return "", &usageError{msg: "provide a video ID as the positional argument or with --video"}
	case looksLikeURL(video):
		return "", &usageError{msg: "provide a bare video ID (for example, aqz-KE-bpKQ), not a URL"}
	case !browser.ValidVideoID(video):
		return "", &usageError{msg: "video ID must contain 1 to 64 letters, digits, underscores, or hyphens"}
	}
	return video, nil
}

func runPlayerContext(cmd *cobra.Command, o *playerContextOpts, args []string) error {
	stdout, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
	video, err := resolvePlayerContextVideoID(o.video, args)
	if err != nil {
		return err
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
	// Load the target video before reading its status-1 context.
	sess, err := browser.Launch(ctx, video, browser.Options{Headful: o.headful, NormalizeUA: !o.headful, Logger: logger})
	if err != nil {
		return fmt.Errorf("browser launch/identity: %w", err)
	}
	defer sess.Close()

	if err := sess.Attest(ctx); err != nil {
		return fmt.Errorf("attestation: %w", err)
	}
	pc, err := sess.PlayerContext(ctx, video)
	if err != nil {
		return fmt.Errorf("player-context: %w", err)
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(pc)
}
