package botguard

import "strings"

// driftMarker prefixes browser APIs that the VM probes but the shim lacks.
const driftMarker = "API-DRIFT probe: "

// DriftProbes extracts the unique, actionable API-drift probe paths from captured
// VM stderr. It ignores empty property names and returns paths in first-seen
// order without duplicates.
func DriftProbes(capture string) []string {
	var paths []string
	seen := map[string]bool{}
	for line := range strings.SplitSeq(capture, "\n") {
		_, after, found := strings.Cut(line, driftMarker)
		if !found {
			continue
		}
		p := strings.TrimSpace(after)
		// An empty leaf or bare label does not identify an API.
		if dot := strings.LastIndex(p, "."); dot < 0 || dot == len(p)-1 {
			continue
		}
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	return paths
}
