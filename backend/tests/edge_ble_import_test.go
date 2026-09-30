package tests

import (
	"aether/backend/internal/adapters/edge"
	"aether/backend/internal/adapters/tuyacloud"
	"aether/backend/internal/app"
	"aether/backend/internal/domain"
	"aether/backend/internal/tuyacloudlink/fakecloud"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
)

// Keys of the fake BLE project: local keys and sec_keys that must never be stored or shown in the clear.
var bleImportSecrets = []string{"FAKEKEY-bt01-000", "FAKEKEY-bt02-000", "SECKEY-bt01-0000", "SECKEY-bt02-0000", "FAKEKEY-sw01-000"}

// fakeBLECloud is a Tuya project with a Wi-Fi switch, two BLE sensors (one whose listing spells secKey, one with no
// factory record) and a Zigbee sensor behind a hub. factoryDown makes the factory-infos API refuse.
func fakeBLECloud(t *testing.T, factoryDown *atomic.Bool) *httptest.Server {
	t.Helper()
	read := func(name string) string {
		b, e := os.ReadFile(filepath.Join("..", "internal", "adapters", "tuyacloud", "testdata", name))
		if e != nil {
			t.Fatal(e)
		}
		return string(b)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("sign") == "" || r.Header.Get("client_id") != fakeAccessID {
			w.Write([]byte(`{"success":false,"code":1004,"msg":"sign invalid"}`))
			return
		}
		switch {
		case r.URL.Path == "/v1.0/token":
			w.Write([]byte(`{"success":true,"result":{"access_token":"faketoken-1","expire_time":7200,"uid":"az_fake_uid"}}`))
		case r.URL.Path == "/v1.0/iot-01/associated-users/devices":
			w.Write([]byte(`{"success":true,"result":{"has_more":false,"devices":[
  {"id":"bf00000000000000sw01","name":"สวิตช์","local_key":"FAKEKEY-sw01-000","category":"kg","product_id":"fakeprod0001","uuid":"uuidsw01"},
  {"id":"bf0000000000000bt01","name":"เซนเซอร์ห้อง 1","local_key":"FAKEKEY-bt01-000","category":"wsdcg","product_id":"blethermo01","uuid":"uuidbt01","sec_key":"SECKEY-bt01-0000"},
  {"id":"bf0000000000000bt02","name":"เซนเซอร์ห้อง 2","local_key":"FAKEKEY-bt02-000","category":"wsdcg","product_id":"blethermo01","uuid":"uuidbt02","secKey":"SECKEY-bt02-0000"},
  {"id":"bf00000000000000zb01","name":"Zigbee via hub","local_key":"FAKEKEY-zb01-000","category":"wsdcg","product_id":"fakeprod0003","sub":true}]}}`))
		case r.URL.Path == "/v1.0/iot-03/devices/factory-infos":
			if factoryDown != nil && factoryDown.Load() {
				w.Write([]byte(`{"success":false,"code":1106,"msg":"permission deny"}`))
				return
			}
			ids := r.URL.Query().Get("device_ids")
			if strings.Contains(ids, "zb01") {
				t.Errorf("factory records asked for a hub sub-device: %s", ids)
			}
			// bt01's address without separators and in upper case, as Tuya writes it; bt02 has no record.
			w.Write([]byte(`{"success":true,"result":[{"id":"bf00000000000000sw01","uuid":"uuidsw01","mac":"a4c138000001","sn":"1"},
  {"id":"bf0000000000000bt01","uuid":"uuidbt01","mac":"DC234D0000B1","sn":"2"}]}`))
		case strings.HasSuffix(r.URL.Path, "/model"):
			w.Write([]byte(`{"success":false,"code":28841101,"msg":"No permissions. This API is not subscribed."}`))
		case strings.HasSuffix(r.URL.Path, "/specifications"):
			w.Write([]byte(read("specifications_plug.json")))
		default:
			w.WriteHeader(404)
		}
	}))
}

