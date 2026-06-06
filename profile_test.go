package waxseal

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxseal/internal/jsassets"
)

// The default profile's timezone must be DST-free, and its static
// UTCOffsetMinutes must equal that zone's real offset. The shim applies a static
// offset and does not model DST, so a DST-observing default would make
// Date#getTimezoneOffset disagree with Intl for part of the year. Needs tz data
// (present on Linux/CI; skips otherwise).
func TestDefaultProfileTimezoneCoherent(t *testing.T) {
	p := DefaultProfile()
	loc, err := time.LoadLocation(p.Timezone)
	if err != nil {
		t.Skipf("tz data unavailable for %q: %v", p.Timezone, err)
	}
	_, offWinter := time.Date(2021, 1, 15, 12, 0, 0, 0, loc).Zone()
	_, offSummer := time.Date(2021, 7, 15, 12, 0, 0, 0, loc).Zone()
	if offWinter != offSummer {
		t.Fatalf("default Timezone %q observes DST (winter %ds, summer %ds); use a fixed-offset zone for the static shim offset",
			p.Timezone, offWinter, offSummer)
	}
	if want := p.UTCOffsetMinutes * 60; offWinter != want {
		t.Fatalf("UTCOffsetMinutes %d (=%ds) disagrees with zone %q actual offset %ds",
			p.UTCOffsetMinutes, want, p.Timezone, offWinter)
	}
}

func TestResolveProfileEmptyUsesDefault(t *testing.T) {
	def := DefaultProfile()
	got, err := resolveProfile("", def, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.UserAgent != def.UserAgent {
		t.Fatalf("got UA %q", got.UserAgent)
	}
	// Derived fields are filled.
	if got.AttestationUA != def.UserAgent || got.NavigatorUA != def.UserAgent {
		t.Fatalf("attestation/navigator not defaulted: %+v", got)
	}
}

func TestResolveProfileMatchesKnown(t *testing.T) {
	def := DefaultProfile()
	known := []BrowserProfile{{
		UserAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Safari/605.1.15",
		Platform:  "MacIntel",
		Vendor:    "Apple Computer, Inc.",
	}}
	got, err := resolveProfile(known[0].UserAgent, def, known)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Platform != "MacIntel" || got.Vendor != "Apple Computer, Inc." {
		t.Fatalf("did not pick the known Safari profile: %+v", got)
	}
}

func TestResolveProfileSynthesizesWebKit(t *testing.T) {
	def := DefaultProfile()
	ua := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	got, err := resolveProfile(ua, def, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.UserAgent != ua || got.NavigatorUA != ua || got.AttestationUA != ua {
		t.Fatalf("synthesized UA not threaded through: %+v", got)
	}
	if got.Platform != "Win32" {
		t.Fatalf("platform = %q, want Win32", got.Platform)
	}
}

func TestResolveProfileRejectsNonWebKit(t *testing.T) {
	def := DefaultProfile()
	// A non-WebKit UA (no AppleWebKit token).
	_, err := resolveProfile("curl/8.0", def, nil)
	if !errors.Is(err, ErrUnsupportedClient) {
		t.Fatalf("want ErrUnsupportedClient, got %v", err)
	}
}

func TestHashStableAndSensitive(t *testing.T) {
	a := DefaultProfile()
	b := DefaultProfile()
	if a.Hash() != b.Hash() {
		t.Fatal("identical profiles must hash identically")
	}
	// Default's AttestationUA/NavigatorUA derive from UserAgent, so setting them
	// explicitly to the same value must not change the hash.
	b.NavigatorUA = a.UserAgent
	b.AttestationUA = a.UserAgent
	if a.Hash() != b.Hash() {
		t.Fatal("normalization must make explicit==derived hash-equal")
	}
	c := DefaultProfile()
	c.Timezone = "Europe/London"
	if a.Hash() == c.Hash() {
		t.Fatal("differing timezone must change the hash")
	}
}

func TestShimProfileShape(t *testing.T) {
	p := DefaultProfile()
	var m map[string]any
	if err := json.Unmarshal(p.shimJSON(), &m); err != nil {
		t.Fatalf("shimJSON: %v", err)
	}
	for _, k := range []string{"userAgent", "platform", "language", "languages", "vendor", "timezone", "utcOffsetMinutes", "screen", "userAgentData"} {
		if _, ok := m[k]; !ok {
			t.Errorf("shim profile missing key %q", k)
		}
	}
	langs, ok := m["languages"].([]any)
	if !ok || len(langs) != 2 || langs[0] != "en-US" || langs[1] != "en" {
		t.Fatalf("languages = %v", m["languages"])
	}
	uad, ok := m["userAgentData"].(map[string]any)
	if !ok {
		t.Fatalf("userAgentData missing/!map: %v", m["userAgentData"])
	}
	if uad["platform"] != "Windows" || uad["mobile"] != false {
		t.Fatalf("userAgentData incoherent: %v", uad)
	}
}

func TestShimProfileNonChromeOmitsUAData(t *testing.T) {
	p := BrowserProfile{
		UserAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Safari/605.1.15",
		Platform:  "MacIntel",
		Language:  "en-US",
	}
	var m map[string]any
	_ = json.Unmarshal(p.shimJSON(), &m)
	if _, ok := m["userAgentData"]; ok {
		t.Fatal("Safari profile should omit userAgentData")
	}
}

func TestChromeMajor(t *testing.T) {
	cases := map[string]int{
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36": 149,
		"Chrome/131.0.0.0": 131,
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 Version/17.0 Safari/605.1.15": 0, // Safari: not Chrome
		"":           0,
		"Chrome/":    0, // no digits after the marker
		"Chrome/abc": 0,
	}
	for ua, want := range cases {
		if got := ChromeMajor(ua); got != want {
			t.Errorf("ChromeMajor(%q) = %d, want %d", ua, got, want)
		}
	}
}

// TestBundleChromeVersionMatchesProfile catches a stale committed bundle after
// chrome_version.json changes.
func TestBundleChromeVersionMatchesProfile(t *testing.T) {
	bundle := string(jsassets.BGBundle)
	// navigator.userAgent uses Chrome's reduced major.0.0.0 form.
	if ua := "Chrome/" + chromeVer.Major + ".0.0.0"; !strings.Contains(bundle, ua) {
		t.Errorf("committed bg_bundle.js does not contain %q; run `make jsbundle` after bumping chrome_version.json", ua)
	}
	// UA-CH high-entropy hints contain the full build.
	if !strings.Contains(bundle, chromeVer.FullVersion) {
		t.Errorf("committed bg_bundle.js does not contain full version %q; run `make jsbundle` after bumping chrome_version.json", chromeVer.FullVersion)
	}
}
