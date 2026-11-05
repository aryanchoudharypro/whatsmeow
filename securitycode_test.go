package whatsmeow

import (
	"testing"

	"go.mau.fi/whatsmeow/types"
)

func TestSecurityCodePhoneIdentifier(t *testing.T) {
	for in, want := range map[string]string{
		"919876543210": "91543210",
		"14155550123":  "1550123",
		"447700900123": "44900123",
	} {
		if got := string(securityCodePhoneIdentifier(types.NewJID(in, types.DefaultUserServer))); got != want {
			t.Errorf("%s: got %s, want %s", in, got, want)
		}
	}
}

func TestSecurityCodeHalfLength(t *testing.T) {
	keys := identityKeysBinary([][32]byte{{2}, {1}})
	if keys[0] != 5 || keys[1] != 1 || len(keys) != 66 {
		t.Fatalf("keys not sorted with type byte: %x", keys[:2])
	}
	if got := securityCodeHalf([]byte("91543210"), keys); len(got) != 30 {
		t.Fatalf("got %d digits", len(got))
	}
}