func newBLEImportRig(t *testing.T, factoryDown *atomic.Bool) *importRig {
	t.Helper()
	r := newImportRig(t, "")
	srv := fakeBLECloud(t, factoryDown)
	t.Cleanup(srv.Close)
	r.f.service.TuyaCloud = func(region, id, secret string) (app.TuyaCloud, error) {
		if _, ok := tuyacloud.Regions[region]; !ok {
			return nil, errors.New("region")
		}
		return tuyacloud.NewForTest(srv.URL, id, secret, srv.Client(), nil), nil
	}
	r.f.service.EdgeBLEEnabled = true
	// Other tests migrate below 00043, which refuses while a BLE registration is live: remove them afterwards.
	t.Cleanup(func() {
		_, _ = r.f.admin.Exec(`UPDATE core.devices SET removed_at=now() WHERE gateway_id=$1 AND removed_at IS NULL`, r.gateway)
	})
	return r
}

// capture feeds one agent message (topic suffix under the gateway) through the ingest, as the collector would.
func (r *importRig) capture(suffix string, payload any) {
	r.t.Helper()
	body, _ := json.Marshal(payload)
	gateway, msg, e := edge.Route(edge.Prefix + r.gateway + "/" + suffix)
	if e != nil {
		r.t.Fatal(e)
	}
	if _, e := r.f.service.CaptureEdge(r.ctx, r.owner.TenantID, gateway, msg, body); e != nil {
		r.t.Fatal(e)
	}
}

func (r *importRig) tuyaDevices() map[string]map[string]any {
	r.t.Helper()
	code, list := r.call("GET", "/api/v1/gateways/"+r.gateway+"/tuya/devices", nil)
	if code != 200 {
		r.t.Fatalf("devices: %d %v", code, list)
	}
	out := map[string]map[string]any{}
	for _, raw := range list["items"].([]any) {
		d := raw.(map[string]any)
		out[d["tuya_id"].(string)] = d
	}
	return out
}

func (r *importRig) text(q string, args ...any) string {
	r.t.Helper()
	var s string
	if e := r.f.admin.QueryRowContext(r.ctx, q, args...).Scan(&s); e != nil {
		r.t.Fatal(e)
	}
	return s
}

