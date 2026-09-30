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
		// Tuya Wi-Fi devices live only under an Aether Edge, and an Edge takes nothing else.
		{TuyaWiFiProfile, EdgeGatewayModel, true},
		{TuyaWiFiProfile, "minew-mg3", false},
		{TuyaWiFiProfile, Z2MGatewayModel, false},
		{"tuya-ts001x-switch@1", EdgeGatewayModel, false},
		{"minew-s1-pending@1", EdgeGatewayModel, false},
		{"retired-profile@1", EdgeGatewayModel, false},
		// Tuya Cloud devices live only under a Tuya Cloud gateway, which takes nothing else.
		{TuyaCloudProfile, TuyaCloudGatewayModel, true},
		{TuyaCloudProfile, EdgeGatewayModel, false},
		{TuyaCloudProfile, "minew-mg3", false},
		{TuyaCloudProfile, Z2MGatewayModel, false},
		{TuyaWiFiProfile, TuyaCloudGatewayModel, false},
		{"minew-s1-pending@1", TuyaCloudGatewayModel, false},
		{"retired-profile@1", TuyaCloudGatewayModel, false},
	}
	for _, c := range cases {
		if got := ProfileAllowedOn(c.profile, c.gateway); got != c.want {
			t.Fatalf("%s on %s: %v", c.profile, c.gateway, got)
		}
	}
	if g := GatewayModelByID(EdgeGatewayModel); g == nil || g.Transport != "mqtt" {
		t.Fatal("edge gateway model missing")
	}
	if p := DeviceProfileByID(TuyaWiFiProfile); p == nil || !p.Actuator || p.Radio != "tuya-wifi" {
		t.Fatal("Tuya Wi-Fi profile")
	}
	if g := GatewayModelByID(TuyaCloudGatewayModel); g == nil || g.Transport != "cloud" {
		t.Fatal("tuya cloud gateway model missing")
	}
	if p := DeviceProfileByID(TuyaCloudProfile); p == nil || !p.Actuator || p.Radio != "tuya-cloud" {
		t.Fatal("Tuya Cloud profile")
	}
	p := DeviceProfileByID("tuya-ts001x-switch@1")
	if !p.MatchesZ2M("ts0012", false) || p.MatchesZ2M("TS0601", false) || !p.MatchesZ2M("TS0601", true) || p.MatchesZ2M("TS0201", true) {
		t.Fatal("MatchesZ2M")
	}
}
