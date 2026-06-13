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
