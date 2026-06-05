package waxseal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
)

// ErrUnsupportedClient is returned when a Request.UserAgent has no coherent
// BrowserProfile, for example a non-WebKit-family UA. WAA needs a WebKit-family
// UA (Chrome qualifies via AppleWebKit/537.36); WaxSeal rejects unsupported UAs
// instead of minting with an inconsistent browser identity.
var ErrUnsupportedClient = errors.New("waxseal: unsupported client (no coherent BrowserProfile for User-Agent)")

// BrowserProfile is the client identity used everywhere identity is observed:
// the attestation HTTP User-Agent (Create/att-get/GenerateIT), the shim's
// navigator/screen/timezone/UA-CH surface, and the WaxTap GVS download UA. The
// fields are kept together so those surfaces do not drift apart. The normalized
// profile is hashed into cache and minter keys.
//
// Resolve profiles via the Client at the orchestration boundary; do not pass a
// zero profile into the VM. NavigatorUA and AttestationUA default to UserAgent;
// they exist as escape hatches if one endpoint needs a different UA.
type BrowserProfile struct {
	UserAgent string // canonical UA; matches the WaxTap GVS download UA

	NavigatorUA   string // shim navigator.userAgent (empty means UserAgent)
	AttestationUA string // UA on Create/att-get/GenerateIT (empty means UserAgent)

	Platform         string            // navigator.platform, e.g. "Win32"
	Language         string            // primary language, e.g. "en-US"
	AcceptLanguage   string            // Accept-Language header (empty derives from Language)
	Vendor           string            // navigator.vendor, e.g. "Google Inc."
	Timezone         string            // IANA tz, e.g. "America/New_York"
	UTCOffsetMinutes int               // Date/Intl offset; must agree with Timezone
	UACH             map[string]string // optional sec-ch-ua* client hints as headers
	Screen           [2]int            // [width, height]
}

// DefaultProfile is a coherent Chrome-on-Windows identity, close to WaxTap's WEB
// profile and matching the committed shim default. Chrome is WebKit-family
// (AppleWebKit/537.36), so it satisfies WAA.
//
// The default timezone is America/Phoenix, which stays at UTC-7 year-round. The
// shim applies a static UTCOffsetMinutes value and does not model DST
// transitions, so a DST-observing default would make Intl report one zone while
// Date#getTimezoneOffset reported the wrong offset for part of the year. Callers
// overriding Timezone should choose a fixed-offset zone or provide an offset
// that matches the date they want to emulate.
func DefaultProfile() BrowserProfile {
	return BrowserProfile{
		UserAgent:        "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
		Platform:         "Win32",
		Language:         "en-US",
		AcceptLanguage:   "en-US,en;q=0.9",
		Vendor:           "Google Inc.",
		Timezone:         "America/Phoenix",
		UTCOffsetMinutes: -420,
		Screen:           [2]int{1920, 1080},
	}
}

// normalized fills derived fields so the attestation UA, shim navigator, and
// profile hash all agree.
func (p BrowserProfile) normalized() BrowserProfile {
	if p.NavigatorUA == "" {
		p.NavigatorUA = p.UserAgent
	}
	if p.AttestationUA == "" {
		p.AttestationUA = p.UserAgent
	}
	if p.AcceptLanguage == "" && p.Language != "" {
		if base, _, ok := strings.Cut(p.Language, "-"); ok {
			p.AcceptLanguage = p.Language + "," + base + ";q=0.9"
		} else {
			p.AcceptLanguage = p.Language
		}
	}
	return p
}

// isWebKitFamily reports whether a UA is acceptable to WAA. Chrome, Safari, and
// Chromium-based Edge all carry "AppleWebKit".
func isWebKitFamily(ua string) bool {
	return strings.Contains(ua, "AppleWebKit")
}

