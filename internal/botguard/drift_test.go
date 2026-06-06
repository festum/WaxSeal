package botguard

import (
	"reflect"
	"strings"
	"testing"
)

func TestDriftProbes(t *testing.T) {
	capture := strings.Join([]string{
		"[js:info] some unrelated line",
		"[js:warn] API-DRIFT probe: window.Foo",
		"[js:warn] API-DRIFT probe: navigator.bar",
		"[js:warn] API-DRIFT probe: window.Foo", // duplicate
		"[js:warn] API-DRIFT probe: document.",  // empty property name
		"",
	}, "\n")
	got := DriftProbes(capture)
	want := []string{"window.Foo", "navigator.bar"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DriftProbes = %v, want %v", got, want)
	}
}

// TestDriftProbesFiltersEmptyLeaf verifies that empty property names are ignored.
func TestDriftProbesFiltersEmptyLeaf(t *testing.T) {
	for _, c := range []string{"", "[js:warn] no marker here", "[js:warn] API-DRIFT probe: document.", "API-DRIFT probe: "} {
		if got := DriftProbes(c); len(got) != 0 {
			t.Errorf("DriftProbes(%q) = %v, want empty", c, got)
		}
	}
}
