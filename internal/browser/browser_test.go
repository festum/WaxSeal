package browser

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestDetectChromeEnvOverride(t *testing.T) {
	t.Setenv("WAXSEAL_CHROME_BIN", "/custom/chromium")
	got, err := DetectChrome()
	if err != nil || got != "/custom/chromium" {
		t.Fatalf("DetectChrome() = %q, %v; want /custom/chromium", got, err)
	}
}

func TestWithDefaults(t *testing.T) {
	o := withDefaults(Options{})
	if o.Logger == nil {
		t.Error("Logger default is nil")
	}
	if o.NavTimeout <= 0 {
		t.Errorf("NavTimeout default = %v, want > 0", o.NavTimeout)
	}
	if got := withDefaults(Options{NavTimeout: 5 * time.Second}).NavTimeout; got != 5*time.Second {
		t.Errorf("explicit NavTimeout overwritten: %v", got)
	}
}

// TestUAOverride pins normalizeUA's actual emitted Network.setUserAgentOverride
// payload. The CDP wire golden marshals a separate hand-written struct, so it
// cannot catch drift in this producer, such as Architecture changing to x86_64,
// brand version changes, or Bitness being omitted. This asserts the real
// producer's bytes.
func TestUAOverride(t *testing.T) {
	const realUA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36"
	const want = `{"userAgent":"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36","userAgentMetadata":{"brands":[{"brand":"Chromium","version":"149"},{"brand":"Not)A;Brand","version":"24"}],"fullVersionList":[{"brand":"Chromium","version":"149.0.0.0"},{"brand":"Not)A;Brand","version":"24.0.0.0"}],"fullVersion":"149.0.0.0","platform":"Linux","platformVersion":"","architecture":"x86","model":"","mobile":false,"bitness":"64"}}`

	got, err := json.Marshal(uaOverride(realUA))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("uaOverride payload drift:\n got: %s\nwant: %s", got, want)
	}
	for _, must := range []string{`"model":""`, `"platformVersion":""`} {
		if !strings.Contains(string(got), must) {
			t.Errorf("payload missing %s (omitempty regression)", must)
		}
	}

	// HeadlessChrome is rewritten and the major comes from the real UA, not the
	// hardcoded fallback.
	hl := uaOverride("Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) HeadlessChrome/151.0.0.0 Safari/537.36")
	if strings.Contains(hl.UserAgent, "HeadlessChrome") {
		t.Errorf("HeadlessChrome marker not removed: %q", hl.UserAgent)
	}
	if hl.UserAgentMetadata.Brands[0].Version != "151" {
		t.Errorf("major = %q, want 151 (derived from the real UA)", hl.UserAgentMetadata.Brands[0].Version)
	}
}

func TestDefaultVideoSet(t *testing.T) {
	if DefaultVideo == "" {
		t.Error("DefaultVideo must be a non-empty (non-copyrighted) video id")
	}
}

// TestAudioFormatTagDrift keeps the extracted JSON fields in sync with AudioFormat.
func TestAudioFormatTagDrift(t *testing.T) {
	const payload = `{"itag":251,"lmt":"171","is_drc":true,"audio_track_id":"en.4"}`
	var f AudioFormat
	if err := json.Unmarshal([]byte(payload), &f); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !f.IsDrc {
		t.Error("is_drc did not decode into IsDrc")
	}
	if f.AudioTrackID != "en.4" {
		t.Errorf("audio_track_id = %q, want en.4", f.AudioTrackID)
	}
}

