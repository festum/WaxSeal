package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/colespringer/waxseal/internal/browser"
	"github.com/spf13/cobra"
)

// doctorOpts holds doctor-subcommand flags.
type doctorOpts struct {
	video   string
	headful bool
	verbose bool
}

func newDoctorCmd() *cobra.Command {
	var o doctorOpts
	c := &cobra.Command{
		Use:   "doctor",
		Short: "Launch a browser, attest, and report the identity + token grade",
		Long: "Diagnostics: launch a real Chromium, run the BotGuard attestation, and report\n" +
			"the captured identity and whether an INTEGRITY token was granted. Exit nonzero\n" +
			"if the browser/IP can't attest, or only a fallback grade is available.",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          func(cmd *cobra.Command, _ []string) error { return runDoctor(cmd, &o) },
	}
	f := c.Flags()
	f.StringVar(&o.video, "video", browser.DefaultVideo, "landing video for the browser session")
	f.BoolVar(&o.headful, "headful", false, "run headful (needs a display/Xvfb)")
	f.BoolVarP(&o.verbose, "verbose", "v", false, "verbose logging to stderr")
	return c
}

func runDoctor(cmd *cobra.Command, o *doctorOpts) error {
	stdout, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
	level := "warn"
	if o.verbose {
		level = "info"
	}
	logger := buildLogger(level, stderr)

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	sess, err := browser.Launch(ctx, o.video, browser.Options{Headful: o.headful, NormalizeUA: !o.headful, Logger: logger})
	if err != nil {
		fmt.Fprintln(stderr, "FAIL: browser launch/identity:", err)
		return err
	}
	defer sess.Close()

	if err := sess.Attest(ctx); err != nil {
		fmt.Fprintln(stderr, "FAIL: attestation:", err)
		return err
	}
	kind := sess.AttestKind()
	report := map[string]any{
		"attest":   kind,
		"identity": sess.Identity(),
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(report)

	if kind != "integrity" {
		fmt.Fprintf(stderr, "WARN: attest grade is %q, not integrity\n", kind)
		return fmt.Errorf("no integrity grant")
	}
	return nil
}
