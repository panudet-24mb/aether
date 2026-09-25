package edge

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"aether/backend/internal/domain"
)

const gw = "8f3c2a1e-4b5d-4c6e-9f70-1a2b3c4d5e6f"

func TestRoute(t *testing.T) {
	dev := "bf1234567890abcdefgh"
	cases := []struct {
		topic  string
		kind   Kind
		device string
		err    bool
	}{
		{Prefix + gw + "/status", Status, "", false},
		{Prefix + gw + "/health", Health, "", false},
		{Prefix + gw + "/discovery", Discovery, "", false},
		{Prefix + gw + "/" + dev + "/state", State, dev, false},
		{Prefix + gw + "/" + dev + "/availability", Availability, dev, false},
		{Prefix + gw + "/" + dev + "/set", Ignore, "", false},
		{Prefix + gw + "/" + dev + "/state/extra", Ignore, "", false},
		{Prefix + gw + "/whatever", Ignore, "", false},
		{Prefix + gw + "/BF1234567890ABCDEFGH/state", Ignore, "", true},
		{Prefix + gw + "/short/state", Ignore, "", true},
		{Prefix + "not-a-uuid/status", Ignore, "", true},
		{Prefix + gw, Ignore, "", true},
		{"aether/z2m/" + gw + "/status", Ignore, "", true},
	}
	for _, c := range cases {
		g, m, e := Route(c.topic)
		if c.err {
			if !errors.Is(e, domain.ErrForbidden) {
				t.Fatalf("%s: want forbidden, got %v", c.topic, e)
			}
			continue
		}
		if e != nil || g != gw || m.Kind != c.kind || m.Device != c.device {
			t.Fatalf("%s: %q %+v %v", c.topic, g, m, e)
		}
	}
}

func TestSetTopicAndPayload(t *testing.T) {
	topic, e := SetTopic(gw, "bf1234567890abcdefgh")
	if e != nil || topic != Prefix+gw+"/bf1234567890abcdefgh/set" {
		t.Fatal(topic, e)
	}
	for _, bad := range [][2]string{{"x", "bf1234567890abcdefgh"}, {gw, "#"}, {gw, "../bridge"}, {gw, "bf1234567890abcdefgh/+"}} {
		if _, e := SetTopic(bad[0], bad[1]); e == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
	p, e := SetPayload(json.RawMessage(`{ "dps": {"1": true} }`))
	if e != nil || string(p) != `{"dps":{"1":true}}` {
		t.Fatal(string(p), e)
	}
	for _, bad := range []string{``, `{}`, `{"dps":{}}`, `{"dps":{"0":1}}`, `{"dps":{"256":1}}`, `{"dps":{"01":1}}`, `{"dps":{"a":1}}`, `[1]`} {
		if _, e := SetPayload(json.RawMessage(bad)); e == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}

func TestParsers(t *testing.T) {
	if on, _, ok := ParseOnline([]byte(`{"state":"online"}`)); !ok || !on {
		t.Fatal("online")
	}
	if on, reason, ok := ParseOnline([]byte(`{"state":"offline","reason":"auth_failed"}`)); !ok || on || reason != ReasonAuthFailed {
		t.Fatal("auth_failed")
	}
	if _, reason, ok := ParseOnline([]byte(`{"state":"offline","reason":"<script>"}`)); !ok || reason != "" {
		t.Fatal("unknown reason must be dropped")
	}
	if _, _, ok := ParseOnline([]byte(`"offline"`)); !ok {
		t.Fatal("bare string")
	}
	if _, _, ok := ParseOnline([]byte(`{"state":"maybe"}`)); ok {
		t.Fatal("unknown state")
	}
	h, ok := ParseHealth([]byte(`{"version":"0.1.0","devices_connected":3,"lan_seen":-4}`))
	if !ok || h.Version != "0.1.0" || h.DevicesConnected != 3 || h.LANSeen != 0 {
		t.Fatal(h)
	}
	lan, e := ParseDiscovery([]byte(`[{"id":"BF1234567890ABCDEFGH","ip":"192.168.1.20","version":"3.4","product_key":"key"},
		{"id":"bad","ip":"192.168.1.21","version":"3.3"},{"id":"bf1234567890abcdefgi","ip":"fe80::1","version":"3.3"},
		{"id":"bf1234567890abcdefgj","ip":"192.168.1.22","version":"9.9"},{"id":"bf1234567890abcdefgh","ip":"192.168.1.23","version":"3.3"}]`))
	if e != nil || len(lan) != 1 || lan[0].ID != "bf1234567890abcdefgh" || lan[0].IP != "192.168.1.20" {
		t.Fatal(lan, e)
	}
	dps, full, e := ParseState([]byte(`{"dps":{"1":true,"19":1234,"25":"white"},"full":true}`))
	if e != nil || !full || len(dps) != 3 || string(dps[19]) != "1234" {
		t.Fatal(dps, full, e)
	}
	for _, bad := range []string{`{}`, `{"dps":[]}`, `{"dps":{"x":1}}`, `not json`} {
		if _, _, e := ParseState([]byte(bad)); e == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}

// A cut in the middle of a multi-byte character would make PostgreSQL refuse the whole message (22021).
func TestClipKeepsUTF8(t *testing.T) {
	s := strings.Repeat("a", 63) + "é" // 65 bytes; 64 falls inside é
	if got := Clip(s, 64); got != strings.Repeat("a", 63) || !utf8.ValidString(got) {
		t.Fatalf("%q", got)
	}
	if got := Clip("ไทย\x00ok", 8); got != "ไทย"[:6] || !utf8.ValidString(got) {
		t.Fatalf("%q", got)
	}
	if got := Clip("a\xffb", 10); got != "ab" {
		t.Fatalf("invalid UTF-8 kept: %q", got)
	}
	lan, e := ParseDiscovery([]byte(`[{"id":"bf1234567890abcdefgh","ip":"10.0.0.2","version":"3.3","product_key":"` + s + `"}]`))
	if e != nil || len(lan) != 1 || !utf8.ValidString(lan[0].ProductKey) || len(lan[0].ProductKey) != 63 {
		t.Fatal(lan, e)
	}
	h, _ := ParseHealth([]byte(`{"version":"` + strings.Repeat("v", 31) + `ก"}`))
	if !utf8.ValidString(h.Version) || len(h.Version) > 32 {
		t.Fatalf("%q", h.Version)
	}
}
