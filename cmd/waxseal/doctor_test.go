package main

import "testing"

func TestLeadingInt(t *testing.T) {
	cases := map[string]int{
		"149.0.7827.54": 149, // the Version History API's version field
		"131":           131,
		"7":             7,
		"":              0,
		"abc":           0,
	}
	for s, want := range cases {
		if got := leadingInt(s); got != want {
			t.Errorf("leadingInt(%q) = %d, want %d", s, got, want)
		}
	}
}
