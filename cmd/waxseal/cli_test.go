package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestCommandTree(t *testing.T) {
	root := newRootCmd()
	want := map[string]bool{"server": false, "doctor": false, "get-pot": false}
	for _, c := range root.Commands() {
		if _, ok := want[c.Name()]; ok {
			want[c.Name()] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("missing subcommand %q", name)
		}
	}
	// The root must run generate mode itself (no subcommand required).
	if root.RunE == nil {
		t.Error("root command has no RunE (generate mode)")
	}
	if f := root.Flags().Lookup("content-binding"); f == nil || f.Shorthand != "c" {
		t.Error("root missing -c/--content-binding")
	}
}

func TestDeprecatedFlagsExitWithoutToken(t *testing.T) {
	// The deprecated-parameter check must fire before any client build or
	// network, returning an error and emitting no "{}" token line.
	for _, flag := range []string{"--visitor-data", "--data-sync-id"} {
		root := newRootCmd()
		var out, errb bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&errb)
		root.SetArgs([]string{flag, "x"})

		if err := root.Execute(); err == nil {
			t.Errorf("%s: expected error", flag)
		}
		if !strings.Contains(errb.String(), "deprecated") {
			t.Errorf("%s: stderr = %q", flag, errb.String())
		}
		if strings.Contains(out.String(), "{}") {
			t.Errorf("%s: deprecated usage error should not print {}", flag)
		}
	}
}

func TestHelpers(t *testing.T) {
	if firstNonEmpty("", "b") != "b" || firstNonEmpty("a", "b") != "a" {
		t.Error("firstNonEmpty")
	}
	if serverCacheTTL(0) != 6*time.Hour {
		t.Error("serverCacheTTL default")
	}
	if serverCacheTTL(time.Minute) != time.Minute {
		t.Error("serverCacheTTL passthrough")
	}
	if p := boolPtr(true); p == nil || !*p {
		t.Error("boolPtr")
	}
}

func TestBuildLoggerFormats(t *testing.T) {
	var buf bytes.Buffer
	buildLogger("debug", "json", &buf).Info("hi", "k", "v")
	if !strings.Contains(buf.String(), `"k":"v"`) {
		t.Errorf("json handler output = %q", buf.String())
	}
	buf.Reset()
	buildLogger("info", "text", &buf).Info("hi", "k", "v")
	if !strings.Contains(buf.String(), "k=v") {
		t.Errorf("text handler output = %q", buf.String())
	}
}
