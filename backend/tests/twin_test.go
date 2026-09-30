package tests

import (
	"aether/backend/internal/adapters/httpapi"
	"aether/backend/internal/adapters/postgres"
	"aether/backend/internal/demotwin"
	"aether/backend/internal/domain"
	"aether/backend/internal/simulation"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// demoTwin builds the demo workspace through demotwin.Setup, marks it demo with the admin connection (the
// runtime role cannot), and feeds it a few steps of the script straight into the capture path.
func demoTwin(t *testing.T, f *fixture) demotwin.State {
	t.Helper()
	ctx := context.Background()
	registration := f.service.Registration
	f.service.Registration = true
	defer func() { f.service.Registration = registration }()
	st, e := demotwin.Setup(ctx, f.service, memberEmail(), chosenPassword, "Demo twin test", func(tenant string) error {
		_, e := f.admin.ExecContext(ctx, `UPDATE core.tenants SET demo=true WHERE id=$1`, tenant)
		return e
	})
	if e != nil {
		t.Fatal(e)
	}
	for i, step := range []int{simulation.TwinSOSStep - 2, simulation.TwinSOSStep - 1, simulation.TwinSOSStep} {
		for id, raw := range demotwin.Uplinks(st, step, time.Now(), simulation.TwinOverrides{Warmup: i == 0}) {
			if _, e := f.service.Capture(ctx, st.TenantID, id, raw); e != nil {
				t.Fatalf("capture step %d: %v", step, e)
			}
		}
	}
	return st
}

func TestTwinStateOfTheDemoSite(t *testing.T) {
	f := setup(t)
	api := httpapi.New(f.cfg, f.service, f.repo)
	st := demoTwin(t, f)
	b := simulation.TwinDemo()
	owner, _ := signIn(t, f, st.OwnerEmail, st.Password, st.TenantID)
	path := "/api/v1/twin/sites/" + st.SiteID + "/state"

	code, out, _ := req(t, api, "GET", path+"?people=named", "Bearer "+owner.AccessToken, "", "", nil)
	if code != 200 || out["demo"] != true {
		t.Fatalf("state: %d %v", code, out)
	}
	devices := out["devices"].([]any)
	if len(devices) != len(b.Gateways)+len(b.Sensors) {
		t.Fatalf("placed devices: %d, want %d", len(devices), len(b.Gateways)+len(b.Sensors))
	}
	cold, withTemp, online := 0.0, 0, 0
	for _, raw := range devices {
		d := raw.(map[string]any)
		if d["online"] == true {
			online++
		}
		if tv, ok := d["t"].(float64); ok {
			withTemp++
			if strings.Contains(d["name"].(string), "ห้องเย็น") {
				cold = tv
			}
		}
	}
	if withTemp != 52 || online != len(devices) || cold < 3 || cold > 7 {
		t.Fatalf("readings: %d with temperature, %d online, cold store %.1f", withTemp, online, cold)
	}
	presence := out["presence"].(map[string]any)
	people := presence["people"].([]any)
	if presence["mode"] != "named" || len(people) != len(b.People) {
		t.Fatalf("people: mode %v, %d people", presence["mode"], len(people))
	}
	sosMAC := ""
	for _, p := range b.People {
		if p.SOS {
			sosMAC = p.MAC
		}
	}
	ward3 := st.Gateways[b.Ward3Gateway].ID
	foundSOS := false
	for _, raw := range out["alerts"].([]any) {
		a := raw.(map[string]any)
		if a["sos"] == true {
			foundSOS = true
			if a["gateway_id"] != ward3 || a["pid"] != sosMAC {
				t.Fatalf("SOS is not located in ward 3: %v", a)
			}
		}
	}
	if !foundSOS {
		t.Fatalf("no SOS alert in the state: %v", out["alerts"])
	}

	// counts: no identity at all, not even in alert titles.
	code, out, raw := req(t, api, "GET", path, "Bearer "+owner.AccessToken, "", "", nil)
	if code != 200 {
		t.Fatalf("counts: %d", code)
	}
	var text strings.Builder
	_ = json.NewEncoder(&text).Encode(out)
	_ = raw
	body := text.String()
	for _, p := range b.People {
		if strings.Contains(body, p.MAC) || strings.Contains(body, p.Name) {
			t.Fatalf("counts mode names a wearer (%s)", p.Name)
		}
	}
	presence = out["presence"].(map[string]any)
	total := 0.0
	for _, c := range presence["counts"].([]any) {
		total += c.(map[string]any)["n"].(float64)
	}
	if presence["mode"] != "counts" || presence["people"] != nil || int(total) != len(b.People) {
		t.Fatalf("counts: %v", presence)
	}
	// tracks: pseudonyms, never the MAC.
	code, out, _ = req(t, api, "GET", path+"?people=tracks", "Bearer "+owner.AccessToken, "", "", nil)
	if code != 200 {
		t.Fatalf("tracks: %d", code)
	}
	first := map[string]bool{}
	prev := ""
	for _, raw := range out["presence"].(map[string]any)["people"].([]any) {
		p := raw.(map[string]any)
		pid := p["pid"].(string)
		if !strings.HasPrefix(pid, "p") || strings.HasPrefix(pid, "f1b") || p["name"] != nil {
			t.Fatalf("tracks mode leaks identity: %v", p)
		}
		if pid < prev {
			t.Fatalf("tracks people are not sorted by pseudonym: %s after %s", pid, prev)
		}
		prev = pid
		first[pid] = true
		for _, k := range []string{"since", "last_at"} {
			if v, ok := p[k].(string); ok {
				at, e := time.Parse(time.RFC3339Nano, v)
				if e != nil || at.Second() != 0 || at.Nanosecond() != 0 {
					t.Fatalf("tracks %s is not rounded to the minute: %s", k, v)
				}
			}
		}
	}
	// A second response (served from the same cached rows) draws new pseudonyms.
	_, again, _ := req(t, api, "GET", path+"?people=tracks", "Bearer "+owner.AccessToken, "", "", nil)
	for _, raw := range again["presence"].(map[string]any)["people"].([]any) {
		if first[raw.(map[string]any)["pid"].(string)] {
			t.Fatal("a pseudonym repeated across two responses")
		}
	}
	for _, raw := range again["devices"].([]any) {
		if d := raw.(map[string]any); d["ext"] != nil && strings.HasPrefix(d["ext"].(string), "f1b") {
			t.Fatalf("tracks names a worn tag: %v", d)
		}
	}
	if code, _, _ := req(t, api, "GET", path+"?people=everyone", "Bearer "+owner.AccessToken, "", "", nil); code != 400 {
		t.Fatalf("unknown people mode: %d", code)
	}

	// The read is in the access log, and a read that returned names is its own resource.
	var n, named int
	if e := f.admin.QueryRow(`SELECT count(*) FILTER (WHERE resource='twin_live'), count(*) FILTER (WHERE resource='twin_live_named')
      FROM core.access_log WHERE tenant_id=$1 AND subject_id=$2`, st.TenantID, st.SiteID).Scan(&n, &named); e != nil || n == 0 || named == 0 {
		t.Fatalf("access log: %d plain, %d named, %v", n, named, e)
	}
	// A demo workspace never sends anything.
	jobs, e := f.repo.ClaimNotifications(context.Background(), st.TenantID, 10, time.Now().Add(time.Hour))
	if e != nil || len(jobs) != 0 {
		t.Fatalf("demo notifications claimed: %d %v", len(jobs), e)
	}
}

func TestTwinStateIsScoped(t *testing.T) {
	f := setup(t)
	api := httpapi.New(f.cfg, f.service, f.repo)
	st := demoTwin(t, f)
	owner, ownerP := signIn(t, f, st.OwnerEmail, st.Password, st.TenantID)
	path := "/api/v1/twin/sites/" + st.SiteID + "/state"

	// Another workspace: the site does not exist for it.
	_, other, otherP := f.account(t)
	if code, _, _ := req(t, api, "GET", path, "Bearer "+other.AccessToken, "", "", nil); code != 404 {
		t.Fatalf("another workspace read the twin: %d", code)
	}
	// A member of another project of the same workspace: not found either.
	elsewhere, e := f.service.CreateProject(context.Background(), ownerP, "โครงการอื่น", "", "blue")
	if e != nil {
		t.Fatal(e)
	}
	scopedEmail := memberEmail()
	addMember(t, api, owner.AccessToken, scopedEmail, "operator", []string{elsewhere.ID})
	scoped, _ := changeInitialPassword(t, f, api, scopedEmail, st.TenantID)
	if code, _, _ := req(t, api, "GET", path, "Bearer "+scoped.AccessToken, "", "", nil); code != 404 {
		t.Fatalf("a member of another project read the twin: %d", code)
	}
	// A member whose floor plan access is none: forbidden (the twin is the live side of the plans).
	blindEmail := memberEmail()
	blindID := addMember(t, api, owner.AccessToken, blindEmail, "operator", nil)
	if code, out, _ := req(t, api, "POST", "/api/v1/members/"+blindID+"/access", "Bearer "+owner.AccessToken, "", "", map[string]string{"floorplan": "none"}); code != 204 {
		t.Fatalf("set access: %d %v", code, out)
	}
	blind, _ := changeInitialPassword(t, f, api, blindEmail, st.TenantID)
	if code, _, _ := req(t, api, "GET", path, "Bearer "+blind.AccessToken, "", "", nil); code != 403 {
		t.Fatalf("floorplan none still reads the twin: %d", code)
	}
	// A viewer asking for names gets counts: named needs owner, admin or operator.
	viewerEmail := memberEmail()
	addMember(t, api, owner.AccessToken, viewerEmail, "viewer", nil)
	viewer, _ := changeInitialPassword(t, f, api, viewerEmail, st.TenantID)
	code, out, _ := req(t, api, "GET", path+"?people=named", "Bearer "+viewer.AccessToken, "", "", nil)
	if code != 200 || out["presence"].(map[string]any)["mode"] != "counts" {
		t.Fatalf("viewer names: %d %v", code, out["presence"])
	}
	// Outside a demo workspace nobody gets names yet (until twin_settings, P2).
	code, site, _ := req(t, api, "POST", "/api/v1/sites", "Bearer "+other.AccessToken, "", "", map[string]any{"name": "อาคารจริง", "description": ""})
	if code != 201 {
		t.Fatalf("site: %d", code)
	}
	code, out, _ = req(t, api, "GET", "/api/v1/twin/sites/"+site["id"].(string)+"/state?people=named", "Bearer "+other.AccessToken, "", "", nil)
	if code != 200 || out["demo"] != false || out["presence"].(map[string]any)["mode"] != "counts" {
		t.Fatalf("named outside a demo workspace: %d %v", code, out)
	}
	_ = otherP
	// The runtime role cannot make a workspace demo.
	if _, e := f.runtime.Exec(`UPDATE core.tenants SET demo=true`); e == nil {
		t.Fatal("aether_app updated core.tenants")
	}
	if _, e := f.runtime.Exec(`INSERT INTO core.tenants(id,name,demo) VALUES(gen_random_uuid(),'x',true)`); e == nil {
		t.Fatal("aether_app inserted a demo workspace")
	}
}

// Outside named mode nothing names a person: a worn tag placed on the plan, an SOS of a (non-roaming) wristband and an
// alert about a roaming tag whose zone is on another site all come back without names, MACs or naming titles.
func TestTwinStateNamesNobodyOutsideNamedMode(t *testing.T) {
	f := setup(t)
	api := httpapi.New(f.cfg, f.service, f.repo)
	ctx := context.Background()
	_, auth, p := f.account(t)
	g, _, e := f.service.CreateGateway(ctx, p, "GW ห้อง A", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	g2, _, e := f.service.CreateGateway(ctx, p, "GW อาคารอื่น", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	const wearerB10, wearerC10, sensor = "นางสาวทดสอบ สวมปุ่ม", "นายทดสอบ เดินข้ามตึก", "เซนเซอร์ห้องยา"
	b10, e := f.service.CreateDevice(ctx, p, g.ID, wearerB10, "f2b000000001", "minew-b10-pending@1")
	if e != nil {
		t.Fatal(e)
	}
	c10, e := f.service.CreateDevice(ctx, p, g2.ID, wearerC10, "f2c000000001", "minew-c10-pending@1")
	if e != nil {
		t.Fatal(e)
	}
	if e := f.repo.SetDeviceRoaming(ctx, p, c10.ID, true); e != nil {
		t.Fatal(e)
	}
	s1, e := f.service.CreateDevice(ctx, p, g.ID, sensor, "f2e000000001", "minew-s1-pending@1")
	if e != nil {
		t.Fatal(e)
	}
	site, e := f.repo.CreateSite(ctx, p, domain.Site{Name: "อาคาร A"})
	if e != nil {
		t.Fatal(e)
	}
	other, e := f.repo.CreateSite(ctx, p, domain.Site{Name: "อาคาร B"})
	if e != nil {
		t.Fatal(e)
	}
	save := func(fl domain.Floor, pl []domain.FloorPlacement) {
		fl.Placements, fl.Revision = pl, 1
		if _, _, e := f.repo.SaveFloor(ctx, p, fl); e != nil {
			t.Fatal(e)
		}
	}
	save(site.Floors[0], []domain.FloorPlacement{{AssetKind: "gateway", AssetID: g.ID, X: 5, Y: 5, Z: 2.5}, {AssetKind: "device", AssetID: b10.ID, X: 6, Y: 6, Z: 1}, {AssetKind: "device", AssetID: s1.ID, X: 7, Y: 7, Z: 1.5}})
	save(other.Floors[0], []domain.FloorPlacement{{AssetKind: "gateway", AssetID: g2.ID, X: 5, Y: 5, Z: 2.5}})
	// The roaming tag's zone is on the other site; the gateway of this site raised an alert about it.
	if _, e := f.admin.Exec(`INSERT INTO core.presence_state(tenant_id,external_id,gateway_id,since) VALUES($1,'f2c000000001',$2,now())`, p.TenantID, g2.ID); e != nil {
		t.Fatal(e)
	}
	alert := func(gateway, external, device, event, title string) {
		ev := uuid.NewString()
		if _, e := f.admin.Exec(`INSERT INTO core.device_events(tenant_id,id,gateway_id,external_id,device_name,event_type,occurred_at) VALUES($1,$2,$3,$4,$5,$6,now())`, p.TenantID, ev, gateway, external, device, event); e != nil {
			t.Fatal(e)
		}
		if _, e := f.admin.Exec(`INSERT INTO core.alerts(tenant_id,id,event_id,gateway_id,external_id,device_name,event_type,severity,title) VALUES($1,$2,$3,$4,$5,$6,$7,'critical',$8)`,
			p.TenantID, uuid.NewString(), ev, gateway, external, device, event, title); e != nil {
			t.Fatal(e)
		}
	}
	alert(g.ID, "f2b000000001", wearerB10, "button", "SOS · "+wearerB10)
	alert(g.ID, "f2c000000001", wearerC10, "zone", wearerC10+" เข้าโซน")
	alert(g.ID, "f2e000000001", sensor, "threshold", sensor+" ร้อนเกิน")

	for _, mode := range []string{"counts", "tracks", "named"} { // named is clamped: not a demo workspace
		code, out, _ := req(t, api, "GET", "/api/v1/twin/sites/"+site.ID+"/state?people="+mode, "Bearer "+auth.AccessToken, "", "", nil)
		if code != 200 {
			t.Fatalf("%s: %d", mode, code)
		}
		var b strings.Builder
		_ = json.NewEncoder(&b).Encode(out)
		body := b.String()
		for _, secret := range []string{wearerB10, wearerC10, "f2b000000001", "f2c000000001"} {
			if strings.Contains(body, secret) {
				t.Fatalf("%s mode shows %q: %s", mode, secret, body)
			}
		}
		if !strings.Contains(body, sensor+" ร้อนเกิน") || !strings.Contains(body, "f2e000000001") {
			t.Fatalf("%s mode hid an ordinary sensor: %s", mode, body)
		}
		found := false
		for _, raw := range out["devices"].([]any) {
			d := raw.(map[string]any)
			if d["id"] == b10.ID {
				found = true
				if d["name"] != postgres.TwinPersonalName || d["ext"] != nil {
					t.Fatalf("%s: the placed wristband: %v", mode, d)
				}
			}
		}
		if !found {
			t.Fatalf("%s: the placed wristband is missing", mode)
		}
	}
}
