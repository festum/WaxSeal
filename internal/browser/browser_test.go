package browser

import (
	"encoding/json"
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

func TestFullLengthProbeModel(t *testing.T) {
	outcomes := map[string]bool{
		OutcomeFullLength:        true,
		OutcomeTargetNotBuffered: true,
		OutcomeNotEstablished:    true,
		OutcomeVideoTooShort:     true,
	}
	if len(outcomes) != 4 {
		t.Fatalf("outcome constants are not all distinct: %v", outcomes)
	}
	if OutcomeFullLength != "full-length" {
		t.Errorf("OutcomeFullLength = %q, want full-length", OutcomeFullLength)
	}
}
