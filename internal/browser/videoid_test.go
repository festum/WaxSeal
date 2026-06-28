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