// The import stores what finds a BLE device (its factory address, its uuid) and seals its sec_key; the Edge's
// Bluetooth sightings mark it a candidate (address in either byte order, or uuid); only an owner or admin sets it to
// BLE, after which it registers with the BLE profile and reaches the agent.
func TestTuyaBLEImportAndClassification(t *testing.T) {
	r := newBLEImportRig(t, nil)
	job := r.importJob("us")
	if job["status"] != "done" || job["found"] != float64(4) || job["factory_infos"] != "ok" || job["ble_candidates"] != float64(1) {
		t.Fatalf("job: %v", job)
	}
	for _, raw := range job["devices"].([]any) {
		d := raw.(map[string]any)
		switch d["tuya_id"] {
		case "bf0000000000000bt01":
			if d["ble_mac"] != "dc:23:4d:00:00:b1" || d["has_sec_key"] != true || d["ble_candidate"] != true || d["local_capable"] != false {
				t.Fatalf("bt01: %v", d)
			}
		case "bf0000000000000bt02":
			// A uuid alone is not a candidate until the Edge hears it.
			if d["ble_mac"] != nil || d["has_sec_key"] != true || d["ble_candidate"] != false {
				t.Fatalf("bt02 (uuid only, secKey spelling): %v", d)
			}
		case "bf00000000000000zb01":
			if d["ble_mac"] != nil || d["has_sec_key"] != false || d["ble_candidate"] != false {
				t.Fatalf("hub sub-device: %v", d)
			}
		case "bf00000000000000sw01":
			if d["ble_candidate"] != false || d["local_capable"] != true {
				t.Fatalf("Wi-Fi switch: %v", d)
			}
		}
	}
	if s := r.text(`SELECT ble_mac||'/'||ble_uuid||'/'||transport||'/'||(sec_key_sealed IS NOT NULL) FROM core.tuya_devices WHERE gateway_id=$1 AND tuya_id='bf0000000000000bt01'`, r.gateway); s != "dc:23:4d:00:00:b1/uuidbt01/wifi/true" {
		t.Fatalf("stored bt01: %s", s)
	}
	if s := r.text(`SELECT ble_mac||'/'||ble_uuid||'/'||(sec_key_sealed IS NOT NULL) FROM core.tuya_devices WHERE gateway_id=$1 AND tuya_id='bf0000000000000bt02'`, r.gateway); s != "/uuidbt02/true" {
		t.Fatalf("stored bt02: %s", s)
	}
	// Nothing is set to BLE by the import, and nothing is a candidate before the Edge hears it.
	devices := r.tuyaDevices()
	for id, d := range devices {
		if d["transport"] != "wifi" || d["detected"] != "unknown" || d["ble_seen"] != false {
			t.Fatalf("%s before sightings: %v", id, d)
		}
	}
	if devices["bf0000000000000bt01"]["ble_capable"] != true || devices["bf0000000000000bt02"]["ble_capable"] != false || devices["bf00000000000000zb01"]["ble_capable"] != false || devices["bf0000000000000bt01"]["has_sec_key"] != true {
		t.Fatalf("ble_capable/has_sec_key: %v %v", devices["bf0000000000000bt01"], devices["bf00000000000000zb01"])
	}
	// The Edge hears bt01 at its address byte-reversed and bt02 by its uuid at an address Tuya never gave; the
	// switch broadcasts on the LAN.
	r.capture("ble", []map[string]any{
		{"mac": "b1:00:00:4d:23:dc", "uuid": "", "product_id": "blethermo01", "proto": 3, "bound": true, "rssi": -61},
		{"mac": "aa:bb:cc:00:00:02", "uuid": "uuidbt02", "product_id": "blethermo01", "proto": 4, "bound": false, "rssi": -70},
		{"mac": "aa:bb:cc:00:00:99", "uuid": "uuidzz99", "product_id": "other", "proto": 3, "rssi": -80},
	})
	r.capture("discovery", []map[string]any{{"id": "bf00000000000000sw01", "ip": "192.168.1.20", "version": "3.3"}})
	devices = r.tuyaDevices()
	bt1, bt2 := devices["bf0000000000000bt01"], devices["bf0000000000000bt02"]
	if bt1["detected"] != "ble_candidate" || bt1["ble_seen"] != true || bt1["rssi"] != float64(-61) || bt1["ble_protocol"] != float64(3) || bt1["bound"] != true || bt1["transport"] != "wifi" {
		t.Fatalf("bt01 heard (reversed address): %v", bt1)
	}
	if bt2["detected"] != "ble_candidate" || bt2["ble_capable"] != true || bt2["rssi"] != float64(-70) || bt2["ble_protocol"] != float64(4) || bt2["bound"] != false {
		t.Fatalf("bt02 heard (uuid): %v", bt2)
	}
	if devices["bf00000000000000sw01"]["detected"] != "wifi" || devices["bf00000000000000zb01"]["detected"] != "unknown" {
		t.Fatalf("switch/sub: %v %v", devices["bf00000000000000sw01"], devices["bf00000000000000zb01"])
	}
	// Heard is not chosen: the BLE profile is refused until an owner or admin sets the device to BLE.
	if _, e := r.f.service.CreateDevice(r.ctx, r.owner, r.gateway, "Sensor 1", "bf0000000000000bt01", domain.TuyaBLEProfile); !errors.Is(e, domain.ErrInvalid) || !strings.Contains(e.Error(), "ble_not_confirmed") {
		t.Fatalf("BLE profile before confirming: %v", e)
	}
	if _, cfg, _ := r.agentWithCaps("ble"); len(cfg) != 0 {
		t.Fatalf("config before registering: %v", cfg)
	}
	// Owner and admin only; a viewer or operator is refused, and so are bad settings.
	for _, role := range []string{"viewer", "operator"} {
		p := r.owner
		p.Role = role
		if _, e := r.f.service.SetTuyaBLE(r.ctx, p, r.gateway, "bf0000000000000bt01", domain.TuyaBLESettings{Transport: "ble"}); !errors.Is(e, domain.ErrForbidden) {
			t.Fatalf("%s: %v", role, e)
		}
	}
	base := "/api/v1/gateways/" + r.gateway + "/tuya/devices/"
	for _, bad := range []map[string]any{
		{"transport": "ble", "poll_seconds": 60},
		{"transport": "ble", "poll_seconds": 90000},
		{"transport": "ble", "mode": "always"},
		{"transport": "zigbee"},
		{},
	} {
		if code, out := r.call("POST", base+"bf0000000000000bt01/ble", bad); code != 400 {
			t.Fatalf("%v: %d %v", bad, code, out)
		}
	}
	if code, out := r.call("POST", base+"bf00000000000000zb01/ble", map[string]any{"transport": "ble"}); code != 400 || out["error"] != "ble_address_unknown" {
		t.Fatalf("hub sub-device to BLE: %d %v", code, out)
	}
	code, out := r.call("POST", base+"bf0000000000000bt01/ble", map[string]any{"transport": "ble", "mode": "on_demand", "poll_seconds": 1800})
	if code != 200 || out["transport"] != "ble" || out["ble_mode"] != "on_demand" || out["ble_poll_seconds"] != float64(1800) {
		t.Fatalf("set BLE: %d %v", code, out)
	}
	if r.count(`SELECT count(*) FROM core.audit_logs WHERE action='tuya.transport_set_ble:bf0000000000000bt01' AND target_id=$1`, r.gateway) != 1 {
		t.Fatal("setting BLE not audited")
	}
	// Discovery offers it as the BLE profile now.
	code, disc := r.call("GET", "/api/v1/discovery", nil)
	found := false
	for _, raw := range disc["items"].([]any) {
		d := raw.(map[string]any)
		if d["external_id"] == "bf0000000000000bt01" {
			found = true
			profile, _ := d["profile"].(map[string]any)
			if d["source"] != "tuya_ble" || profile["id"] != domain.TuyaBLEProfile || d["transport"] != "ble" {
				t.Fatalf("discovery: %v", d)
			}
		}
		if d["external_id"] == "bf0000000000000bt02" && (d["source"] != "tuya" || d["ble_seen"] != true || d["profile"] != nil) {
			t.Fatalf("a heard sensor not set to BLE: %v", d)
		}
	}
	if code != 200 || !found || disc["edge_ble"] != true {
		t.Fatalf("discovery: %d %v", code, disc)
	}
	// Registered as BLE, it reaches an agent that can do BLE, keys opened only there.
	if _, e := r.f.service.CreateDevice(r.ctx, r.owner, r.gateway, "Sensor 1", "bf0000000000000bt01", domain.TuyaBLEProfile); e != nil {
		t.Fatal(e)
	}
	_, cfg, _ := r.agentWithCaps("ble")
	if len(cfg) != 1 || cfg[0]["transport"] != "ble" || cfg[0]["mac"] != "dc:23:4d:00:00:b1" || cfg[0]["sec_key"] != "SECKEY-bt01-0000" || cfg[0]["poll"] != float64(1800) {
		t.Fatalf("config: %v", cfg)
	}
	// The transport of a registered device does not change under it.
	if code, out := r.call("POST", base+"bf0000000000000bt01/ble", map[string]any{"transport": "wifi"}); code != 409 || out["error"] != "registered" {
		t.Fatalf("change a registered device: %d %v", code, out)
	}
	if code, out := r.call("POST", base+"bf0000000000000bt01/ble", map[string]any{"poll_seconds": 3600}); code != 200 || out["ble_poll_seconds"] != float64(3600) {
		t.Fatalf("poll of a registered device: %d %v", code, out)
	}
	// Set to BLE, a device cannot be registered as Wi-Fi.
	if code, _ := r.call("POST", base+"bf0000000000000bt02/ble", map[string]any{"transport": "ble"}); code != 200 {
		t.Fatalf("bt02 to BLE: %d", code)
	}
	if _, e := r.f.service.CreateDevice(r.ctx, r.owner, r.gateway, "Sensor 2", "bf0000000000000bt02", domain.TuyaWiFiProfile); !errors.Is(e, domain.ErrInvalid) || !strings.Contains(e.Error(), "set_to_ble") {
		t.Fatalf("Wi-Fi profile for a BLE device: %v", e)
	}
	// A lock is shown read-only.
	if _, e := r.f.admin.ExecContext(r.ctx, `UPDATE core.tuya_devices SET tuya_category='jtmspro' WHERE gateway_id=$1 AND tuya_id='bf0000000000000bt02'`, r.gateway); e != nil {
		t.Fatal(e)
	}
	if d := r.tuyaDevices()["bf0000000000000bt02"]; d["readonly"] != true {
		t.Fatalf("lock: %v", d)
	}
	// Another workspace sees none of it and changes nothing.
	other := newBLEImportRig(t, nil)
	if code, out := other.call("GET", "/api/v1/gateways/"+r.gateway+"/tuya/devices", nil); code == 200 && len(out["items"].([]any)) != 0 {
		t.Fatalf("another workspace lists this gateway's devices: %v", out)
	}
	if code, _ := other.call("POST", base+"bf0000000000000bt02/ble", map[string]any{"transport": "wifi"}); code != 404 {
		t.Fatalf("another workspace changed a device: %d", code)
	}
	// EDGE_BLE off: nothing to set, and discovery hides BLE adoption (and says so).
	r.f.service.EdgeBLEEnabled = false
	if code, out := r.call("POST", base+"bf0000000000000bt02/ble", map[string]any{"transport": "wifi"}); code != 400 || out["error"] != "edge_ble_off" {
		t.Fatalf("flag off: %d %v", code, out)
	}
	if _, e := r.f.admin.ExecContext(r.ctx, `UPDATE core.devices SET removed_at=now() WHERE gateway_id=$1`, r.gateway); e != nil {
		t.Fatal(e)
	}
	_, disc = r.call("GET", "/api/v1/discovery", nil)
	for _, raw := range disc["items"].([]any) {
		if raw.(map[string]any)["source"] == "tuya_ble" {
			t.Fatalf("BLE adoption offered with EDGE_BLE off: %v", raw)
		}
	}
	if disc["edge_ble"] != false || disc["hidden_ble"] != float64(2) {
		t.Fatalf("discovery with the flag off: %v %v", disc["edge_ble"], disc["hidden_ble"])
	}
	// Nothing secret in the clear, in the database or any response.
	for _, secret := range bleImportSecrets {
		for _, table := range []string{"core.tuya_devices", "core.audit_logs", "core.gateway_packets", "core.edge_agents", "core.edge_ble_seen"} {
			if n := r.count(`SELECT count(*) FROM `+table+` x WHERE x::text LIKE '%'||$1||'%'`, secret); n != 0 {
				t.Fatalf("%s stored in the clear in %s", secret, table)
			}
		}
		for _, body := range r.bodies {
			if strings.Contains(body, secret) {
				t.Fatalf("%s in an API response: %s", secret, body)
			}
		}
	}
}

