package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	waxseal "github.com/colespringer/waxseal"
	"github.com/colespringer/waxseal/config"
	"github.com/colespringer/waxseal/internal/botguard"
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
	// This best-effort check uses direct egress and does not affect attestation.
	checkChromeFreshness(cmd.Context(), stdout)
	// Capture missing browser APIs while minting a real token.
	driftBuf := &bytes.Buffer{}
	client, err := buildClient(cfg, logger, cfg.CacheDir, buildClientOpts{Discovery: true, DiscoverySink: driftBuf})
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
	kind := tok.Kind
	if kind == "" {
		kind = "unknown"
	}
	report(stdout, "mint", true, fmt.Sprintf("token issued (%s, %d bytes, expires %s)",
		kind, len(tok.Value), tok.ExpiresAt.UTC().Format(time.RFC3339)))

	// Report whether a fallback coincided with a missing browser API.
	switch drift := botguard.DriftProbes(driftBuf.String()); {
	case len(drift) > 0:
		reportLine(stdout, "warn", "shim coverage", fmt.Sprintf(
			"%d missing browser APIs: %s; implement them in build/js/dom.js and build/js/shim.js, then run make jsbundle",
			len(drift), strings.Join(drift, ", ")))
	case tok.Kind == "fallback":
		reportLine(stdout, "warn", "attestation",
			"fallback token with no API drift; retry with different egress to distinguish session and IP reputation issues")
	default:
		report(stdout, "shim coverage", true, "no API drift (integrity mint)")
	}

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
	reportLine(w, mark, stage, detail)
}

// reportLine prints one "[mark] stage: detail" line.
func reportLine(w io.Writer, mark, stage, detail string) {
	fmt.Fprintf(w, "[%s] %s: %s\n", mark, stage, detail)
}

// redactTag returns a short non-reversible tag for a sensitive value, so the
// report confirms presence without printing visitor_data.
func redactTag(s string) string {
	h := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(h[:])[:12]
}

// chromeVersionAPI is Google's public Chrome version history feed.
const chromeVersionAPI = "https://versionhistory.googleapis.com/v1/chrome/platforms/win/channels/stable/versions"

// chromeStaleGap is how many major versions the default profile may trail stable
// before doctor warns.
const chromeStaleGap = 6

// checkChromeFreshness compares the default profile with the current stable
// Chrome major. The check is advisory; a fetch error does not fail doctor.
func checkChromeFreshness(ctx context.Context, w io.Writer) {
	prof := waxseal.ChromeMajor(waxseal.DefaultProfile().UserAgent)
	if prof == 0 {
		return // non-Chrome default profile: nothing to compare
	}
	fctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	cur, err := currentStableChromeMajor(fctx)
	if err != nil {
		reportLine(w, "warn", "chrome version", fmt.Sprintf("profile claims Chrome %d; stable-version check skipped (%v)", prof, err))
		return
	}
	switch gap := cur - prof; {
	case gap >= chromeStaleGap:
		reportLine(w, "warn", "chrome version", fmt.Sprintf(
			"profile Chrome %d is %d behind current stable %d; update chrome_version.json and run make jsbundle if integrity degrades", prof, gap, cur))
	case gap < 0:
		report(w, "chrome version", true, fmt.Sprintf("profile Chrome %d (newer than current stable %d)", prof, cur))
	default:
		report(w, "chrome version", true, fmt.Sprintf("profile Chrome %d, current stable %d (%d behind)", prof, cur, gap))
	}
}

// currentStableChromeMajor fetches the latest stable Chrome major from the public
// Version History API.
func currentStableChromeMajor(ctx context.Context) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, chromeVersionAPI, nil)
	if err != nil {
		return 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("status %d", resp.StatusCode)
	}
	var body struct {
		Versions []struct {
			Version string `json:"version"`
		} `json:"versions"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return 0, err
	}
	if len(body.Versions) == 0 {
		return 0, fmt.Errorf("no versions returned")
	}
	if m := leadingInt(body.Versions[0].Version); m > 0 {
		return m, nil
	}
	return 0, fmt.Errorf("unparseable version %q", body.Versions[0].Version)
}

// leadingInt returns the leading decimal integer in s, or 0 if s starts with no
// digits.
func leadingInt(s string) int {
	n := 0
	for i := 0; i < len(s) && s[i] >= '0' && s[i] <= '9'; i++ {
		n = n*10 + int(s[i]-'0')
	}
	return n
}
