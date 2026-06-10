package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/spf13/cobra"
)

// pingOpts holds ping-subcommand flags.
type pingOpts struct {
	addr string
	key  string
}

// newPingCmd checks a running server with GET /ping and exits nonzero on failure.
// It is a curl-free probe for scripts, systemd, and container health checks.
func newPingCmd() *cobra.Command {
	var p pingOpts
	c := &cobra.Command{
		Use:           "ping",
		Short:         "Health-check a running waxseal server (exit 0 if healthy)",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          func(cmd *cobra.Command, _ []string) error { return runPing(cmd, &p) },
	}
	f := c.Flags()
	f.StringVar(&p.addr, "addr", "127.0.0.1:4416", "server address to connect to")
	f.StringVar(&p.key, "key", "", "tenant API key (required if the server is multi-tenant)")
	return c
}

func runPing(cmd *cobra.Command, p *pingOpts) error {
	stdout, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
	u := "http://" + p.addr + "/ping"
	if p.key != "" {
		u += "?key=" + url.QueryEscape(p.key)
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 100*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(stderr, "unreachable:", err)
		return err
	}
	defer resp.Body.Close()
	var body struct {
		OK     bool   `json:"ok"`
		Attest string `json:"attest"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if resp.StatusCode != http.StatusOK || !body.OK {
		fmt.Fprintf(stderr, "unhealthy: status=%d ok=%v\n", resp.StatusCode, body.OK)
		return fmt.Errorf("unhealthy")
	}
	fmt.Fprintf(stdout, "ok (attest=%s)\n", body.Attest)
	return nil
}
