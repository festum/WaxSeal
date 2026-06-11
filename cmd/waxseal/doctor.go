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
	full    bool
}

func newDoctorCmd() *cobra.Command {
	var o doctorOpts
	c := &cobra.Command{
		Use:   "doctor",
		Short: "Launch a browser, attest, and report the identity + token grade",
		Long: "Diagnostics: launch a real Chromium, run the BotGuard attestation, and report\n" +
			"the captured identity and whether an INTEGRITY token was granted. Exit nonzero\n" +
			"if the browser/IP can't attest, or only a fallback grade is available.\n\n" +
			"With --full, also verify that the browser can stream beyond the ~70s status-2\n" +
			"preview cap. The report includes full_length_probe, and the command exits\n" +
			"nonzero unless the probe verifies full-length streaming.",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          func(cmd *cobra.Command, _ []string) error { return runDoctor(cmd, &o) },
	}
	f := c.Flags()
	f.StringVar(&o.video, "video", browser.DefaultVideo, "landing video for the browser session")
	f.BoolVar(&o.headful, "headful", false, "run headful (needs a display/Xvfb)")
	f.BoolVarP(&o.verbose, "verbose", "v", false, "verbose logging to stderr")
	f.BoolVar(&o.full, "full", false, "verify full-length streaming past the ~70s preview cap (use on demand; the probe seeks and drives playback)")
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

	// Run the optional probe before writing the report so failed and skipped probes
	// are included in the output.
	var probe browser.FullLengthProbe
	var probeErr error
	if o.full {
		probe, probeErr = sess.VerifyFullLength(ctx, o.video)
		report["full_length_probe"] = probe
	}

	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(report)

	if kind != "integrity" {
		fmt.Fprintf(stderr, "WARN: attest grade is %q, not integrity\n", kind)
	}
	if o.full {
		// A successful full-length probe is stronger evidence than the attest grade.
		if probeErr != nil {
			fmt.Fprintln(stderr, "FAIL: full-length probe:", probeErr)
			return probeErr
		}
		if probe.Outcome != browser.OutcomeFullLength {
			fmt.Fprintf(stderr, "WARN: full-length not verified (outcome %q): %s\n", probe.Outcome, probe.Reason)
			return fmt.Errorf("full-length not verified: %s", probe.Outcome)
		}
		return nil
	}
	if kind != "integrity" {
		return fmt.Errorf("no integrity grant")
	}
	return nil
}