// Factory records are optional: without them the import still succeeds (BLE devices then match by uuid), and a
// re-import keeps the address it already knew and the operator's choice of transport; the sec_key follows the
// latest import.
func TestTuyaBLEReimportWithoutFactoryRecords(t *testing.T) {
	down := &atomic.Bool{}
	r := newBLEImportRig(t, down)
	r.importJob("us")
	if code, _ := r.call("POST", "/api/v1/gateways/"+r.gateway+"/tuya/devices/bf0000000000000bt01/ble", map[string]any{"transport": "ble"}); code != 200 {
		t.Fatal(code)
	}
	down.Store(true)
	job := r.importJob("us")
	if job["status"] != "done" || job["factory_infos"] != "unavailable" || job["imported"] != float64(4) {
		t.Fatalf("job without factory records: %v", job)
	}
	if s := r.text(`SELECT ble_mac||'/'||transport||'/'||(sec_key_sealed IS NOT NULL) FROM core.tuya_devices WHERE gateway_id=$1 AND tuya_id='bf0000000000000bt01'`, r.gateway); s != "dc:23:4d:00:00:b1/ble/true" {
		t.Fatalf("after re-import: %s", s)
	}
	// Forgetting the key drops the sec_key with it, audited with the device named.
	if code, _ := r.call("POST", "/api/v1/gateways/"+r.gateway+"/tuya/devices/bf0000000000000bt01/forget", nil); code != 204 {
		t.Fatal(code)
	}
	if r.count(`SELECT count(*) FROM core.audit_logs WHERE action='tuya.key_forgotten:bf0000000000000bt01' AND target_id=$1`, r.gateway) != 1 {
		t.Fatal("forgetting not audited with the device")
	}
	if n := r.count(`SELECT count(*) FROM core.tuya_devices WHERE gateway_id=$1 AND tuya_id='bf0000000000000bt01' AND sec_key_sealed IS NULL AND local_key_sealed IS NULL`, r.gateway); n != 1 {
		t.Fatal("sec_key kept after forgetting the key")
	}
}