// TestConfirmTerminal covers stale evidence that must not mark the current video
// unavailable.
func TestConfirmTerminal(t *testing.T) {
	const want = "vid123"
	raw := func(mut func(*playerContextRaw)) playerContextRaw {
		r := playerContextRaw{Error: "pending"}
		mut(&r)
		return r
	}
	tests := []struct {
		name         string
		raw          playerContextRaw
		wantTerminal bool
		wantStatus   string
	}{
		{"gen-matched onError 100, id match", raw(func(r *playerContextRaw) { r.ErrCode = 100; r.ErrGenMatch = true; r.ErrVideoID = want }), true, "ERROR"},
		{"gen-matched onError 150, id match", raw(func(r *playerContextRaw) { r.ErrCode = 150; r.ErrGenMatch = true; r.ErrVideoID = want }), true, "ERROR"},
		{"gen-matched onError 100, stale video id", raw(func(r *playerContextRaw) { r.ErrCode = 100; r.ErrGenMatch = true; r.ErrVideoID = "othervid" }), false, ""},
		{"gen-matched onError 100, empty video id", raw(func(r *playerContextRaw) { r.ErrCode = 100; r.ErrGenMatch = true; r.ErrVideoID = "" }), false, ""},
		{"non-OK status + id match", raw(func(r *playerContextRaw) { r.PlayabilityStatus = "LOGIN_REQUIRED"; r.VideoIDMatch = true }), true, "LOGIN_REQUIRED"},
		{"non-OK status for another video", raw(func(r *playerContextRaw) { r.PlayabilityStatus = "ERROR"; r.VideoIDMatch = false }), false, ""},
		{"onError 100 with gen mismatch", raw(func(r *playerContextRaw) { r.ErrCode = 100; r.ErrGenMatch = false; r.ErrVideoID = want }), false, ""},
		{"onError 5 (non-terminal code)", raw(func(r *playerContextRaw) { r.ErrCode = 5; r.ErrGenMatch = true; r.ErrVideoID = want }), false, ""},
		{"status OK + id match", raw(func(r *playerContextRaw) { r.PlayabilityStatus = "OK"; r.VideoIDMatch = true }), false, ""},
		{"no evidence", raw(func(r *playerContextRaw) {}), false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ue, ok := confirmTerminal(tt.raw, want)
			if ok != tt.wantTerminal {
				t.Fatalf("terminal = %v, want %v", ok, tt.wantTerminal)
			}
			if !ok {
				if ue != nil {
					t.Errorf("non-terminal returned a non-nil error: %v", ue)
				}
				return
			}
			if ue.Status != tt.wantStatus {
				t.Errorf("status = %q, want %q", ue.Status, tt.wantStatus)
			}
		})
	}
}

func TestIsUnavailableCode(t *testing.T) {
	for _, c := range []int{2, 100, 101, 150} {
		if !isUnavailableCode(c) {
			t.Errorf("isUnavailableCode(%d) = false, want true", c)
		}
	}
	for _, c := range []int{0, 5, 3, 104, 999} {
		if isUnavailableCode(c) {
			t.Errorf("isUnavailableCode(%d) = true, want false", c)
		}
	}
}

func TestFullLengthProbeModel(t *testing.T) {
	outcomes := map[string]bool{
		OutcomeFullLength:        true,
		OutcomeTargetNotBuffered: true,
		OutcomeNotEstablished:    true,
		OutcomeVideoTooShort:     true,
		OutcomeCanceled:          true,
	}
	if len(outcomes) != 5 {
		t.Fatalf("outcome constants are not all distinct: %v", outcomes)
	}
	if OutcomeFullLength != "full-length" {
		t.Errorf("OutcomeFullLength = %q, want full-length", OutcomeFullLength)
	}
}

