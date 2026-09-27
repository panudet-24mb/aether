package tests

import (
	"aether/backend/internal/domain"
	"aether/backend/internal/edge"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
)

// The installer script is served with this server's origin and the pinned image filled in.
func TestEdgeInstallScriptRoute(t *testing.T) {
	r := newImportRig(t, "")
	res, e := r.api.Test(httptest.NewRequest("GET", "/edge/install.sh", nil), fiber.TestConfig{Timeout: 15 * time.Second})
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	script := string(b)
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/plain") || res.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("install.sh: %d %q %q", res.StatusCode, res.Header.Get("Content-Type"), res.Header.Get("Cache-Control"))
	}
	for _, want := range []string{"#!/bin/sh", "ORIGIN='" + r.f.cfg.Origin + "'", "IMAGE='" + domain.EdgeImage + ":" + domain.EdgeImageTag + "'",
		"Z2M_IMAGE='" + domain.Zigbee2MQTTImage + "'"} {
		if !strings.Contains(script, want) {
			t.Fatalf("install.sh lacks %q", want)
		}
	}
}

// An install code may pair a Zigbee2MQTT gateway: redeeming it rotates that gateway's broker password too and hands
// it to the installer once.
func TestEdgeInstallCodePairsZigbee2MQTT(t *testing.T) {
	t.Setenv("MQTT_PUBLIC_HOST", "mqtt.example.test")
	t.Setenv("MQTT_PUBLIC_PORT", "8883")
	t.Setenv("MQTT_PUBLIC_SCHEME", "ssl")
	certPEM, _ := testCA(t)
	ca := filepath.Join(t.TempDir(), "ca.crt")
	if e := os.WriteFile(ca, certPEM, 0o644); e != nil {
		t.Fatal(e)
	}
	t.Setenv("MQTT_CA_FILE", ca)
	r := newImportRig(t, "")
	z, _, e := r.f.service.CreateGatewayIn(r.ctx, r.owner, "Zigbee", domain.Z2MGatewayModel, nil)
	if e != nil {
		t.Fatal(e)
	}
	ble, _, e := r.f.service.CreateGatewayIn(r.ctx, r.owner, "BLE", "minew-mg3", nil)
	if e != nil {
		t.Fatal(e)
	}
	// Only a live Zigbee2MQTT gateway of this workspace can be paired.
	for id, want := range map[string]string{ble.ID: "not_a_zigbee2mqtt_gateway", r.gateway: "invalid_input", "not-a-uuid": "invalid_input"} {
		if code, out := r.call("POST", "/api/v1/gateways/"+r.gateway+"/edge/install-code", map[string]any{"zigbee_gateway_id": id}); code != 400 || out["error"] != want {
			t.Fatalf("pair %s: %d %v", id, code, out)
		}
	}
	other := newImportRig(t, "")
	oz, _, e := other.f.service.CreateGatewayIn(other.ctx, other.owner, "Other zigbee", domain.Z2MGatewayModel, nil)
	if e != nil {
		t.Fatal(e)
	}
	if code, out := r.call("POST", "/api/v1/gateways/"+r.gateway+"/edge/install-code", map[string]any{"zigbee_gateway_id": oz.ID}); code != 400 || out["error"] != "not_a_zigbee2mqtt_gateway" {
		t.Fatalf("another workspace's gateway: %d %v", code, out)
	}
	code, out := r.call("POST", "/api/v1/gateways/"+r.gateway+"/edge/install-code", map[string]any{"zigbee_gateway_id": z.ID})
	if code != 201 || out["zigbee_paired"] != true || !strings.Contains(out["install_command"].(string), "--zigbee <SLZB_IP>") || strings.Contains(out["install_command"].(string), out["code"].(string)) ||
		out["image"] != domain.EdgeImage+":"+domain.EdgeImageTag {
		t.Fatalf("paired install code: %d %v", code, out)
	}
	bootstrap := func(code string) (int, map[string]any) {
		b, _ := json.Marshal(map[string]string{"code": code})
		q := httptest.NewRequest("POST", "/edge/bootstrap", strings.NewReader(string(b)))
		q.Header.Set("Content-Type", "application/json")
		res, e := r.api.Test(q, fiber.TestConfig{Timeout: 15 * time.Second})
		if e != nil {
			t.Fatal(e)
		}
		defer res.Body.Close()
		m := map[string]any{}
		_ = json.NewDecoder(res.Body).Decode(&m)
		return res.StatusCode, m
	}
	// A paired gateway revoked in the meantime fails the bootstrap and leaves the code unspent.
	if _, e := r.f.admin.ExecContext(r.ctx, `UPDATE core.gateways SET revoked_at=now() WHERE id=$1`, z.ID); e != nil {
		t.Fatal(e)
	}
	if status, m := bootstrap(out["code"].(string)); status != 400 || m["error"] != "zigbee_gateway_unavailable" {
		t.Fatalf("revoked pair: %d %v", status, m)
	}
	if r.count(`SELECT count(*) FROM core.edge_install_codes WHERE gateway_id=$1 AND redeemed_at IS NULL`, r.gateway) != 1 {
		t.Fatal("a failed bootstrap spent the code")
	}
	if _, e := r.f.admin.ExecContext(r.ctx, `UPDATE core.gateways SET revoked_at=NULL WHERE id=$1`, z.ID); e != nil {
		t.Fatal(e)
	}
	status, bundle := bootstrap(out["code"].(string))
	if status != 201 {
		t.Fatalf("bootstrap: %d %v", status, bundle)
	}
	zb, ok := bundle["zigbee"].(map[string]any)
	if !ok || zb["gateway_id"] != z.ID || zb["base_topic"] != "aether/z2m/"+z.ID || zb["username"] != "gw-"+z.ID ||
		zb["server"] != "mqtts://mqtt.example.test:8883" || zb["password"] == "" || zb["password"] == bundle["mqtt"].(map[string]any)["password"] {
		t.Fatalf("zigbee credentials: %v", bundle["zigbee"])
	}
	if r.count(`SELECT count(*) FROM core.mqtt_accounts WHERE gateway_id=$1`, z.ID) != 1 {
		t.Fatal("the paired Zigbee2MQTT gateway has no broker account")
	}
	// An unpaired code carries no Zigbee2MQTT credentials.
	code, out = r.call("POST", "/api/v1/gateways/"+r.gateway+"/edge/install-code", nil)
	if code != 201 || out["zigbee_paired"] != false {
		t.Fatalf("unpaired code: %d %v", code, out)
	}
	if status, bundle := bootstrap(out["code"].(string)); status != 201 || bundle["zigbee"] != nil {
		t.Fatalf("unpaired bootstrap: %d %v", status, bundle["zigbee"])
	}
}

