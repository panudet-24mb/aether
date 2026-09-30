package tests

import (
	"aether/backend/internal/adapters/edge"
	"aether/backend/internal/adapters/postgres"
	"aether/backend/internal/adapters/tuya"
	"aether/backend/internal/commander"
	"aether/backend/internal/domain"
	"aether/backend/internal/security"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/pressly/goose/v3"
)

const (
	bleLocalKey = "BLEKEY-th01-0000"
	bleSecKey   = "BLESEC-th01-0000"
	bleMAC      = "dc:23:4d:00:00:01"
	bleUUID     = "tuya1234abcd5678"
)

// bleRig is an edgeRig whose thermostat is a Tuya BLE device (Transport "ble", real sealed keys), plus the Edge
// gateway's HTTP token for the agent's configuration pull.
type bleRig struct {
	*edgeRig
	gwKey string
}

func newBLERig(t *testing.T) *bleRig {
	t.Helper()
	f := setup(t)
	f.service.EdgeBLEEnabled = true
	r := &edgeRig{f: f, api: busyAPI(f), ctx: t.Context(), ids: map[string]string{}, devices: map[string]string{}, t: t}
	_, r.ownerAuth, r.owner = f.account(t)
	g, key, e := f.service.CreateGatewayIn(r.ctx, r.owner, "Edge BLE", domain.EdgeGatewayModel, nil)
	if e != nil {
		t.Fatal(e)
	}
	r.gateway = g.ID
	r.dispatcher = &commander.Dispatcher{Store: rigStore{f.repo, r.owner.TenantID}, Publisher: r, Now: time.Now}
	var imports []postgres.TuyaImport
	for _, d := range edgeDevices {
		if d.name != "thermo" && d.name != "switch" {
			continue
		}
		raw, e := os.ReadFile("../internal/adapters/tuya/testdata/" + d.fixture)
		if e != nil {
			t.Fatal(e)
		}
		dps, category, e := tuya.ParseSpecifications(raw)
		if e != nil {
			t.Fatal(e)
		}
		sealed, e := security.Seal(f.service.TuyaKeys, bleLocalKey)
		if e != nil {
			t.Fatal(e)
		}
		r.ids[d.name] = d.id
		imports = append(imports, postgres.TuyaImport{TuyaID: d.id, Name: "Tuya " + d.name, Category: category, ProductID: "gvygg3m8", Spec: dps,
			LocalKeySealed: sealed, KeyFingerprint: tuya.Fingerprint(bleLocalKey)})
	}
	if n, e := f.repo.SaveTuyaDevices(r.ctx, r.owner, g.ID, imports); e != nil || n != len(imports) {
		t.Fatalf("import: %d %v", n, e)
	}
	// B3 will set these from the import and the sightings; here the thermostat is made a BLE device by hand.
	sec, e := security.Seal(f.service.TuyaKeys, bleSecKey)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := f.admin.ExecContext(r.ctx, `UPDATE core.tuya_devices SET transport='ble',ble_mac=$1,ble_uuid=$2,sec_key_sealed=$3,ble_protocol=3,ble_mode='on_demand',ble_poll_seconds=1800
    WHERE gateway_id=$4 AND tuya_id=$5`, bleMAC, bleUUID, sec, g.ID, r.ids["thermo"]); e != nil {
		t.Fatal(e)
	}
	// Other tests migrate below 00043, which refuses while a BLE registration is live: remove it afterwards.
	t.Cleanup(func() {
		_, _ = f.admin.Exec(`UPDATE core.devices SET removed_at=now() WHERE gateway_id=$1 AND removed_at IS NULL`, g.ID)
	})
	return &bleRig{edgeRig: r, gwKey: key}
}

func (r *bleRig) registerBLE() string {
	r.t.Helper()
	d, e := r.f.service.CreateDevice(r.ctx, r.owner, r.gateway, "Tuya thermo", r.ids["thermo"], domain.TuyaBLEProfile)
	if e != nil {
		r.t.Fatalf("register: %v", e)
	}
	r.devices["thermo"] = d.ID
	return d.ID
}