// Hash is the stable identity fingerprint mixed into cache and minter keys. Two
// profiles that render the same identity hash identically.
func (p BrowserProfile) Hash() string {
	n := p.normalized()
	// encoding/json sorts map keys, so this is deterministic.
	b, _ := json.Marshal(n)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

// languages renders navigator.languages: the primary plus its base subtag.
func (p BrowserProfile) languages() []string {
	if p.Language == "" {
		return []string{"en-US", "en"}
	}
	if base, _, ok := strings.Cut(p.Language, "-"); ok && base != p.Language {
		return []string{p.Language, base}
	}
	return []string{p.Language}
}

var chromeVersionRE = regexp.MustCompile(`Chrome/(\d+)`)

// userAgentData derives navigator.userAgentData (sec-ch-ua) for Chrome UAs, so
// the high-entropy client-hint probes BotGuard reads stay coherent with the UA.
// Non-Chrome UAs return nil (the shim leaves userAgentData undefined).
func (p BrowserProfile) userAgentData() map[string]any {
	m := chromeVersionRE.FindStringSubmatch(p.NavigatorUA)
	if m == nil {
		return nil
	}
	major := m[1]
	platform := "Windows"
	switch {
	case strings.Contains(p.NavigatorUA, "Macintosh"):
		platform = "macOS"
	case strings.Contains(p.NavigatorUA, "Linux") && !strings.Contains(p.NavigatorUA, "Android"):
		platform = "Linux"
	case strings.Contains(p.NavigatorUA, "Android"):
		platform = "Android"
	}
	return map[string]any{
		"brands": []map[string]any{
			{"brand": "Google Chrome", "version": major},
			{"brand": "Chromium", "version": major},
			{"brand": "Not_A Brand", "version": "24"},
		},
		"mobile":   platform == "Android",
		"platform": platform,
	}
}

// shimProfile renders the object the shim's __wxApplyProfile consumes. Keys must
// match shim.js exactly.
func (p BrowserProfile) shimProfile() map[string]any {
	n := p.normalized()
	m := map[string]any{
		"userAgent":        n.NavigatorUA,
		"platform":         n.Platform,
		"language":         n.Language,
		"languages":        n.languages(),
		"vendor":           n.Vendor,
		"timezone":         n.Timezone,
		"utcOffsetMinutes": n.UTCOffsetMinutes,
		"screen":           []int{n.Screen[0], n.Screen[1]},
	}
	if uad := n.userAgentData(); uad != nil {
		m["userAgentData"] = uad
	}
	return m
}

// shimJSON is the profile serialized for runBotguard's 4th argument.
func (p BrowserProfile) shimJSON() json.RawMessage {
	b, _ := json.Marshal(p.shimProfile())
	return b
}

// resolveProfile picks the profile for a request UA. An empty UA uses the
// default; a UA matching a configured profile uses it; a WebKit-family UA with
// no match synthesizes a profile from the default; a non-WebKit UA is rejected
// with ErrUnsupportedClient.
func resolveProfile(ua string, def BrowserProfile, known []BrowserProfile) (BrowserProfile, error) {
	if ua == "" {
		return def.normalized(), nil
	}
	for _, p := range known {
		if p.UserAgent == ua {
			return p.normalized(), nil
		}
	}
	if def.UserAgent == ua {
		return def.normalized(), nil
	}
	if !isWebKitFamily(ua) {
		return BrowserProfile{}, ErrUnsupportedClient
	}
	// Keep the default's platform/timezone/screen but use the caller's exact UA
	// across attestation, navigator, and downstream GVS requests.
	syn := def
	syn.UserAgent = ua
	syn.NavigatorUA = ua
	syn.AttestationUA = ua
	syn.UACH = nil
	if plat := platformFromUA(ua); plat != "" {
		syn.Platform = plat
	}
	return syn.normalized(), nil
}

// platformFromUA best-effort maps a UA to navigator.platform so a synthesized
// profile stays internally coherent.
func platformFromUA(ua string) string {
	switch {
	case strings.Contains(ua, "Windows"):
		return "Win32"
	case strings.Contains(ua, "Macintosh"):
		return "MacIntel"
	case strings.Contains(ua, "Android"):
		return "Linux armv8l"
	case strings.Contains(ua, "Linux"):
		return "Linux x86_64"
	}
	return ""
}
