package main

import (
	"testing"

	"github.com/colespringer/waxseal/config"
	"github.com/spf13/cobra"
)

// parseServer builds a server command, binds its flags, and parses args, then
// returns the bound opts + command for applyServerFlags.
func parseServer(t *testing.T, args ...string) (*cobra.Command, *serverOpts) {
	t.Helper()
	var s serverOpts
	c := &cobra.Command{Use: "server", RunE: func(*cobra.Command, []string) error { return nil }}
	bindServerFlags(c, &s)
	if err := c.ParseFlags(args); err != nil {
		t.Fatalf("ParseFlags(%v): %v", args, err)
	}
	return c, &s
}

// TestServerPersistTokensFlagPrecedence checks flags > env: an explicit
// --persist-tokens=false overrides PERSIST_TOKENS=true, and an explicit =true
// overrides a false env, while an unset flag leaves the env/file value intact.
func TestServerPersistTokensFlagPrecedence(t *testing.T) {
	t.Run("explicit false overrides env true", func(t *testing.T) {
		t.Setenv("PERSIST_TOKENS", "true")
		cfg, err := config.Load("")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !cfg.PersistTokens {
			t.Fatal("precondition: env should set PersistTokens true")
		}
		cmd, s := parseServer(t, "--persist-tokens=false")
		applyServerFlags(cmd, s, &cfg)
		if cfg.PersistTokens {
			t.Error("--persist-tokens=false did not override env true")
		}
	})

	t.Run("explicit true overrides env false", func(t *testing.T) {
		t.Setenv("PERSIST_TOKENS", "false")
		cfg, _ := config.Load("")
		cmd, s := parseServer(t, "--persist-tokens=true")
		applyServerFlags(cmd, s, &cfg)
		if !cfg.PersistTokens {
			t.Error("--persist-tokens=true did not override env false")
		}
	})

	t.Run("unset flag leaves env value", func(t *testing.T) {
		t.Setenv("PERSIST_TOKENS", "true")
		cfg, _ := config.Load("")
		cmd, s := parseServer(t) // no flag
		applyServerFlags(cmd, s, &cfg)
		if !cfg.PersistTokens {
			t.Error("unset --persist-tokens should leave the env value (true)")
		}
	})
}

// TestServerEndpointModeFlagOverlay confirms a string flag overlays onto config.
func TestServerEndpointModeFlagOverlay(t *testing.T) {
	cfg := config.Default()
	cfg.EndpointMode = "youtube"
	cmd, s := parseServer(t, "--endpoint-mode=googleapis")
	applyServerFlags(cmd, s, &cfg)
	if cfg.EndpointMode != "googleapis" {
		t.Errorf("endpoint mode = %q, want googleapis (flag over config)", cfg.EndpointMode)
	}
}