// pull is the agent's configuration pull, with the capabilities it announces.
func (r *bleRig) pull(caps, etag string) (int, []map[string]any, string) {
	r.t.Helper()
	q := httptest.NewRequest("GET", "/ingest/gateways/"+r.gateway+"/edge/config", nil)
	q.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(r.gateway+":"+r.gwKey)))
	if caps != "" {
		q.Header.Set("X-Aether-Edge-Caps", caps)
	}
	if etag != "" {
		q.Header.Set("If-None-Match", etag)
	}
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

// BLE devices reach only an agent that announced Bluetooth, and only while EDGE_BLE is on; the ETag says which
// answer the agent holds, so a change of either side is never answered with a stale 304.
func TestEdgeConfigBLE(t *testing.T) {
	r := newBLERig(t)
	r.registerBLE()
	r.register("switch")
	ids := func(devices []map[string]any) string {
		var out []string
		for _, d := range devices {
			out = append(out, d["id"].(string))
		}
		return strings.Join(out, ",")
	}
	// An agent without the capability (0.1.x): Wi-Fi devices only, and no BLE fields at all.
	code, devices, plain := r.pull("", "")
	if code != 200 || ids(devices) != r.ids["switch"] || strings.Contains(plain, "ble") {
		t.Fatalf("no capability: %d %v %q", code, devices, plain)
	}
	if _, ok := devices[0]["transport"]; ok {
		t.Fatalf("Wi-Fi device carries BLE fields: %v", devices[0])
	}
	// With it: the BLE device too, with its keys opened, its address and the data-point types.
	code, devices, withBLE := r.pull("ble", plain)
	if code != 200 || ids(devices) != r.ids["switch"]+","+r.ids["thermo"] || withBLE == plain || !strings.Contains(withBLE, "ble") {
		t.Fatalf("with capability: %d %v %q", code, devices, withBLE)
	}
	d := devices[1]
	if d["transport"] != "ble" || d["key"] != bleLocalKey || d["sec_key"] != bleSecKey || d["mac"] != bleMAC || d["uuid"] != bleUUID ||
		d["product_id"] != "gvygg3m8" || d["protocol"] != float64(3) || d["mode"] != "on_demand" || d["poll"] != float64(1800) {
		t.Fatalf("BLE device: %v", d)
	}
	types := d["dp_types"].(map[string]any)
	if types["1"] != "bool" || types["2"] != "value" || types["4"] != "enum" {
		t.Fatalf("dp types: %v", types)
	}
	if code, _, _ := r.pull("ble", withBLE); code != 304 {
		t.Fatalf("unchanged: %d", code)
	}
	if caps := r.text(`SELECT array_to_string(capabilities,',') FROM core.edge_agents WHERE gateway_id=$1`, r.gateway); caps != "ble" {
		t.Fatalf("stored capabilities %q", caps)
	}
	// Unknown capabilities are not stored.
	r.pull("ble, teleport,,BLE", "")
	if caps := r.text(`SELECT array_to_string(capabilities,',') FROM core.edge_agents WHERE gateway_id=$1`, r.gateway); caps != "ble" {
		t.Fatalf("stored capabilities %q", caps)
	}
	// EDGE_BLE off: the same agent gets the Wi-Fi answer, and its BLE ETag no longer matches.
	r.f.service.EdgeBLEEnabled = false
	code, devices, tag := r.pull("ble", withBLE)
	if code != 200 || ids(devices) != r.ids["switch"] || tag != plain {
		t.Fatalf("flag off: %d %v %q", code, devices, tag)
	}
	// The BLE profile does not exist without the flag.
	if _, e := r.f.service.CreateDevice(r.ctx, r.owner, r.gateway, "x", "bf0000000000000000x1", domain.TuyaBLEProfile); !errors.Is(e, domain.ErrInvalid) {
		t.Fatalf("BLE profile without the flag: %v", e)
	}
	for _, p := range r.f.service.DeviceProfiles() {
		if p.ID == domain.TuyaBLEProfile {
			t.Fatal("catalog lists the BLE profile without the flag")
		}
	}
	r.f.service.EdgeBLEEnabled = true
	// A BLE device needs an address or a uuid; without either it is not sent.
	if _, e := r.f.admin.ExecContext(r.ctx, `UPDATE core.tuya_devices SET ble_mac='',ble_uuid='' WHERE gateway_id=$1 AND tuya_id=$2`, r.gateway, r.ids["thermo"]); e != nil {
		t.Fatal(e)
	}
	if _, devices, _ = r.pull("ble", ""); ids(devices) != r.ids["switch"] {
		t.Fatalf("BLE device without address: %v", devices)
	}
	// Nothing secret in the clear in the database.
	for _, secret := range []string{bleLocalKey, bleSecKey} {
		if n := r.count(`SELECT count(*) FROM core.tuya_devices x WHERE x::text LIKE '%'||$1||'%'`, secret); n != 0 {
			t.Fatalf("%s stored in the clear", secret)
		}
	}
}

