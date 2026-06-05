package botguard

import (
	"encoding/base64"
	"errors"
	"testing"
)

// Vector lifted from rustypipe protobuf.rs (MIT): a real token whose field 6
// decodes to a known value. Proves our scanner matches the reference.
func TestValidatePOTokenReferenceVector(t *testing.T) {
	const token = "MlJ0C4bw9OdQ1nQ3M4AKb9hdTYvhORYpaKAnKK5fE1Jcf5kd988G3WHrMWU-nCS0nDwoHlg27bfcBHpSg0_F3aaXu3da8V2QE3q0M_97L-iBTWF0"
	const wantField6 = "dAuG8PTnUNZ0NzOACm_YXU2L4TkWKWigJyiuXxNSXH-ZHffPBt1h6zFlPpwktJw8KB5YNu233AR6UoNPxd2ml7t3WvFdkBN6tDP_ey_ogU1hdA=="

	field6, err := ValidatePOToken(token)
	if err != nil {
		t.Fatalf("validate reference token: %v", err)
	}
	got := base64.URLEncoding.EncodeToString(field6)
	if got != wantField6 {
		t.Fatalf("field6 = %s\nwant     %s", got, wantField6)
	}
}

// The BgUtils u8ToBase64 helper keeps '=' padding while emitting websafe chars;
// other producers strip it. Both must validate.
func TestValidateAcceptsPaddedAndUnpadded(t *testing.T) {
	// protobuf: field 6 (wire type 2), len 3, "abc"
	raw := []byte{0x32, 0x03, 'a', 'b', 'c'}
	padded := base64.URLEncoding.EncodeToString(raw)      // has '='
	unpadded := base64.RawURLEncoding.EncodeToString(raw) // no '='

	for name, tok := range map[string]string{"padded": padded, "unpadded": unpadded} {
		field6, err := ValidatePOToken(tok)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(field6) != "abc" {
			t.Fatalf("%s: field6 = %q", name, field6)
		}
	}
}

func TestValidateRejectsMissingField6(t *testing.T) {
	// Field 1 varint, field 5 bytes, no field 6.
	raw := []byte{0x08, 0x96, 0x01, 0x2a, 0x02, 'h', 'i'}
	_, err := ValidatePOToken(base64.RawURLEncoding.EncodeToString(raw))
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("want ErrInvalidToken, got %v", err)
	}
}

func TestValidateRejectsBadBase64(t *testing.T) {
	_, err := ValidatePOToken("!!!not base64!!!")
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("want ErrInvalidToken, got %v", err)
	}
}

// Field 6 must be reachable past skipped varint/fixed64/fixed32 fields.
func TestValidateSkipsOtherWireTypes(t *testing.T) {
	raw := []byte{
		0x08, 0x01, // field 1 varint
		0x11, 1, 2, 3, 4, 5, 6, 7, 8, // field 2 fixed64
		0x25, 9, 9, 9, 9, // field 4 fixed32
		0x32, 0x02, 'o', 'k', // field 6 bytes "ok"
	}
	field6, err := ValidatePOToken(base64.RawURLEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if string(field6) != "ok" {
		t.Fatalf("field6 = %q", field6)
	}
}
