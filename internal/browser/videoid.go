package browser

import "regexp"

// videoIDPattern defines the bare video ID format accepted by WaxSeal. YouTube
// video IDs are currently 11 characters, but the wider bound avoids making
// that length part of the API contract.
var videoIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ValidVideoID reports whether s is a syntactically valid bare video ID. It
// does not verify that the video exists.
func ValidVideoID(s string) bool { return videoIDPattern.MatchString(s) }