// A BLE device reports enums by index: the ingest turns them into labels. A command to it sends the index, and the
// device's echo confirms it; a report counts as a read.
func TestEdgeBLEStateAndCommand(t *testing.T) {
	r := newBLERig(t)
	r.registerBLE()
	r.report("thermo", map[int]any{1: true, 2: 200, 4: 1})
	if s := r.text(`SELECT state->>'mode' FROM core.tuya_devices WHERE gateway_id=$1 AND tuya_id=$2`, r.gateway, r.ids["thermo"]); s != "manual" {
		t.Fatalf("mode from index 1: %q", s)
	}
	if n := r.count(`SELECT count(*) FROM core.tuya_devices WHERE gateway_id=$1 AND tuya_id=$2 AND last_read_at IS NOT NULL`, r.gateway, r.ids["thermo"]); n != 1 {
		t.Fatal("last_read_at not set")
	}
	// An index outside the range is dropped, not stored as a label.
	r.report("thermo", map[int]any{4: 7})
	if s := r.text(`SELECT state->>'mode' FROM core.tuya_devices WHERE gateway_id=$1 AND tuya_id=$2`, r.gateway, r.ids["thermo"]); s != "manual" {
		t.Fatalf("mode after a bad index: %q", s)
	}
	code, out := r.send(r.ownerAuth.AccessToken, map[string]any{"device_id": r.devices["thermo"], "property": "mode", "value": "auto"})
	if code != 202 || out["transport"] != "edge" {
		t.Fatalf("command: %d %v", code, out)
	}
	r.published = nil
	if n := r.dispatch(); n != 1 {
		t.Fatalf("published %d", n)
	}
	want := edge.Prefix + r.gateway + "/" + r.ids["thermo"] + `/set {"dps":{"4":0}}`
	if len(r.published) != 1 || r.published[0] != want {
		t.Fatalf("wire: %v, want %s", r.published, want)
	}
	if s := r.text(`SELECT status FROM core.device_commands WHERE id=$1`, out["id"]); s != "confirmed" {
		t.Fatalf("status %s", s)
	}
	// A BLE command waits 45 s for its confirmation, and tells the agent it has 30 s.
	if n := r.count(`SELECT confirm_sec FROM core.device_commands WHERE id=$1`, out["id"]); n != 45 || r.ttls[len(r.ttls)-1] != 30000 {
		t.Fatalf("confirm window %d, ttl %v", n, r.ttls)
	}
	// Numbers and booleans are the same as over the LAN.
	code, out = r.send(r.ownerAuth.AccessToken, map[string]any{"device_id": r.devices["thermo"], "property": "temp_set", "value": 21.5})
	if code != 202 {
		t.Fatalf("temp_set: %d %v", code, out)
	}
	r.published = nil
	r.dispatch()
	if want := edge.Prefix + r.gateway + "/" + r.ids["thermo"] + `/set {"dps":{"2":215}}`; len(r.published) != 1 || r.published[0] != want {
		t.Fatalf("wire: %v", r.published)
	}
}

