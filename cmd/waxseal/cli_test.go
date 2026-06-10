package main

import (
	"bytes"
	"testing"
)

func TestCommandTree(t *testing.T) {
	root := newRootCmd()
	have := map[string]bool{}
	for _, c := range root.Commands() {
		have[c.Name()] = true
	}
	for _, want := range []string{"server", "doctor", "get-pot", "ping"} {
		if !have[want] {
			t.Errorf("missing subcommand %q", want)
		}
	}
}

// TestGenerateRequiresBinding: the root (generate mode) with no -c prints "{}"
// and errors before ever launching a browser.
func TestGenerateRequiresBinding(t *testing.T) {
	root := newRootCmd()
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetArgs([]string{})
	if err := root.Execute(); err == nil {
		t.Error("expected an error when --content-binding is missing")
	}
	if out.String() != "{}\n" {
		t.Errorf("stdout = %q, want %q", out.String(), "{}\n")
	}
}

func TestBuildLogger(t *testing.T) {
	if buildLogger("debug", &bytes.Buffer{}) == nil {
		t.Error("buildLogger returned nil")
	}
}
