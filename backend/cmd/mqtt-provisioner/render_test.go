package main

import (
	"strings"
	"testing"
)

func TestRenderPerModel(t *testing.T) {
	accounts := []account{
		{ID: "11111111-1111-4111-8111-111111111111", Hash: "$7$h1", Revision: 1, Model: "minew-mg3"},
		{ID: "22222222-2222-4222-8222-222222222222", Hash: "$7$h2", Revision: 3, Model: "zigbee2mqtt"},
	}
	p, a := render("aether-ingest:$7$ingest\n", "# base\n", accounts)
	if p != "aether-ingest:$7$ingest\ngw-11111111-1111-4111-8111-111111111111:$7$h1\ngw-22222222-2222-4222-8222-222222222222:$7$h2\n" {
		t.Fatalf("passwords:\n%s", p)
	}
	// The Minew block is exactly what the provisioner has always written.
	minew := "\nuser gw-11111111-1111-4111-8111-111111111111\ntopic write /aether/gateways/11111111-1111-4111-8111-111111111111/status\ntopic write /aether/gateways/11111111-1111-4111-8111-111111111111/response\ntopic read /aether/gateways/11111111-1111-4111-8111-111111111111/action\n"
	z2m := "\nuser gw-22222222-2222-4222-8222-222222222222\ntopic readwrite aether/z2m/22222222-2222-4222-8222-222222222222/#\n"
	ingest := "# base\n\nuser aether-ingest\ntopic read /aether/gateways/+/status\ntopic read aether/z2m/+/#\n"
	if a != ingest+minew+z2m {
		t.Fatalf("acl:\n%s", a)
	}
	// A Zigbee2MQTT account never gets the Minew topics, and no account reaches another gateway's tree.
	if strings.Count(a, "aether/z2m/22222222") != 1 || strings.Contains(a, "gw-22222222-2222-4222-8222-222222222222\ntopic write /aether") {
		t.Fatalf("z2m isolation:\n%s", a)
	}
}