// Sightings are stored per gateway (address, uuid, product, protocol, signal), bounded per message and per
// interval, visible only within the workspace and its projects; an imported BLE device heard gets its signal.
func TestEdgeBLESightings(t *testing.T) {
	r := newBLERig(t)
	var list []map[string]any
	for i := 0; i < 250; i++ {
		list = append(list, map[string]any{"mac": fmt.Sprintf("DC:23:4D:00:%02X:%02X", i/256, i%256), "uuid": fmt.Sprintf("uuid%012d", i),
			"product_id": "gvygg3m8", "proto": 3, "bound": true, "rssi": -60})
	}
	list[0]["mac"], list[0]["uuid"] = bleMAC, bleUUID
	list[1]["mac"] = "not-an-address"
	list[2]["uuid"] = "bad uuid!"
	list[3]["rssi"] = 99
	r.must(r.capture("ble", list))
	if n := r.count(`SELECT count(*) FROM core.edge_ble_seen WHERE gateway_id=$1`, r.gateway); n != 200 {
		t.Fatalf("kept %d sightings", n)
	}
	if n := r.count(`SELECT count(*) FROM core.edge_ble_seen WHERE gateway_id=$1 AND rssi IS NULL`, r.gateway); n != 1 {
		t.Fatalf("out-of-range signal kept: %d", n)
	}
	if n := r.count(`SELECT count(*) FROM core.tuya_devices WHERE gateway_id=$1 AND tuya_id=$2 AND rssi=-60`, r.gateway, r.ids["thermo"]); n != 1 {
		t.Fatal("the imported BLE device did not get its signal")
	}
	// A second list within the interval is acknowledged and dropped.
	r.must(r.capture("ble", []map[string]any{{"mac": "aa:bb:cc:dd:ee:ff", "proto": 4}}))
	if n := r.count(`SELECT count(*) FROM core.edge_ble_seen WHERE gateway_id=$1 AND mac='aa:bb:cc:dd:ee:ff'`, r.gateway); n != 0 {
		t.Fatal("a list inside the interval was stored")
	}
	// Row level security: the runtime role sees them in their workspace only, and cannot write another's.
	other := newBLERig(t)
	asTenant := func(tenant string, q string, args ...any) (int, error) {
		tx, e := r.f.runtime.BeginTx(r.ctx, nil)
		if e != nil {
			return 0, e
		}
		defer tx.Rollback()
		if _, e := tx.Exec(`SELECT set_config('app.user_id','',true),set_config('app.tenant_id',$1,true),set_config('app.project_scope','*',true)`, tenant); e != nil {
			return 0, e
		}
		if strings.HasPrefix(q, "SELECT") {
			var n int
			return n, tx.QueryRow(q, args...).Scan(&n)
		}
		_, e = tx.Exec(q, args...)
		return 0, e
	}
	if n, e := asTenant(r.owner.TenantID, `SELECT count(*) FROM core.edge_ble_seen WHERE gateway_id=$1`, r.gateway); e != nil || n != 200 {
		t.Fatalf("own workspace: %d %v", n, e)
	}
	if n, e := asTenant(other.owner.TenantID, `SELECT count(*) FROM core.edge_ble_seen WHERE gateway_id=$1`, r.gateway); e != nil || n != 0 {
		t.Fatalf("another workspace sees %d sightings (%v)", n, e)
	}
	if _, e := asTenant(other.owner.TenantID, `INSERT INTO core.edge_ble_seen(tenant_id,gateway_id,mac) VALUES($1,$2,'aa:bb:cc:dd:ee:01')`, r.owner.TenantID, r.gateway); e == nil {
		t.Fatal("another workspace wrote a sighting under this gateway")
	}
	// Health carries the radio's state, shown on the gateway page.
	r.must(r.capture("health", map[string]any{"version": "0.2.0", "devices_connected": 0, "lan_seen": 0,
		"ble": map[string]any{"state": "no_permission", "adapter": "hci0", "seen": 3, "connected": 1}}))
	st, e := r.f.service.EdgeStatus(r.ctx, r.owner, r.gateway)
	if e != nil || st.BLEState != "no_permission" || st.BLESeen != 3 || st.BLEConnected != 1 || st.BLEDevices != 200 {
		t.Fatalf("status: %+v %v", st, e)
	}
	r.must(r.capture("health", map[string]any{"version": "0.1.1", "devices_connected": 0, "lan_seen": 0}))
	if st, _ = r.f.service.EdgeStatus(r.ctx, r.owner, r.gateway); st.BLEState != "" {
		t.Fatalf("an agent without Bluetooth: %q", st.BLEState)
	}
}

