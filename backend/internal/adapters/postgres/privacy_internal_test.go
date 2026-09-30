package postgres

import (
	"testing"
	"unicode/utf8"
)

func TestClipKeepsRunes(t *testing.T) {
	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{
		{"abc", 5, "abc"},
		{"abcdef", 3, "abc"},
		{"กขค", 4, "ก"}, // each Thai letter is 3 bytes
		{"กขค", 6, "กข"},
		{"กขค", 2, ""},
	} {
		got := clip(tc.in, tc.n)
		if got != tc.want || !utf8.ValidString(got) {
			t.Fatalf("clip(%q,%d) = %q", tc.in, tc.n, got)
		}
	}
}
