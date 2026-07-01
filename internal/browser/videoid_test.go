package browser

import (
	"strings"
	"testing"
)

func TestValidVideoID(t *testing.T) {
	valid := []string{
		"exampleVid1",
		"aqz-KE-bpKQ",
		"a",
		"_",
		"a-b",
		strings.Repeat("a", 64),
	}
	for _, s := range valid {
		if !ValidVideoID(s) {
			t.Errorf("ValidVideoID(%q) = false, want true", s)
		}
	}

	invalid := []string{
		"",
		strings.Repeat("a", 65),
		"a b",
		"a!b",
		"a/b",
		"http://x",
		"abc\n",
	}
	for _, s := range invalid {
		if ValidVideoID(s) {
			t.Errorf("ValidVideoID(%q) = true, want false", s)
		}
	}

	// DefaultVideo must satisfy the same rule as caller-supplied video IDs.
	if !ValidVideoID(DefaultVideo) {
		t.Errorf("ValidVideoID(DefaultVideo=%q) = false, want true", DefaultVideo)
	}
}

func TestLooksLikeWatchURL(t *testing.T) {
	// Scheme forms and scheme-less youtube.com/youtu.be pastes are links.
	links := []string{
		"http://youtube.com",
		"https://youtu.be/x",
		"https://www.youtube.com/watch?v=aqz-KE-bpKQ",
		"ftp://h",
		"a://b",
		"youtube.com/watch?v=aqz-KE-bpKQ", // scheme-less paste
		"www.youtu.be/aqz-KE-bpKQ",
		"YouTube.com/watch?v=x", // domain match is case-insensitive
	}
	for _, s := range links {
		if !LooksLikeWatchURL(s) {
			t.Errorf("LooksLikeWatchURL(%q) = false, want true", s)
		}
	}
	// Bare IDs and visitor_data-like strings are not links.
	bare := []string{
		"exampleVid1",
		"aqz-KE-bpKQ",
		"",
		"abc123",
		"CgtHQVZQX1lEMUJ3ayiIyLtBjIKCgJVUxIEGgAgVw", // representative visitor_data
	}
	for _, s := range bare {
		if LooksLikeWatchURL(s) {
			t.Errorf("LooksLikeWatchURL(%q) = true, want false", s)
		}
	}
}

func TestURLBindingWarningFor(t *testing.T) {
	// A URL-shaped value warns with the field name prepended to the shared body.
	msg, warn := URLBindingWarningFor("content_binding", "https://youtube.com/watch?v=x")
	if !warn {
		t.Fatal("URL binding: warn = false, want true")
	}
	if !strings.HasPrefix(msg, "content_binding ") || !strings.Contains(msg, URLBindingWarning) {
		t.Errorf("URL binding: msg = %q, want the field-prefixed shared warning", msg)
	}
	// The field name is the caller's; a different field flows through unchanged.
	if msg, _ := URLBindingWarningFor("content-binding", "youtu.be/x"); !strings.HasPrefix(msg, "content-binding ") {
		t.Errorf("field prefix = %q, want it to start with the caller's field name", msg)
	}
	// A bare identifier neither warns nor produces a message.
	if msg, warn := URLBindingWarningFor("content_binding", "aqz-KE-bpKQ"); warn || msg != "" {
		t.Errorf("bare ID: (%q, %v), want (\"\", false)", msg, warn)
	}
}

func TestHasControlChars(t *testing.T) {
	withControl := []string{
		"abc\n",    // newline
		"a\tb",     // tab
		"a\rb",     // carriage return
		"a\x00b",   // NUL
		"a\x7fb",   // DEL
		"a\u0085b", // C1 NEL, covering cases a byte scan would miss
	}
	for _, s := range withControl {
		if !HasControlChars(s) {
			t.Errorf("HasControlChars(%q) = false, want true", s)
		}
	}

	clean := []string{
		"aqz-KE-bpKQ", // a bare video_id (player binding)
		"CgtHQVZQX1lEMUJ3ayiIyLtBjIKCgJVUxIEGgAgVw", // representative base64url visitor_data (gvs binding)
		"aqz-KE-bpKQ-中文-😊",                          // printable UTF-8 text
	}
	for _, s := range clean {
		if HasControlChars(s) {
			t.Errorf("HasControlChars(%q) = true, want false", s)
		}
	}
}
