package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Bad --addr values should be usage errors (exit 2), not nil-request panics. URL
// input is rejected before request construction; the rest fail URL parsing.
func TestPingCLIInvalidAddr(t *testing.T) {
	for _, addr := range []string{
		"http://127.0.0.1:4416", // URL instead of host:port
		"127.0.0.1",             // bare host, no port (would otherwise dial :80)
		"host with space:80",    // space in authority
		"[::1",                  // unbalanced bracket
		"%zz",                   // bad percent-escape
	} {
		t.Run(addr, func(t *testing.T) {
			c := newPingCmd()
			c.SetArgs([]string{"--addr", addr})
			c.SetOut(io.Discard)
			c.SetErr(io.Discard)
			err := c.Execute()
			if err == nil {
				t.Fatalf("addr %q: want an error, got nil", addr)
			}
			if _, ok := errors.AsType[*usageError](err); !ok {
				t.Fatalf("addr %q: error %v is not a *usageError", addr, err)
			}
			if got := exitCodeFor(err); got != 2 {
				t.Errorf("addr %q: exit code = %d, want 2", addr, got)
			}
		})
	}
}

// TestPingCLIStrict verifies the exit semantics of `waxseal ping` with and
// without --strict against canned /ping responses. Without --strict a live
// session (ok:true) is required; with --strict the CLI defers to the server's
// status code, so the benign no-session window (HTTP 200) is healthy and only a
// real probe failure (non-200) fails.
func TestPingCLIStrict(t *testing.T) {
	var status int
	var payload string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, payload)
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	run := func(strict bool) error {
		c := newPingCmd()
		args := []string{"--addr", addr}
		if strict {
			args = append(args, "--strict")
		}
		c.SetArgs(args)
		c.SetOut(io.Discard)
		c.SetErr(io.Discard)
		return c.Execute()
	}

	// Healthy: both modes succeed.
	status, payload = http.StatusOK, `{"ok":true,"attest":"integrity","reason":"ok"}`
	if err := run(false); err != nil {
		t.Errorf("healthy non-strict: %v, want success", err)
	}
	if err := run(true); err != nil {
		t.Errorf("healthy strict: %v, want success", err)
	}

	// Benign no-session (HTTP 200, ok:false): non-strict reports not-ready, strict
	// treats it as healthy so a liveness probe does not flap.
	status, payload = http.StatusOK, `{"ok":false,"reason":"no-session"}`
	if err := run(false); err == nil {
		t.Error("no-session non-strict: want error (no live session)")
	}
	if err := run(true); err != nil {
		t.Errorf("no-session strict: %v, want success (benign window)", err)
	}

	// Real probe failure: a strict-aware daemon maps it to 503; both modes fail.
	status, payload = http.StatusServiceUnavailable, `{"ok":false,"reason":"probe-failed","error":"cdp closed"}`
	if err := run(true); err == nil {
		t.Error("probe-failed strict (503): want error")
	}
	status, payload = http.StatusOK, `{"ok":false,"reason":"probe-failed","error":"cdp closed"}`
	if err := run(false); err == nil {
		t.Error("probe-failed non-strict: want error")
	}

	// A pre-strict daemon ignores ?strict and returns 200 {"ok":false} for a probe
	// failure. --strict must still flag it rather than trusting the 200 alone.
	status, payload = http.StatusOK, `{"ok":false,"reason":"probe-failed","error":"cdp closed"}`
	if err := run(true); err == nil {
		t.Error("probe-failed strict (200 body): want error (must not mask an unhealthy target)")
	}

	// A non-WaxSeal service returning a bare 200 at /ping has no ok field; strict
	// mode must not report it healthy.
	status, payload = http.StatusOK, `{}`
	if err := run(true); err == nil {
		t.Error("empty 200 body strict: want error")
	}
}