// agentWithCaps pulls the agent's configuration announcing the given capabilities, and returns its devices.
func (r *importRig) agentWithCaps(caps string) (int, []map[string]any, string) {
	r.t.Helper()
	q := httptest.NewRequest("GET", "/ingest/gateways/"+r.gateway+"/edge/config", nil)
	q.SetBasicAuth(r.gateway, r.gwKey)
	q.Header.Set("X-Aether-Edge-Caps", caps)
	res, e := r.api.Test(q, fiber.TestConfig{Timeout: 15 * time.Second})
	if e != nil {
		r.t.Fatal(e)
	}
	defer res.Body.Close()
	var body struct {
		Devices []map[string]any `json:"devices"`
	}
	_ = json.NewDecoder(res.Body).Decode(&body)
	return res.StatusCode, body.Devices, res.Header.Get("ETag")
}

// A sighting records the advertised protocol only while none is known: later sightings (anyone in radio range can
// send one) do not change it.
func TestTuyaBLESightingKeepsKnownProtocol(t *testing.T) {
	r := newBLEImportRig(t, nil)
	r.importJob("us")
	proto := func() string {
		return r.text(`SELECT ble_protocol::text FROM core.tuya_devices WHERE gateway_id=$1 AND tuya_id='bf0000000000000bt01'`, r.gateway)
	}
	if code, _ := r.call("POST", "/api/v1/gateways/"+r.gateway+"/tuya/devices/bf0000000000000bt01/ble", map[string]any{"transport": "ble"}); code != 200 {
		t.Fatal(code)
	}
	r.capture("ble", []map[string]any{{"mac": "dc:23:4d:00:00:b1", "proto": 3, "rssi": -60}})
	if p := proto(); p != "3" {
		t.Fatalf("first sighting: %s", p)
	}
	// The next list is accepted only after the 30 s interval: make the last one older.
	if _, e := r.f.admin.ExecContext(r.ctx, `UPDATE core.edge_agents SET last_ble_at=now()-interval '1 minute' WHERE gateway_id=$1`, r.gateway); e != nil {
		t.Fatal(e)
	}
	r.capture("ble", []map[string]any{{"mac": "dc:23:4d:00:00:b1", "proto": 4, "rssi": -55}})
	if p := proto(); p != "3" {
		t.Fatalf("a later sighting changed the protocol: %s", p)
	}
	if n := r.count(`SELECT count(*) FROM core.tuya_devices WHERE gateway_id=$1 AND tuya_id='bf0000000000000bt01' AND rssi=-55`, r.gateway); n != 1 {
		t.Fatal("signal not updated")
	}
}

