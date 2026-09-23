package tests

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Runs against aether_test only. Optional isolated broker; no external notification channel.
// Supply AETHER_JS_RUNNER and a python3 with QuickJS to exercise the actual widget sandbox.
func TestCustomerJourneyFromGatewayToDashboardAndAlert(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux sandbox resource limits; run infra/test-customer-journey.py")
	}
	if exec.Command("python3", "-c", "import quickjs").Run() != nil {
		t.Skip("requires python3 with quickjs for actual decode and render")
	}
	f := setup(t)
	_, ownerAuth, owner := f.account(t)
	_, foreignAuth, _ := f.account(t)
	api := busyAPI(f)
	t.Setenv("MQTT_PUBLIC_HOST", "127.0.0.1")
	t.Setenv("MQTT_PUBLIC_PORT", "1883")
	t.Setenv("MQTT_PUBLIC_SCHEME", "tcp")
	post := func(path, token string, payload any, want int) map[string]any {
		t.Helper()
		code, out, _ := req(t, api, "POST", path, "Bearer "+token, "", "", payload)
		if code != want {
			// Never dump a response that might contain issued credentials.
			t.Fatalf("POST %s: got %d, want %d", path, code, want)
		}
		return out
	}
	project := post("/api/v1/projects", ownerAuth.AccessToken, map[string]any{"name": "Journey project", "color": "mint"}, 201)
	projectID := project["id"].(string)
	email := memberEmail()
	addMember(t, api, ownerAuth.AccessToken, email, "viewer", []string{projectID})
	viewerAuth, _ := changeInitialPassword(t, f, api, email, owner.TenantID)
	gateway := post("/api/v1/gateways", ownerAuth.AccessToken, map[string]any{"name": "Journey MG3", "model": "minew-mg3", "project_id": projectID}, 201)
	gatewayID := gateway["gateway"].(map[string]any)["id"].(string)
	credentials := post("/api/v1/gateways/"+gatewayID+"/mqtt", ownerAuth.AccessToken, map[string]any{}, 201)
	password, _ := credentials["password"].(string)
	if password == "" || credentials["post_topic"] != "/aether/gateways/"+gatewayID+"/status" {
		t.Fatal("missing MQTT credentials or incorrect topic")
	}
	post("/api/v1/gateways/"+gatewayID+"/mqtt/rotate", foreignAuth.AccessToken, map[string]any{}, 404)
	post("/api/v1/rules", ownerAuth.AccessToken, map[string]any{"name": "Journey temperature", "event_type": "threshold", "scope": map[string]any{"metric": "temperature", "op": ">", "value": 24}, "severity": "warning", "channels": []string{}, "dedupe_sec": 600}, 201)

	// A unique identity prevents accidental matching against other test fixtures.
	mac := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	packet := []map[string]any{{"mac": mac, "rawData": "1016e1ffa1015a19803700" + mac, "aether_source": "simulated"}}
	finishMQTT := func() {}
	if os.Getenv("MQTT_JOURNEY_BROKER") != "" {
		finishMQTT = mqttJourney(t, f, api, ownerAuth.AccessToken, credentials, packet)
	} else {
		basic := "Basic " + base64.StdEncoding.EncodeToString([]byte(gatewayID+":"+gateway["token"].(string)))
		code, _, _ := req(t, api, "POST", "/ingest/gateways/"+gatewayID+"/packets", basic, "", "", packet)
		if code != 202 {
			t.Fatalf("authenticated packet ingestion: %d", code)
		}
	}
	code, sources := get(t, api, "/api/v1/studio/sources", ownerAuth.AccessToken)
	if code != 200 || !values(sources, "key")[gatewayID+"/"+mac] {
		t.Fatal("ingested device missing from Studio discovery")
	}
	panel := map[string]any{"id": "temperature", "title": "Journey", "widget_id": "official-environment-v1", "gateway_id": gatewayID, "external_id": mac, "width": 2, "height": 1}
	dashboard := post("/api/v1/studio/items", ownerAuth.AccessToken, map[string]any{"kind": "dashboard", "name": "Journey dashboard", "brand": "Any", "model": "Any", "version": 1, "visibility": "private", "definition": map[string]any{"project_id": projectID, "panels": []any{panel}}}, 201)
	code, saved := get(t, api, "/api/v1/studio/items?kind=dashboard", ownerAuth.AccessToken)
	if code != 200 || !values(saved, "id")[dashboard["id"].(string)] {
		t.Fatal("dashboard did not persist")
	}
	render := map[string]any{"panels": dashboard["definition"].(map[string]any)["panels"], "range": "1h"}
	rendered := post("/api/v1/studio/render", ownerAuth.AccessToken, render, 200)
	panels := listOf(rendered, "panels")
	if len(panels) != 1 || panels[0]["error"] != nil || !strings.Contains(panels[0]["html"].(string), "25.5") || panels[0]["source"] != "simulated" {
		t.Fatalf("actual widget decode/render failed: %v", panels)
	}
	foreignRender := post("/api/v1/studio/render", foreignAuth.AccessToken, render, 200)
	foreignPanels := listOf(foreignRender, "panels")
	if len(foreignPanels) != 1 || foreignPanels[0]["error"] == nil || foreignPanels[0]["html"] != nil {
		t.Fatal("another tenant rendered private sensor data")
	}
	// Studio is currently owner/admin only; a viewer uses Live and Alerts instead.
	post("/api/v1/studio/render", viewerAuth.AccessToken, render, 403)
	code, live := get(t, api, "/api/v1/live", viewerAuth.AccessToken)
	liveJSON, _ := json.Marshal(live)
	if code != 200 || !strings.Contains(string(liveJSON), mac) {
		t.Fatal("project viewer cannot see the ingested reading")
	}
	code, alerts := get(t, api, "/api/v1/alerts?status=open", viewerAuth.AccessToken)
	if code != 200 || len(rows(alerts)) != 1 || rows(alerts)[0]["external_id"] != mac {
		t.Fatalf("packet did not produce the expected project alert: %d %v", code, alerts)
	}
	code, foreignAlerts := get(t, api, "/api/v1/alerts?status=open", foreignAuth.AccessToken)
	if code != 200 || len(rows(foreignAlerts)) != 0 {
		t.Fatal("alert leaked across tenants")
	}
	finishMQTT()
}
