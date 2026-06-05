package main

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/colespringer/waxseal/config"
	"github.com/spf13/cobra"
)

// pingOpts holds ping-subcommand flags.
type pingOpts struct {
	host       string
	port       int
	configPath string
}

// newPingCmd checks a running server with GET /ping and exits nonzero on
// failure. It gives scripts, systemd, and container health checks a curl-free
// probe.
func newPingCmd() *cobra.Command {
	var p pingOpts
	c := &cobra.Command{
		Use:   "ping",
		Short: "Health-check a running waxseal server (exit 0 if healthy)",
		Long: "GET /ping against a running daemon and exit 0 when it responds 200. It connects\n" +
			"to loopback by default (the server's bind address is not a connect address), so it\n" +
			"is intended for scripts, systemd, and container health checks.",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          func(cmd *cobra.Command, _ []string) error { return runPing(cmd, &p) },
	}
	f := c.Flags()
	f.StringVar(&p.host, "host", "", "server host to connect to (default 127.0.0.1)")
	f.IntVar(&p.port, "port", 0, "server port (default from POT_SERVER_PORT or 4416)")
	f.StringVar(&p.configPath, "config", "", "path to a JSON config file (for the port)")
	return c
}

func runPing(cmd *cobra.Command, p *pingOpts) error {
	cfg, err := config.Load(p.configPath)
	if err != nil {
		return err
	}
	// The server binds POT_SERVER_HOST (often :: / 0.0.0.0), which is not a usable
	// connect target; default to loopback and take only the port from config.
	host := "127.0.0.1"
	if p.host != "" {
		host = p.host
	}
	port := cfg.Port
	if cmd.Flags().Changed("port") {
		port = p.port
	}

	url := "http://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/ping"
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("ping %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ping %s: status %d", url, resp.StatusCode)
	}
	fmt.Fprintln(cmd.OutOrStdout(), "ok")
	return nil
}
