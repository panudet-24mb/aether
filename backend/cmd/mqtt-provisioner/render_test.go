package main

import (
	"strings"
	"testing"
)

func TestRenderPerModel(t *testing.T) {
	accounts := []account{
		{ID: "11111111-1111-4111-8111-111111111111", Hash: "$7$h1", Revision: 1, Model: "minew-mg3"},
		{ID: "22222222-2222-4222-8222-222222222222", Hash: "$7$h2", Revision: 3, Model: "zigbee2mqtt"},
		{ID: "44444444-4444-4444-8444-444444444444", Hash: "$7$h4", Revision: 1, Model: "aether-edge"},
	}
	p, a := render("aether-ingest:$7$ingest\n", "# base\n", accounts)
	if p != "aether-ingest:$7$ingest\ngw-11111111-1111-4111-8111-111111111111:$7$h1\ngw-22222222-2222-4222-8222-222222222222:$7$h2\ngw-44444444-4444-4444-8444-444444444444:$7$h4\n" {
		t.Fatalf("passwords:\n%s", p)
	}
	// The Minew block is exactly what the provisioner has always written.
	minew := "\nuser gw-11111111-1111-4111-8111-111111111111\ntopic write /aether/gateways/11111111-1111-4111-8111-111111111111/status\ntopic write /aether/gateways/11111111-1111-4111-8111-111111111111/response\ntopic read /aether/gateways/11111111-1111-4111-8111-111111111111/action\n"
	z2m := "\nuser gw-22222222-2222-4222-8222-222222222222\ntopic readwrite aether/z2m/22222222-2222-4222-8222-222222222222/#\n"
	edge := "\nuser gw-44444444-4444-4444-8444-444444444444\ntopic write aether/edge/44444444-4444-4444-8444-444444444444/#\ntopic read aether/edge/44444444-4444-4444-8444-444444444444/+/set\n"
	ingest := "# base\n\nuser aether-ingest\ntopic read /aether/gateways/+/status\ntopic read aether/z2m/+/#\ntopic read aether/edge/+/#\n"
	commander := "\nuser aether-commander\ntopic write aether/z2m/+/+/set\ntopic write aether/edge/+/+/set\n"
	if a != ingest+commander+minew+z2m+edge {
		t.Fatalf("acl:\n%s", a)
	}
	// A Zigbee2MQTT account never gets the Minew topics, and no account reaches another gateway's tree.
	if strings.Count(a, "aether/z2m/22222222") != 1 || strings.Contains(a, "gw-22222222-2222-4222-8222-222222222222\ntopic write /aether") {
		t.Fatalf("z2m isolation:\n%s", a)
	}
	// An Edge reads nothing but its own devices' command topics: no other gateway's tree, no wildcard read.
	if strings.Count(a, "aether/edge/44444444") != 2 || strings.Contains(a, "topic read aether/edge/44444444-4444-4444-8444-444444444444/#") {
		t.Fatalf("edge isolation:\n%s", a)
	}
}

// The commander's grant is exactly one write pattern: no read (it subscribes to nothing), no bridge requests.
func TestRenderCommanderIsWriteOnly(t *testing.T) {
	_, a := render("", "", nil)
	block := a[strings.Index(a, "user aether-commander"):]
	if next := strings.Index(block[1:], "\nuser "); next >= 0 {
		block = block[:next+1]
	}
	if strings.TrimSpace(block) != "user aether-commander\ntopic write aether/z2m/+/+/set\ntopic write aether/edge/+/+/set" || strings.Contains(block, "read") || strings.Contains(block, "bridge") || strings.Contains(block, "#") {
		t.Fatalf("commander grant:\n%s", block)
	}
}