func TestEstablishFromCandidates(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	full := FullLengthProbe{Outcome: OutcomeFullLength, FullLength: true}
	tooShort := FullLengthProbe{Outcome: OutcomeVideoTooShort, Reason: "too short"}
	capped := FullLengthProbe{Outcome: OutcomeTargetNotBuffered, Reason: "status-2 cap"}
	noEstablish := FullLengthProbe{Outcome: OutcomeNotEstablished, Reason: "no context"}

	type res struct {
		probe FullLengthProbe
		err   error
	}
	// A real proveFullLength reports an unplayable video as OutcomeNotEstablished
	// with a non-nil ErrUnplayable; the helper keys off the error, not the outcome.
	unplayable := res{FullLengthProbe{Outcome: OutcomeNotEstablished}, &UnplayableError{Status: "ERROR"}}

	tests := []struct {
		name        string
		candidates  []string
		results     map[string]res
		wantErr     bool
		wantErrText []string
		errIs       error
		errIsNot    error
		wantCalls   []string
	}{
		{
			name:       "dead first video falls through to a healthy candidate",
			candidates: []string{"dead", "good"},
			results:    map[string]res{"dead": unplayable, "good": {full, nil}},
			wantCalls:  []string{"dead", "good"},
		},
		{
			name:       "too-short advances to the next candidate",
			candidates: []string{"short", "good"},
			results:    map[string]res{"short": {tooShort, nil}, "good": {full, nil}},
			wantCalls:  []string{"short", "good"},
		},
		{
			name:        "target-not-buffered stops fallback",
			candidates:  []string{"capped", "good"},
			results:     map[string]res{"capped": {capped, nil}, "good": {full, nil}},
			wantErr:     true,
			wantErrText: []string{OutcomeTargetNotBuffered},
			wantCalls:   []string{"capped"},
		},
		{
			name:        "not-established stops fallback",
			candidates:  []string{"noctx", "good"},
			results:     map[string]res{"noctx": {noEstablish, nil}, "good": {full, nil}},
			wantErr:     true,
			wantErrText: []string{OutcomeNotEstablished},
			wantCalls:   []string{"noctx"},
		},
		{
			name:       "context cancellation propagates without further candidates",
			candidates: []string{"cancel", "good"},
			results:    map[string]res{"cancel": {FullLengthProbe{Outcome: OutcomeCanceled}, context.Canceled}, "good": {full, nil}},
			wantErr:    true,
			errIs:      context.Canceled,
			wantCalls:  []string{"cancel"},
		},
		{
			name:        "all unusable candidates return an aggregate error",
			candidates:  []string{"dead", "short"},
			results:     map[string]res{"dead": unplayable, "short": {tooShort, nil}},
			wantErr:     true,
			wantErrText: []string{"no usable proof video", "dead", "short"},
			wantCalls:   []string{"dead", "short"},
		},
		{
			// Do not let failures from internal proof videos mark the caller's video
			// as unavailable.
			name:        "exhausted candidates do not expose ErrUnplayable",
			candidates:  []string{"dead1", "dead2"},
			results:     map[string]res{"dead1": unplayable, "dead2": unplayable},
			wantErr:     true,
			wantErrText: []string{"no usable proof video", "dead1", "dead2"},
			errIsNot:    ErrUnplayable,
			wantCalls:   []string{"dead1", "dead2"},
		},
		{
			name:       "duplicate and empty candidates are skipped",
			candidates: []string{"good", "", "good"},
			results:    map[string]res{"good": {full, nil}},
			wantCalls:  []string{"good"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls []string
			prove := func(v string) (FullLengthProbe, error) {
				calls = append(calls, v)
				r, ok := tt.results[v]
				if !ok {
					t.Fatalf("prove called with unexpected video %q", v)
				}
				return r.probe, r.err
			}
			err := establishFromCandidates(context.Background(), prove, tt.candidates, log)
			switch {
			case tt.wantErr && err == nil:
				t.Fatalf("err = nil, want an error")
			case !tt.wantErr && err != nil:
				t.Fatalf("err = %v, want nil (established)", err)
			}
			for _, text := range tt.wantErrText {
				if !strings.Contains(err.Error(), text) {
					t.Errorf("err = %q, want it to contain %q", err.Error(), text)
				}
			}
			if tt.errIs != nil && !errors.Is(err, tt.errIs) {
				t.Errorf("err = %v, want errors.Is %v", err, tt.errIs)
			}
			if tt.errIsNot != nil && errors.Is(err, tt.errIsNot) {
				t.Errorf("err = %v, unexpectedly matches errors.Is %v", err, tt.errIsNot)
			}
			if !slices.Equal(calls, tt.wantCalls) {
				t.Errorf("calls = %v, want %v", calls, tt.wantCalls)
			}
		})
	}
}