// The server and the agent pin the same Zigbee2MQTT release (the agent's is only a fallback).
func TestZigbee2MQTTImagePinnedOnce(t *testing.T) {
	if domain.Zigbee2MQTTImage != edge.Zigbee2MQTTImage {
		t.Fatalf("server pins %s, agent defaults to %s", domain.Zigbee2MQTTImage, edge.Zigbee2MQTTImage)
	}
}

// The paired Zigbee2MQTT gateway's credential rotation is audited on that gateway.
func TestEdgeBootstrapAuditsZigbeeRotation(t *testing.T) {
	t.Setenv("MQTT_PUBLIC_HOST", "mqtt.example.test")
	t.Setenv("MQTT_PUBLIC_PORT", "8883")
	t.Setenv("MQTT_PUBLIC_SCHEME", "ssl")
	certPEM, _ := testCA(t)
	ca := filepath.Join(t.TempDir(), "ca.crt")
	if e := os.WriteFile(ca, certPEM, 0o644); e != nil {
		t.Fatal(e)
	}
	t.Setenv("MQTT_CA_FILE", ca)
	r := newImportRig(t, "")
	z, _, e := r.f.service.CreateGatewayIn(r.ctx, r.owner, "Zigbee", domain.Z2MGatewayModel, nil)
	if e != nil {
		t.Fatal(e)
	}
	code, _, e := r.f.service.CreateEdgeInstallCode(r.ctx, r.owner, r.gateway, z.ID)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := r.f.service.BootstrapEdge(r.ctx, code); e != nil {
		t.Fatal(e)
	}
	if r.count(`SELECT count(*) FROM core.audit_logs WHERE action='zigbee2mqtt.credentials_rotated_by_edge' AND target_id=$1 AND actor_id=$2`, z.ID, r.owner.UserID) != 1 {
		t.Fatal("the paired gateway's credential rotation was not audited")
	}
}
