package domain

import "testing"

func TestProfileAllowedOn(t *testing.T) {
	cases := []struct {
		profile, gateway string
		want             bool
	}{
		{"tuya-ts001x-switch@1", Z2MGatewayModel, true},
		{"tuya-ts001x-switch@1", "minew-mg3", false},
		{"minew-s1-pending@1", Z2MGatewayModel, false},
		{"minew-s1-pending@1", "minew-mg3", true},
		// A profile id that left the catalog counts as non-Zigbee: movable between BLE gateways, never onto Zigbee.
		{"retired-profile@1", "minew-mg3", true},
		{"retired-profile@1", "retired-gateway-model", true},
		{"retired-profile@1", Z2MGatewayModel, false},
	}
	for _, c := range cases {
		if got := ProfileAllowedOn(c.profile, c.gateway); got != c.want {
			t.Fatalf("%s on %s: %v", c.profile, c.gateway, got)
		}
	}
	p := DeviceProfileByID("tuya-ts001x-switch@1")
	if !p.MatchesZ2M("ts0012", false) || p.MatchesZ2M("TS0601", false) || !p.MatchesZ2M("TS0601", true) || p.MatchesZ2M("TS0201", true) {
		t.Fatal("MatchesZ2M")
	}
}
