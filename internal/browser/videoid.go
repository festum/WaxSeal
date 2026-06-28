package browser

import (
	"regexp"
	"strings"
	"unicode"
)

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

// HasControlChars reports whether s contains a Unicode control character: C0
// 0x00-0x1F, DEL 0x7F, or C1 0x80-0x9F. Token-minting bindings are expected to
// be printable values such as bare video IDs or base64url visitor_data, so a
// control character marks the binding as malformed. Printable text, including
// CJK text and emoji, is allowed.
//
// Invalid UTF-8 is allowed for now. ContainsFunc decodes malformed bytes to
// U+FFFD (REPLACEMENT CHARACTER), which is not a control character. The binding
// is opaque, and JSON responses encode invalid bytes as U+FFFD. Add
// utf8.ValidString here only if content_binding gains a strict UTF-8 contract.
func HasControlChars(s string) bool {
	return strings.ContainsFunc(s, unicode.IsControl)
}
