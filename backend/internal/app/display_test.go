package app

import (
	"strings"
	"testing"
)

func TestPairingCodes(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 2000; i++ {
		code := newPairingCode()
		if len(code) != 9 || code[4] != '-' {
			t.Fatalf("shape: %q", code)
		}
		norm, ok := NormalizePairingCode(code)
		if !ok || norm != strings.ReplaceAll(code, "-", "") {
			t.Fatalf("normalise %q: %q %v", code, norm, ok)
		}
		seen[code] = true
	}
	if len(seen) < 1990 {
		t.Fatalf("only %d distinct codes in 2000", len(seen))
	}
	for raw, want := range map[string]string{" abcd-efgh ": "ABCDEFGH", "AB CD EF GH": "ABCDEFGH", "abcdefgh": "ABCDEFGH"} {
		if got, ok := NormalizePairingCode(raw); !ok || got != want {
			t.Fatalf("%q: %q %v", raw, got, ok)
		}
	}
	// Look-alikes are not in the alphabet, and neither is anything else.
	for _, bad := range []string{"ABCD-EFG0", "ABCD-EFGI", "ABCD-EFGL", "ABCD-EFGO", "ABCD-EFG", "ABCD-EFGHJ", "ABCD;EFGH", strings.Repeat("A", 40)} {
		if _, ok := NormalizePairingCode(bad); ok {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestDisplayTokens(t *testing.T) {
	token := newDisplayToken()
	if !IsDisplayToken(token) || !validDisplayToken(token) || len(token) != 4+43 {
		t.Fatalf("token %q", token)
	}
	for _, bad := range []string{"", "dsp_", "dsp_short", token + "x", "Bearer " + token, strings.Replace(token, "dsp_", "dsx_", 1)} {
		if validDisplayToken(bad) {
			t.Fatalf("accepted %q", bad)
		}
	}
}