// Restoring a Tuya registration checks its device's transport as creating one does: a BLE registration whose device
// was set back to Wi-Fi, or a Wi-Fi one whose device was set to BLE, is not restored.
func TestTuyaBLERestoreFollowsTransport(t *testing.T) {
	r := newBLEImportRig(t, nil)
	r.importJob("us")
	base := "/api/v1/gateways/" + r.gateway + "/tuya/devices/"
	if code, _ := r.call("POST", base+"bf0000000000000bt01/ble", map[string]any{"transport": "ble"}); code != 200 {
		t.Fatal(code)
	}
	ble, e := r.f.service.CreateDevice(r.ctx, r.owner, r.gateway, "Sensor 1", "bf0000000000000bt01", domain.TuyaBLEProfile)
	if e != nil {
		t.Fatal(e)
	}
	wifi, e := r.f.service.CreateDevice(r.ctx, r.owner, r.gateway, "Switch", "bf00000000000000sw01", domain.TuyaWiFiProfile)
	if e != nil {
		t.Fatal(e)
	}
	for _, id := range []string{ble.ID, wifi.ID} {
		if e := r.f.service.RemoveDevice(r.ctx, r.owner, id); e != nil {
			t.Fatal(e)
		}
	}
	if code, out := r.call("POST", base+"bf0000000000000bt01/ble", map[string]any{"transport": "wifi"}); code != 200 {
		t.Fatalf("bt01 back to Wi-Fi: %d %v", code, out)
	}
	if code, out := r.call("POST", base+"bf00000000000000sw01/ble", map[string]any{"transport": "ble"}); code != 200 {
		t.Fatalf("switch to BLE: %d %v", code, out)
	}
	if e := r.f.service.RestoreDevice(r.ctx, r.owner, ble.ID); !errors.Is(e, domain.ErrInvalid) || !strings.Contains(e.Error(), "ble_not_confirmed") {
		t.Fatalf("restore a BLE registration of a Wi-Fi device: %v", e)
	}
	if e := r.f.service.RestoreDevice(r.ctx, r.owner, wifi.ID); !errors.Is(e, domain.ErrInvalid) || !strings.Contains(e.Error(), "set_to_ble") {
		t.Fatalf("restore a Wi-Fi registration of a BLE device: %v", e)
	}
	if n := r.count(`SELECT count(*) FROM core.devices WHERE gateway_id=$1 AND removed_at IS NULL`, r.gateway); n != 0 {
		t.Fatalf("%d registrations restored", n)
	}
	// Set back as registered, both restore.
	r.call("POST", base+"bf0000000000000bt01/ble", map[string]any{"transport": "ble"})
	r.call("POST", base+"bf00000000000000sw01/ble", map[string]any{"transport": "wifi"})
	for _, id := range []string{ble.ID, wifi.ID} {
		if e := r.f.service.RestoreDevice(r.ctx, r.owner, id); e != nil {
			t.Fatalf("restore %s: %v", id, e)
		}
	}
}