// 00043 goes down only when no BLE registration is live, and comes back up.
func TestTuyaBLEMigrationDown(t *testing.T) {
	r := newBLERig(t)
	id := r.registerBLE()
	if e := goose.DownTo(r.f.admin, "../migrations", 42); e == nil || !strings.Contains(e.Error(), "BLE registrations") {
		t.Fatalf("down with a BLE registration: %v", e)
	}
	if _, e := r.f.admin.ExecContext(r.ctx, `UPDATE core.devices SET removed_at=now() WHERE id=$1`, id); e != nil {
		t.Fatal(e)
	}
	if e := goose.DownTo(r.f.admin, "../migrations", 42); e != nil {
		t.Fatal(e)
	}
	if n := r.count(`SELECT count(*) FROM information_schema.columns WHERE table_schema='core' AND table_name='tuya_devices' AND column_name='transport'`); n != 0 {
		t.Fatal("down kept the columns")
	}
	if e := goose.Up(r.f.admin, "../migrations"); e != nil {
		t.Fatal(e)
	}
	if n := r.count(`SELECT count(*) FROM pg_tables WHERE schemaname='core' AND tablename='edge_ble_seen' AND rowsecurity`); n != 1 {
		t.Fatal("up again: edge_ble_seen missing or without row level security")
	}
}

// A lock over BLE is read-only: commands are refused, and the agent gets no data-point types and the readonly flag.
func TestEdgeBLELockIsReadOnly(t *testing.T) {
	r := newBLERig(t)
	r.registerBLE()
	if _, e := r.f.admin.ExecContext(r.ctx, `UPDATE core.tuya_devices SET tuya_category='jtmspro' WHERE gateway_id=$1 AND tuya_id=$2`, r.gateway, r.ids["thermo"]); e != nil {
		t.Fatal(e)
	}
	r.report("thermo", map[int]any{1: true, 2: 200, 4: 1})
	code, out := r.send(r.ownerAuth.AccessToken, map[string]any{"device_id": r.devices["thermo"], "property": "temp_set", "value": 21.5})
	if code != 400 || out["error"] != "not_settable" {
		t.Fatalf("lock command: %d %v", code, out)
	}
	_, devices, _ := r.pull("ble", "")
	if len(devices) != 1 || devices[0]["readonly"] != true || devices[0]["dp_types"] != nil {
		t.Fatalf("lock config: %v", devices)
	}
	// Still read: its state comes in as usual.
	if s := r.text(`SELECT state->>'mode' FROM core.tuya_devices WHERE gateway_id=$1 AND tuya_id=$2`, r.gateway, r.ids["thermo"]); s != "manual" {
		t.Fatalf("lock state: %q", s)
	}
}

// Each command times out on its own window: a Wi-Fi one after 10 s, a BLE one after 45 s.
func TestEdgeBLEConfirmWindow(t *testing.T) {
	r := newBLERig(t)
	r.registerBLE()
	r.register("switch")
	r.report("thermo", map[int]any{1: true, 2: 200, 4: 1})
	r.report("switch", map[int]any{1: true, 2: false, 3: false})
	// The rig's Publish echoes the command back as the device's report; here nothing may confirm, so dispatch
	// through a publisher that only records.
	r.dispatcher.Publisher = silentPublisher{}
	ids := map[string]string{}
	for name, body := range map[string]map[string]any{
		"ble":  {"device_id": r.devices["thermo"], "property": "temp_set", "value": 22.0},
		"wifi": {"device_id": r.devices["switch"], "property": "switch_2", "value": "ON"},
	} {
		code, out := r.send(r.ownerAuth.AccessToken, body)
		if code != 202 {
			t.Fatalf("%s: %d %v", name, code, out)
		}
		ids[name] = out["id"].(string)
	}
	if n := r.dispatch(); n != 2 {
		t.Fatalf("published %d", n)
	}
	sent := time.Now()
	status := func(id string) string {
		return r.text(`SELECT status FROM core.device_commands WHERE id=$1`, id)
	}
	if _, e := r.f.repo.TimeoutCommands(r.ctx, r.owner.TenantID, sent.Add(20*time.Second)); e != nil {
		t.Fatal(e)
	}
	if status(ids["wifi"]) != "timeout" || status(ids["ble"]) != "sent" {
		t.Fatalf("after 20 s: wifi %s, ble %s", status(ids["wifi"]), status(ids["ble"]))
	}
	if _, e := r.f.repo.TimeoutCommands(r.ctx, r.owner.TenantID, sent.Add(46*time.Second)); e != nil {
		t.Fatal(e)
	}
	if status(ids["ble"]) != "timeout" {
		t.Fatalf("after 46 s: ble %s", status(ids["ble"]))
	}
}

type silentPublisher struct{}

func (silentPublisher) Publish(string, []byte) error { return nil }
