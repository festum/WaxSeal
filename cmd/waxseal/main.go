// Command waxseal is the WaxSeal CLI. It mints YouTube PO tokens from a real
// headless Chromium (BotGuard in the actual browser). With no subcommand it runs
// generate mode, compatible with bgutil's script provider (JSON on the last
// stdout line). For yt-dlp, prefer `waxseal server`; a warm browser mints fast.
package main

import (
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// version is overridable at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

// newRootCmd assembles the command tree. The root runs generate mode, so
// `waxseal -c <binding>` works with no subcommand.
func newRootCmd() *cobra.Command {
	var g genOpts
	root := &cobra.Command{
		Use:   "waxseal",
		Short: "Real-browser YouTube PO Token (POT) provider",
		Long: "WaxSeal mints YouTube PO tokens from a real headless Chromium (BotGuard in the\n" +
			"actual browser). With no subcommand it runs generate mode, compatible with\n" +
			"bgutil's script provider: it prints the token as JSON on the last stdout line,\n" +
			"or {} and a nonzero exit on failure. For yt-dlp, prefer `waxseal server`.",
		Version:       version,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          func(cmd *cobra.Command, _ []string) error { return runGenerate(cmd, &g) },
	}
	bindGenerateFlags(root, &g)
	root.AddCommand(newServerCmd(), newDoctorCmd(), newGetPotCmd(), newPingCmd())
	return root
}

// buildLogger builds a slog logger at the given level, writing to w (stderr for
// the CLI, stdout for the daemon).
func buildLogger(level string, w io.Writer) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: lvl}))
}
