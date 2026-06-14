package browser

import "regexp"

// MaxContentBindingBytes is the maximum content_binding size accepted by
// token-minting endpoints. The limit rejects accidental oversized inputs without
// imposing a format on generic bindings.
const MaxContentBindingBytes = 4096

// videoIDPattern defines the bare video ID format accepted by WaxSeal. YouTube
// video IDs are currently 11 characters, but the wider bound avoids making
// that length part of the API contract.
var videoIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ValidVideoID reports whether s is a syntactically valid bare video ID. It
// does not verify that the video exists.
func ValidVideoID(s string) bool { return videoIDPattern.MatchString(s) }
