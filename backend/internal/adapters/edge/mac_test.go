package edge

import "testing"

func TestNormalMAC(t *testing.T) {
	for in, want := range map[string]string{
		"DC234D000001":      "dc:23:4d:00:00:01",
		"dc:23:4d:00:00:01": "dc:23:4d:00:00:01",
		"DC-23-4D-00-00-01": "dc:23:4d:00:00:01",
		" dc234d000001 ":    "dc:23:4d:00:00:01",
		"dc234d00000":       "",
		"dc234d0000011":     "",
		"zz234d000001":      "",
		"":                  "",
		"dc:23:4d:00:00:0g": "",
		"000000000000":      "",
		"FF:FF:FF:FF:FF:FF": "",
		"a5a5a5a5a5a5":      "",
	} {
		if got := NormalMAC(in); got != want {
			t.Fatalf("NormalMAC(%q) = %q, want %q", in, got, want)
		}
	}
	if got := ReversedMAC("dc:23:4d:00:00:01"); got != "01:00:00:4d:23:dc" {
		t.Fatalf("reversed: %s", got)
	}
	if ReversedMAC(ReversedMAC("a4:c1:38:12:34:56")) != "a4:c1:38:12:34:56" {
		t.Fatal("reversing twice is not the identity")
	}
}
