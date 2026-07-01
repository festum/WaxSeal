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

// URLBindingWarning explains why a URL-shaped binding is a mistake. A token binds
// a bare identifier, so a pasted watch link mints successfully but is later
// rejected by YouTube's SABR layer. Callers prepend the field name for the value
// (for example "content-binding" or "content_binding").
//
// The message stays scope-neutral because the opaque binding path carries no
// scope, so a player-centric line would mislead a gvs caller. A player token binds
// a video ID, a GVS token binds visitor_data, and neither is a URL.
const URLBindingWarning = "looks like a URL; a token binds a bare identifier " +
	"(a video ID for player scope, or visitor_data for gvs), not a URL, so YouTube rejects it"

// URLBindingWarningFor returns the warning to show when a content_binding value
// looks like a pasted URL instead of the bare identifier a token binds. field is
// the caller's name for the input ("content_binding" for the HTTP API,
// "content-binding" for the CLI flag), prepended to the shared explanation. When
// value is a normal binding, warn is false and msg is empty, so callers gate on
// warn. Owning both the LooksLikeWatchURL gate and the field-prefix assembly here
// keeps the CLI and HTTP warnings from drifting apart.
func URLBindingWarningFor(field, value string) (msg string, warn bool) {
	if !LooksLikeWatchURL(value) {
		return "", false
	}
	return field + " " + URLBindingWarning, true
}

// LooksLikeWatchURL reports whether s looks like a pasted YouTube link instead of
// the bare identifier a token binds. It drives the content_binding warning and
// sharpens the landing-video error message.
//
// It is true when s carries a URL scheme (contains "://") or contains "youtube.com"
// or "youtu.be" in any case. The domain check catches a scheme-less paste such as
// "youtube.com/watch?v=...", which a scheme-only test would miss.
//
// It is broader than a scheme check and does not validate a host:port. It reports
// true for "youtube.com:4416", which is a valid --addr, so code that only rejects
// a doubled scheme uses a scheme-only test instead.
func LooksLikeWatchURL(s string) bool {
	if strings.Contains(s, "://") {
		return true
	}
	lower := strings.ToLower(s)
	return strings.Contains(lower, "youtube.com") || strings.Contains(lower, "youtu.be")
}

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