// A registration and a transport change of the same device are serialised on the gateway row: a registration
// waiting behind a change that sets the device back to Wi-Fi sees the committed transport and is refused.
func TestTuyaBLERegistrationWaitsForTransportChange(t *testing.T) {
	r := newBLEImportRig(t, nil)
	r.importJob("us")
	if code, _ := r.call("POST", "/api/v1/gateways/"+r.gateway+"/tuya/devices/bf0000000000000bt01/ble", map[string]any{"transport": "ble"}); code != 200 {
		t.Fatal(code)
	}
	// Stand in for SetTuyaBLE: hold the gateway row FOR UPDATE, then flip the device to Wi-Fi before committing.
	tx, e := r.f.admin.BeginTx(r.ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	if _, e := tx.Exec(`SELECT 1 FROM core.gateways WHERE id=$1 FOR UPDATE`, r.gateway); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() {
		_, e := r.f.service.CreateDevice(r.ctx, r.owner, r.gateway, "Sensor 1", "bf0000000000000bt01", domain.TuyaBLEProfile)
		done <- e
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		// Any statement of the registration blocked on a lock (the gateway row, whichever statement reaches it first).
		if r.count(`SELECT count(*) FROM pg_stat_activity WHERE wait_event_type='Lock' AND datname=current_database() AND usename='aether_app'`) > 0 {
			break
		}
		select {
		case e := <-done:
			t.Fatalf("the registration did not wait for the gateway row: %v", e)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the registration never waited on the gateway row")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, e := tx.Exec(`UPDATE core.tuya_devices SET transport='wifi' WHERE gateway_id=$1 AND tuya_id='bf0000000000000bt01'`, r.gateway); e != nil {
		t.Fatal(e)
	}
	if e := tx.Commit(); e != nil {
		t.Fatal(e)
	}
	if e := <-done; !errors.Is(e, domain.ErrInvalid) || !strings.Contains(e.Error(), "ble_not_confirmed") {
		t.Fatalf("registration after the change: %v", e)
	}
}

// A project-restricted admin cannot see or change another project's Edge devices.
func TestTuyaBLEProjectScope(t *testing.T) {
	r := newBLEImportRig(t, nil)
	a, e := r.f.service.CreateProject(r.ctx, r.owner, "อาคาร A", "", "mint")
	if e != nil {
		t.Fatal(e)
	}
	b, e := r.f.service.CreateProject(r.ctx, r.owner, "อาคาร B", "", "blue")
	if e != nil {
		t.Fatal(e)
	}
	gw, _, e := r.f.service.CreateGatewayIn(r.ctx, r.owner, "Edge B", domain.EdgeGatewayModel, &b.ID)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := r.f.admin.ExecContext(r.ctx, `INSERT INTO core.tuya_devices(tenant_id,gateway_id,tuya_id,name,local_key_sealed,key_fingerprint,key_status,ble_mac)
    VALUES($1,$2,'bf0000000000000pb01','B sensor','sealed','0123456789abcdef','ok','dc:23:4d:00:00:e1')`, r.owner.TenantID, gw.ID); e != nil {
		t.Fatal(e)
	}
	email := memberEmail()
	addMember(t, r.api, strings.TrimPrefix(r.token, "Bearer "), email, "admin", []string{a.ID})
	auth, _ := changeInitialPassword(t, r.f, r.api, email, r.owner.TenantID)
	scoped := &importRig{f: r.f, api: r.api, ctx: r.ctx, token: "Bearer " + auth.AccessToken, gateway: gw.ID, t: t}
	if code, out := scoped.call("POST", "/api/v1/gateways/"+gw.ID+"/tuya/devices/bf0000000000000pb01/ble", map[string]any{"transport": "ble"}); code != 404 {
		t.Fatalf("another project's device: %d %v", code, out)
	}
	if code, out := scoped.call("GET", "/api/v1/gateways/"+gw.ID+"/tuya/devices", nil); code == 200 && len(out["items"].([]any)) != 0 {
		t.Fatalf("another project's devices listed: %v", out)
	}
	if s := r.text(`SELECT transport FROM core.tuya_devices WHERE gateway_id=$1`, gw.ID); s != "wifi" {
		t.Fatalf("transport changed: %s", s)
	}
}

// The real import job against the fake Tuya OpenAPI (fakecloud): factory records, uuid, both sec_key spellings; a
// sec_key without a local key, a placeholder address and a hub sub-device are all left out.
func TestTuyaBLEImportThroughFakeCloud(t *testing.T) {
	r := newImportRig(t, "")
	api := fakecloud.NewAPI(t, fakeAccessID)
	r.f.service.TuyaCloud = func(region, id, secret string) (app.TuyaCloud, error) {
		c, e := api.Client(region, id, secret)
		if e != nil {
			return nil, e
		}
		return c, nil
	}
	spec := cloudSpec(t, "wsdcg_sleeper.json")
	api.SetDevices(
		fakecloud.CloudDevice{ID: "bf0000000000000fc01", Name: "snake", Category: "wsdcg", ProductID: "fcp", Spec: spec, LocalKey: "FAKEKEY-fc01-000", UUID: "uuidfc01", SecKey: "SECKEY-fc01-0000", MAC: "DC234D0000F1"},
		fakecloud.CloudDevice{ID: "bf0000000000000fc02", Name: "camel", Category: "wsdcg", ProductID: "fcp", Spec: spec, LocalKey: "FAKEKEY-fc02-000", UUID: "uuidfc02", SecKey: "SECKEY-fc02-0000", SecKeyCamel: true, MAC: "dc:23:4d:00:00:f2"},
		fakecloud.CloudDevice{ID: "bf0000000000000fc03", Name: "no local key", Category: "wsdcg", ProductID: "fcp", Spec: spec, UUID: "uuidfc03", SecKey: "SECKEY-fc03-0000", MAC: "DC234D0000F3"},
		fakecloud.CloudDevice{ID: "bf0000000000000fc04", Name: "zero address", Category: "wsdcg", ProductID: "fcp", Spec: spec, LocalKey: "FAKEKEY-fc04-000", MAC: "000000000000"},
		fakecloud.CloudDevice{ID: "bf0000000000000fc05", Name: "behind a hub", Category: "wsdcg", ProductID: "fcp", Spec: spec, LocalKey: "FAKEKEY-fc05-000", UUID: "uuidfc05", MAC: "DC234D0000F5", Sub: true},
	)
	job := r.importJob("us")
	if job["status"] != "done" || job["found"] != float64(5) || job["factory_infos"] != "ok" || job["ble_candidates"] != float64(2) || api.Calls("factory") != 1 {
		t.Fatalf("job: %v (factory calls %d)", job, api.Calls("factory"))
	}
	row := func(id string) string {
		return r.text(`SELECT ble_mac||'/'||ble_uuid||'/'||(sec_key_sealed IS NOT NULL)||'/'||(local_key_sealed IS NOT NULL) FROM core.tuya_devices WHERE gateway_id=$1 AND tuya_id=$2`, r.gateway, id)
	}
	for id, want := range map[string]string{
		"bf0000000000000fc01": "dc:23:4d:00:00:f1/uuidfc01/true/true",
		"bf0000000000000fc02": "dc:23:4d:00:00:f2/uuidfc02/true/true",
		"bf0000000000000fc03": "dc:23:4d:00:00:f3/uuidfc03/false/false",
		"bf0000000000000fc04": "//false/true",
		"bf0000000000000fc05": "//false/true",
	} {
		if got := row(id); got != want {
			t.Fatalf("%s: %s, want %s", id, got, want)
		}
	}
	for _, secret := range []string{"FAKEKEY-fc01-000", "SECKEY-fc01-0000", "SECKEY-fc02-0000", "SECKEY-fc03-0000"} {
		if n := r.count(`SELECT count(*) FROM core.tuya_devices x WHERE x::text LIKE '%'||$1||'%'`, secret); n != 0 {
			t.Fatalf("%s stored in the clear", secret)
		}
		for _, body := range r.bodies {
			if strings.Contains(body, secret) {
				t.Fatalf("%s in a response", secret)
			}
		}
	}
}
