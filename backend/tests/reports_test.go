package tests

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"aether/backend/internal/domain"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

// Reports and data export (docs/platform/reports.md): CSV of fixed sensors, summary reports, saved definitions.
// Data is seeded straight into core.sensor_samples and core.sample_rollup so every number below is known.

type reportRig struct {
	t                *testing.T
	f                *fixture
	api              *fiber.App
	owner            domain.Principal
	ownerToken       string
	projectA, projB  string
	gwA, gwB         string
	from, to         time.Time
	fixedA, fixedB   string // external ids
	wearable, roamer string
	stranger         string // a stream nobody registered
}

func reportSensor(gateway, external string) map[string]string {
	return map[string]string{"gateway_id": gateway, "external_id": external}
}

// postRaw posts JSON and returns the raw body (the CSV export is not JSON).
func postRaw(t *testing.T, api *fiber.App, path, token string, payload any) (int, string, http.Header) {
	t.Helper()
	b, _ := json.Marshal(payload)
	r := httptest.NewRequest("POST", path, bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	res, e := api.Test(r, fiber.TestConfig{Timeout: 30 * time.Second})
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(raw), res.Header
}

func newReportRig(t *testing.T) *reportRig {
	t.Helper()
	f := setup(t)
	ctx := context.Background()
	_, auth, owner := f.account(t)
	rig := &reportRig{t: t, f: f, api: busyAPI(f), owner: owner, ownerToken: auth.AccessToken}
	a, e := f.service.CreateProject(ctx, owner, "Cold room A", "", "mint")
	if e != nil {
		t.Fatal(e)
	}
	b, e := f.service.CreateProject(ctx, owner, "Cold room B", "", "blue")
	if e != nil {
		t.Fatal(e)
	}
	rig.projectA, rig.projB = a.ID, b.ID
	ga, _, e := f.service.CreateGatewayIn(ctx, owner, "Gateway A", "minew-mg3", &a.ID)
	if e != nil {
		t.Fatal(e)
	}
	gb, _, e := f.service.CreateGatewayIn(ctx, owner, "Gateway B", "minew-mg3", &b.ID)
	if e != nil {
		t.Fatal(e)
	}
	rig.gwA, rig.gwB = ga.ID, gb.ID
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	rig.fixedA, rig.fixedB, rig.wearable, rig.roamer, rig.stranger = "a1"+suffix, "b1"+suffix, "c1"+suffix, "d1"+suffix, "e1"+suffix
	for _, d := range []struct{ gw, name, ext, profile string }{
		{ga.ID, "Fridge A", rig.fixedA, "generic-environment@1"},
		{gb.ID, "Fridge B", rig.fixedB, "generic-environment@1"},
		{ga.ID, "Nurse card", rig.wearable, "minew-c10-pending@1"},
		{ga.ID, "Trolley", rig.roamer, "generic-environment@1"},
	} {
		dev, e := f.service.CreateDevice(ctx, owner, d.gw, d.name, d.ext, d.profile)
		if e != nil {
			t.Fatal(e)
		}
		if d.ext == rig.roamer {
			if e := f.repo.SetDeviceRoaming(ctx, owner, dev.ID, true); e != nil {
				t.Fatal(e)
			}
		}
	}
	// The name starts with "=": the CSV must neutralise it (spreadsheets would run it as a formula).
	rig.exec(`UPDATE core.devices SET name='=HYPERLINK("x")' WHERE tenant_id=$1 AND external_id=$2`, owner.TenantID, rig.fixedB)
	loc, _ := time.LoadLocation("Asia/Bangkok")
	now := time.Now().In(loc)
	rig.from = time.Date(now.Year(), now.Month(), now.Day()-2, 0, 0, 0, 0, loc)
	rig.to = rig.from.Add(24 * time.Hour)
	for _, s := range []struct{ gw, ext string }{{ga.ID, rig.fixedA}, {gb.ID, rig.fixedB}, {ga.ID, rig.wearable}, {ga.ID, rig.stranger}} {
		rig.exec(`INSERT INTO core.sensor_streams(tenant_id,gateway_id,external_id,name,last_seen) VALUES($1,$2,$3,'stream',now())`, owner.TenantID, s.gw, s.ext)
	}
	sample := func(gw, ext string, at time.Time, reading string) {
		rig.exec(`INSERT INTO core.sensor_samples(tenant_id,gateway_id,external_id,event_key,received_at,decoder_id,reading) VALUES($1,$2,$3,$4,$5,'test',$6::jsonb)`,
			owner.TenantID, gw, ext, uuid.NewString(), at, reading)
	}
	sample(ga.ID, rig.fixedA, rig.from.Add(time.Hour), `{"temperature":4.5,"humidity":51,"battery":90}`)
	sample(ga.ID, rig.fixedA, rig.from.Add(time.Hour+10*time.Minute), `{"temperature":-1.25,"humidity":52}`)
	sample(ga.ID, rig.fixedA, rig.from.Add(time.Hour+20*time.Minute), `{"kind":"motion","frames":["minew-ffe1-a1@1"],"metrics":{"motion":1}}`) // no temperature, humidity or battery
	sample(ga.ID, rig.fixedA, rig.from.Add(time.Hour+30*time.Minute), `{"frames":["z2m-state@1"],"metrics":{"temperature":6,"humidity":44,"battery":80},"temperature":99}`)
	sample(ga.ID, rig.fixedA, rig.from.Add(time.Hour+40*time.Minute), `{"kind":"environment","frames":["minew-ffe1-a101@1"],"temperature":7,"humidity":45,"battery":0}`)
	sample(ga.ID, rig.fixedA, rig.from.Add(-time.Minute), `{"temperature":99}`) // before the range
	sample(gb.ID, rig.fixedB, rig.from.Add(2*time.Hour), `{"temperature":3,"humidity":40}`)
	// Twelve 5-minute buckets from 02:00: two high, one low (2..8 °C), one humid (max 70 %).
	temps := []float64{5, 5, 9, 10, 5, 5, 1, 5, 5, 5, 5, 5}
	for i, v := range temps {
		h := 50.0
		if i == 8 {
			h = 75
		}
		rig.exec(`INSERT INTO core.sample_rollup(tenant_id,gateway_id,external_id,bucket,n,t_avg,t_min,t_max,h_avg,h_min,h_max) VALUES($1,$2,$3,$4,1,$5,$5,$5,$6,$6,$6)`,
			owner.TenantID, ga.ID, rig.fixedA, rig.from.Add(2*time.Hour+time.Duration(i)*5*time.Minute), v, h)
	}
	return rig
}

func (r *reportRig) exec(q string, args ...any) {
	r.t.Helper()
	if _, e := r.f.admin.Exec(q, args...); e != nil {
		r.t.Fatal(e)
	}
}

func (r *reportRig) window(extra map[string]any) map[string]any {
	out := map[string]any{"from": r.from.Format(time.RFC3339), "to": r.to.Format(time.RFC3339)}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func (r *reportRig) export(token string, sensors []map[string]string, resolution string) (int, string, http.Header) {
	r.t.Helper()
	return postRaw(r.t, r.api, "/api/v1/reports/export", token, r.window(map[string]any{"sensors": sensors, "resolution": resolution}))
}

func readCSV(t *testing.T, body string) [][]string {
	t.Helper()
	if !strings.HasPrefix(body, "\ufeff") {
		t.Fatalf("no BOM: %q", body[:min(len(body), 20)])
	}
	reader := csv.NewReader(strings.NewReader(strings.TrimPrefix(body, "\ufeff")))
	reader.FieldsPerRecord = -1 // the closing marker row has two fields
	rows, e := reader.ReadAll()
	if e != nil {
		t.Fatalf("csv: %v\n%s", e, body)
	}
	// A whole file ends with #END and its number of data rows; the rows before it are returned.
	if len(rows) < 2 || strings.Join(rows[len(rows)-1], ",") != "#END,"+strconv.Itoa(len(rows)-2) {
		t.Fatalf("no #END marker: %v", rows[max(0, len(rows)-2):])
	}
	return rows[:len(rows)-1]
}

// rollupRaw rolls the seeded raw samples up through the real rollup (as the worker would): a raw export lists only
// samples of buckets the rollup holds. The hand-written buckets of Fridge A at 02:00.. stay (they are a fixed sensor's).
func (r *reportRig) rollupRaw() {
	r.t.Helper()
	for _, span := range [][2]time.Duration{{time.Hour, time.Hour + 45*time.Minute}, {2 * time.Hour, 2*time.Hour + time.Minute}} {
		if _, e := r.f.repo.RollupRange(context.Background(), r.owner.TenantID, r.from.Add(span[0]), r.from.Add(span[1])); e != nil {
			r.t.Fatal(e)
		}
	}
}

func TestReportExportCSV(t *testing.T) {
	r := newReportRig(t)
	r.rollupRaw()
	day := r.from.Format("2006-01-02")
	code, body, h := r.export(r.ownerToken, []map[string]string{reportSensor(r.gwA, strings.ToUpper(r.fixedA)), reportSensor(r.gwB, r.fixedB)}, "raw")
	if code != 200 || !strings.HasPrefix(h.Get("Content-Type"), "text/csv") || !strings.Contains(h.Get("Content-Disposition"), "attachment") {
		t.Fatalf("raw export: %d %v %s", code, h, body)
	}
	rows := readCSV(t, body)
	want := [][]string{
		{"เวลา", "เซนเซอร์", "gateway", "external_id", "temperature", "humidity", "battery"},
		{day + " 01:00:00", "Fridge A", "Gateway A", r.fixedA, "4.5", "51", "90"},
		{day + " 01:10:00", "Fridge A", "Gateway A", r.fixedA, "-1.25", "52", ""},
		{day + " 01:30:00", "Fridge A", "Gateway A", r.fixedA, "6", "44", "80"}, // Zigbee2MQTT: metrics, not the top level
		{day + " 01:40:00", "Fridge A", "Gateway A", r.fixedA, "7", "45", ""},   // a battery of 0 is no reading
		{day + " 02:00:00", `'=HYPERLINK("x")`, "Gateway B", r.fixedB, "3", "40", ""},
	}
	if len(rows) != len(want) {
		t.Fatalf("raw rows: %v", rows)
	}
	for i := range want {
		if strings.Join(rows[i], "|") != strings.Join(want[i], "|") {
			t.Fatalf("row %d: %v, want %v", i, rows[i], want[i])
		}
	}
	// Fridge A has five rolled-up buckets at 01:00..01:40 (one with only a motion frame) and twelve written at 02:00.
	for _, res := range []struct {
		resolution string
		rows       int
		want       string
	}{
		// Means of the twelve 5-minute averages: 65/12 °C and (11*50+75)/12 %.
		{"1h", 3, day + " 02:00:00|Fridge A|Gateway A|" + r.fixedA + "|12|5.42|1|10|52.08|50|75"},
		// All seventeen buckets, sixteen with a temperature: 81.25/16 °C and 817/16 %.
		{"1d", 2, day + " 00:00:00|Fridge A|Gateway A|" + r.fixedA + "|17|5.08|-1.25|10|51.06|44|75"},
	} {
		code, body, _ := r.export(r.ownerToken, []map[string]string{reportSensor(r.gwA, r.fixedA)}, res.resolution)
		rows := readCSV(t, body)
		if code != 200 || len(rows) != res.rows {
			t.Fatalf("%s export: %d %v", res.resolution, code, rows)
		}
		if strings.Join(rows[0], "|") != "เวลา|เซนเซอร์|gateway|external_id|n|t_avg|t_min|t_max|h_avg|h_min|h_max" {
			t.Fatalf("%s header: %v", res.resolution, rows[0])
		}
		if got := strings.Join(rows[len(rows)-1], "|"); got != res.want {
			t.Fatalf("%s row: %s", res.resolution, got)
		}
	}
	code, body, _ = r.export(r.ownerToken, []map[string]string{reportSensor(r.gwA, r.fixedA)}, "5m")
	if rows := readCSV(t, body); code != 200 || len(rows) != 18 || rows[1][0] != day+" 01:00:00" || rows[8][0] != day+" 02:10:00" || rows[8][7] != "9" {
		t.Fatalf("5m export: %d %v", code, rows)
	}
}

func TestReportLimitsAndPersonalSensors(t *testing.T) {
	r := newReportRig(t)
	ok := []map[string]string{reportSensor(r.gwA, r.fixedA)}
	many := []map[string]string{}
	for i := 0; i <= domain.ReportMaxSensors; i++ {
		many = append(many, reportSensor(r.gwA, r.fixedA+strings.Repeat("x", i+1)))
	}
	for _, c := range []struct {
		name    string
		payload map[string]any
		code    int
		reason  string
	}{
		{"window", map[string]any{"from": r.from.Format(time.RFC3339), "to": r.from.Add(32 * 24 * time.Hour).Format(time.RFC3339), "sensors": ok, "resolution": "raw"}, 400, "window_too_long"},
		{"order", map[string]any{"from": r.to.Format(time.RFC3339), "to": r.from.Format(time.RFC3339), "sensors": ok, "resolution": "raw"}, 400, "invalid_range"},
		{"format", map[string]any{"from": "yesterday", "to": r.to.Format(time.RFC3339), "sensors": ok, "resolution": "raw"}, 400, "invalid_range"},
		{"resolution", r.window(map[string]any{"sensors": ok, "resolution": "1w"}), 400, "invalid_resolution"},
		{"none", r.window(map[string]any{"sensors": []map[string]string{}, "resolution": "raw"}), 400, "no_sensors"},
		{"many", r.window(map[string]any{"sensors": many, "resolution": "raw"}), 400, "too_many_sensors"},
		{"duplicate", r.window(map[string]any{"sensors": []map[string]string{reportSensor(r.gwA, r.fixedA), reportSensor(r.gwA, strings.ToUpper(r.fixedA))}, "resolution": "raw"}), 400, "duplicate_sensor"},
		{"bad id", r.window(map[string]any{"sensors": []map[string]string{reportSensor("not-a-uuid", r.fixedA)}, "resolution": "raw"}), 400, "invalid_sensor"},
		{"wearable", r.window(map[string]any{"sensors": []map[string]string{reportSensor(r.gwA, r.fixedA), reportSensor(r.gwA, r.wearable)}, "resolution": "raw"}), 400, "personal_sensor"},
		{"roaming", r.window(map[string]any{"sensors": []map[string]string{reportSensor(r.gwA, r.roamer)}, "resolution": "5m"}), 400, "personal_sensor"},
		{"unregistered", r.window(map[string]any{"sensors": []map[string]string{reportSensor(r.gwA, r.stranger)}, "resolution": "raw"}), 400, "sensor_not_registered"},
		{"unknown", r.window(map[string]any{"sensors": []map[string]string{reportSensor(r.gwA, "f0f0f0f0f0f0")}, "resolution": "raw"}), 404, "sensor_not_found"},
		{"wrong gateway", r.window(map[string]any{"sensors": []map[string]string{reportSensor(r.gwB, r.fixedA)}, "resolution": "raw"}), 404, "sensor_not_found"},
	} {
		code, out, _ := req(t, r.api, "POST", "/api/v1/reports/export", "Bearer "+r.ownerToken, "", "", c.payload)
		if code != c.code || out["error"] != c.reason {
			t.Fatalf("%s: %d %v, want %d %s", c.name, code, out, c.code, c.reason)
		}
		// The summary applies the same limits and the same refusal of personal sensors.
		delete(c.payload, "resolution")
		if c.name == "resolution" {
			continue
		}
		code, out, _ = req(t, r.api, "POST", "/api/v1/reports/summary", "Bearer "+r.ownerToken, "", "", c.payload)
		if code != c.code || out["error"] != c.reason {
			t.Fatalf("summary %s: %d %v, want %d %s", c.name, code, out, c.code, c.reason)
		}
	}
	code, out, _ := req(t, r.api, "POST", "/api/v1/reports/summary", "Bearer "+r.ownerToken, "", "", r.window(map[string]any{"sensors": ok, "temp": map[string]any{"min": 8, "max": 2}}))
	if code != 400 || out["error"] != "invalid_limits" {
		t.Fatalf("limits: %d %v", code, out)
	}
	// The picker lists fixed sensors only: no wearable, no roaming tag, no unregistered stream.
	code, out = get(t, r.api, "/api/v1/reports/sensors", r.ownerToken)
	if listed := values(out, "external_id"); code != 200 || !listed[r.fixedA] || !listed[r.fixedB] || listed[r.wearable] || listed[r.roamer] || listed[r.stranger] {
		t.Fatalf("sensors: %d %v", code, out)
	}
}

func TestReportSummaryExcursions(t *testing.T) {
	r := newReportRig(t)
	code, out, _ := req(t, r.api, "POST", "/api/v1/reports/summary", "Bearer "+r.ownerToken, "", "", r.window(map[string]any{
		"sensors": []map[string]string{reportSensor(r.gwA, r.fixedA), reportSensor(r.gwB, r.fixedB)},
		"temp":    map[string]any{"min": 2, "max": 8}, "hum": map[string]any{"max": 70},
	}))
	if code != 200 {
		t.Fatal(code, out)
	}
	items := listOf(out, "sensors")
	if out["workspace"] != "Test workspace" || out["time_zone"] != "Asia/Bangkok" || len(items) != 2 || items[0]["name"] != "Fridge A" || items[0]["gateway_name"] != "Gateway A" {
		t.Fatalf("sensors: %v", items)
	}
	a := items[0]
	// 12 of 288 buckets; overall from the buckets.
	if a["buckets"] != 12.0 || a["expected_buckets"] != 288.0 || a["coverage_pct"] != 4.2 {
		t.Fatalf("coverage: %v %v %v", a["buckets"], a["expected_buckets"], a["coverage_pct"])
	}
	temp := a["temperature"].(map[string]any)
	if temp["min"] != 1.0 || temp["max"] != 10.0 || temp["avg"] != 5.42 {
		t.Fatalf("temperature: %v", temp)
	}
	start := r.from.Add(2 * time.Hour)
	want := []struct {
		metric, direction string
		slot, minutes     int
		peak              float64
	}{{"temperature", "high", 2, 10, 10}, {"temperature", "low", 6, 5, 1}, {"humidity", "high", 8, 5, 75}}
	excursions := listOf(a, "excursions")
	if len(excursions) != len(want) {
		t.Fatalf("excursions: %v", excursions)
	}
	for i, w := range want {
		e := excursions[i]
		at := start.Add(time.Duration(w.slot) * 5 * time.Minute)
		got, _ := time.Parse(time.RFC3339, e["start"].(string))
		end, _ := time.Parse(time.RFC3339, e["end"].(string))
		if e["metric"] != w.metric || e["direction"] != w.direction || !got.Equal(at) || !end.Equal(at.Add(time.Duration(w.minutes)*time.Minute)) || e["duration_min"] != float64(w.minutes) || e["peak"] != w.peak {
			t.Fatalf("excursion %d: %v, want %+v", i, e, w)
		}
	}
	minutes := a["minutes_out_of_range"].(map[string]any)
	if minutes["temperature"] != 15.0 || minutes["humidity"] != 5.0 {
		t.Fatalf("minutes out of range: %v", minutes)
	}
	days := listOf(a, "days")
	if len(days) != 1 || days[0]["date"] != r.from.Format("2006-01-02") || days[0]["expected"] != 288.0 {
		t.Fatalf("days: %v", days)
	}
	if series := listOf(a, "series"); len(series) != 1 || series[0]["t_max"] != 10.0 || series[0]["h_max"] != 75.0 {
		t.Fatalf("series: %v", series)
	}
	// Fridge B has raw samples but no rollup yet: no coverage, no numbers, no excursions.
	if b := items[1]; b["buckets"] != 0.0 || b["temperature"] != nil || len(listOf(b, "excursions")) != 0 {
		t.Fatalf("empty sensor: %v", b)
	}
}

func TestReportIsolationScopeAndModuleAccess(t *testing.T) {
	r := newReportRig(t)
	fixedA := []map[string]string{reportSensor(r.gwA, r.fixedA)}
	fixedB := []map[string]string{reportSensor(r.gwB, r.fixedB)}
	// Another workspace cannot name this workspace's sensors.
	_, otherAuth, _ := r.f.account(t)
	if code, out, _ := req(t, r.api, "POST", "/api/v1/reports/export", "Bearer "+otherAuth.AccessToken, "", "", r.window(map[string]any{"sensors": fixedA, "resolution": "raw"})); code != 404 || out["error"] != "sensor_not_found" {
		t.Fatalf("cross-tenant export: %d %v", code, out)
	}
	if code, out, _ := req(t, r.api, "POST", "/api/v1/reports/summary", "Bearer "+otherAuth.AccessToken, "", "", r.window(map[string]any{"sensors": fixedA})); code != 404 {
		t.Fatalf("cross-tenant summary: %d %v", code, out)
	}
	if code, out := get(t, r.api, "/api/v1/reports/sensors", otherAuth.AccessToken); code != 200 || len(rows(out)) != 0 {
		t.Fatalf("cross-tenant sensors: %d %v", code, out)
	}
	// An operator limited to project A: A's sensors only. An operator, so that a refused write below is the module
	// gate's doing and not the role's.
	r.rollupRaw()
	email := memberEmail()
	id := addMember(t, r.api, r.ownerToken, email, "operator", []string{r.projectA})
	auth, _ := changeInitialPassword(t, r.f, r.api, email, r.owner.TenantID)
	token := auth.AccessToken
	if code, body, _ := r.export(token, fixedA, "raw"); code != 200 || len(readCSV(t, body)) != 5 {
		t.Fatalf("own project export: %d %s", code, body)
	}
	if code, out, _ := req(t, r.api, "POST", "/api/v1/reports/export", "Bearer "+token, "", "", r.window(map[string]any{"sensors": fixedB, "resolution": "raw"})); code != 404 || out["error"] != "sensor_not_found" {
		t.Fatalf("other project export: %d %v", code, out)
	}
	if code, out := get(t, r.api, "/api/v1/reports/sensors", token); code != 200 || len(rows(out)) != 1 || !values(out, "external_id")[r.fixedA] {
		t.Fatalf("scoped sensors: %d %v", code, out)
	}
	// Module access "read": export and summary are reads and still run; writing a saved report does not.
	if code, out, _ := req(t, r.api, "POST", "/api/v1/members/"+id+"/access", "Bearer "+r.ownerToken, "", "", map[string]string{"reports": "read"}); code != 204 {
		t.Fatal(code, out)
	}
	if code, _, _ := r.export(token, fixedA, "1h"); code != 200 {
		t.Fatalf("read-only export: %d", code)
	}
	if code, out, _ := req(t, r.api, "POST", "/api/v1/reports/summary", "Bearer "+token, "", "", r.window(map[string]any{"sensors": fixedA})); code != 200 {
		t.Fatalf("read-only summary: %d %v", code, out)
	}
	if code, _, _ := req(t, r.api, "POST", "/api/v1/reports/definitions", "Bearer "+token, "", "", map[string]any{"name": "x", "project_id": r.projectA, "sensors": fixedA}); code != 403 {
		t.Fatalf("read-only write: %d", code)
	}
	// With write access the same operator may save it: the 403 above was the module gate.
	if code, _, _ := req(t, r.api, "POST", "/api/v1/members/"+id+"/access", "Bearer "+r.ownerToken, "", "", map[string]string{"reports": "write"}); code != 204 {
		t.Fatal(code)
	}
	if code, out, _ := req(t, r.api, "POST", "/api/v1/reports/definitions", "Bearer "+token, "", "", map[string]any{"name": "x", "project_id": r.projectA, "sensors": fixedA}); code != 201 {
		t.Fatalf("write with module access: %d %v", code, out)
	}
	// Module access "none": nothing of reports.
	if code, _, _ := req(t, r.api, "POST", "/api/v1/members/"+id+"/access", "Bearer "+r.ownerToken, "", "", map[string]string{"reports": "none"}); code != 204 {
		t.Fatal(code)
	}
	for _, path := range []string{"/api/v1/reports/sensors", "/api/v1/reports/definitions"} {
		if code, _ := get(t, r.api, path, token); code != 403 {
			t.Fatalf("hidden module %s: %d", path, code)
		}
	}
	if code, _, _ := r.export(token, fixedA, "raw"); code != 403 {
		t.Fatalf("hidden module export: %d", code)
	}
}

func TestReportDefinitionsCRUDAndScope(t *testing.T) {
	r := newReportRig(t)
	ctx := context.Background()
	fixedA, fixedB := reportSensor(r.gwA, r.fixedA), reportSensor(r.gwB, r.fixedB)
	code, ws, _ := req(t, r.api, "POST", "/api/v1/reports/definitions", "Bearer "+r.ownerToken, "", "", map[string]any{
		"name": "ห้องเย็นทั้งหมด", "project_id": nil, "sensors": []map[string]string{fixedA, fixedB}, "temp_min": 2, "temp_max": 8, "hum_max": 70,
	})
	if code != 201 || ws["kind"] != "environment" || ws["temp_max"] != 8.0 || ws["hum_min"] != nil || len(ws["sensors"].([]any)) != 2 {
		t.Fatalf("create workspace report: %d %v", code, ws)
	}
	code, pb, _ := req(t, r.api, "POST", "/api/v1/reports/definitions", "Bearer "+r.ownerToken, "", "", map[string]any{"name": "B only", "project_id": r.projB, "sensors": []map[string]string{fixedB}})
	if code != 201 {
		t.Fatal(code, pb)
	}
	for _, bad := range []struct {
		payload map[string]any
		code    int
		reason  string
	}{
		{map[string]any{"name": "w", "sensors": []map[string]string{reportSensor(r.gwA, r.wearable)}}, 400, "personal_sensor"},
		{map[string]any{"name": "mixed", "project_id": r.projectA, "sensors": []map[string]string{fixedA, fixedB}}, 400, "sensor_outside_project"},
		{map[string]any{"name": "", "sensors": []map[string]string{fixedA}}, 400, "invalid_name"},
		{map[string]any{"name": "x", "sensors": []map[string]string{fixedA}, "hum_min": 80, "hum_max": 20}, 400, "invalid_limits"},
		{map[string]any{"name": "x", "sensors": []map[string]string{fixedA}, "temp_max": 500}, 400, "invalid_limits"},
	} {
		if code, out, _ := req(t, r.api, "POST", "/api/v1/reports/definitions", "Bearer "+r.ownerToken, "", "", bad.payload); code != bad.code || out["error"] != bad.reason {
			t.Fatalf("bad definition %v: %d %v", bad.payload, code, out)
		}
	}
	// An operator limited to project A: reads the workspace report (trimmed to A's sensors) but not B's report,
	// writes only project A reports.
	email := memberEmail()
	addMember(t, r.api, r.ownerToken, email, "operator", []string{r.projectA})
	auth, _ := changeInitialPassword(t, r.f, r.api, email, r.owner.TenantID)
	op := auth.AccessToken
	code, list := get(t, r.api, "/api/v1/reports/definitions", op)
	if code != 200 || len(rows(list)) != 1 || rows(list)[0]["id"] != ws["id"] || len(rows(list)[0]["sensors"].([]any)) != 1 {
		t.Fatalf("scoped list: %d %v", code, list)
	}
	if code, _ := get(t, r.api, "/api/v1/reports/definitions/"+pb["id"].(string), op); code != 404 {
		t.Fatalf("other project's report: %d", code)
	}
	if code, out, _ := req(t, r.api, "POST", "/api/v1/reports/definitions", "Bearer "+op, "", "", map[string]any{"name": "mine", "sensors": []map[string]string{fixedA}}); code != 403 {
		t.Fatalf("scoped workspace report: %d %v", code, out)
	}
	if code, out, _ := req(t, r.api, "POST", "/api/v1/reports/definitions/"+ws["id"].(string)+"/update", "Bearer "+op, "", "", map[string]any{"name": "x", "project_id": r.projectA, "sensors": []map[string]string{fixedA}}); code != 403 {
		t.Fatalf("scoped update of a workspace report: %d %v", code, out)
	}
	if code, out, _ := req(t, r.api, "POST", "/api/v1/reports/definitions", "Bearer "+op, "", "", map[string]any{"name": "mine", "project_id": r.projectA, "sensors": []map[string]string{fixedB}}); code != 404 {
		t.Fatalf("another project's sensor in a report: %d %v", code, out)
	}
	code, mine, _ := req(t, r.api, "POST", "/api/v1/reports/definitions", "Bearer "+op, "", "", map[string]any{"name": "mine", "project_id": r.projectA, "sensors": []map[string]string{fixedA}, "temp_max": 8})
	if code != 201 {
		t.Fatal(code, mine)
	}
	code, updated, _ := req(t, r.api, "POST", "/api/v1/reports/definitions/"+mine["id"].(string)+"/update", "Bearer "+op, "", "", map[string]any{"name": "mine, renamed", "project_id": r.projectA, "sensors": []map[string]string{fixedA}, "temp_min": 2, "temp_max": 8})
	if code != 200 || updated["name"] != "mine, renamed" || updated["temp_min"] != 2.0 {
		t.Fatalf("update: %d %v", code, updated)
	}
	// Run a saved report: the operator's run of the workspace report covers the sensor they can see; once Fridge A
	// turns into a roaming tag the report leaves it out and says why.
	code, run, _ := req(t, r.api, "POST", "/api/v1/reports/summary", "Bearer "+op, "", "", r.window(map[string]any{"report_id": ws["id"]}))
	if code != 200 || len(listOf(run, "sensors")) != 1 || run["report"].(map[string]any)["name"] != "ห้องเย็นทั้งหมด" || len(listOf(listOf(run, "sensors")[0], "excursions")) != 3 {
		t.Fatalf("run saved report: %d %v", code, run)
	}
	if code, out, _ := req(t, r.api, "POST", "/api/v1/reports/summary", "Bearer "+op, "", "", r.window(map[string]any{"report_id": ws["id"], "sensors": []map[string]string{fixedA}})); code != 400 || out["error"] != "report_or_sensors" {
		t.Fatalf("report and sensors: %d %v", code, out)
	}
	r.exec(`UPDATE core.devices SET roaming=true WHERE tenant_id=$1 AND external_id=$2`, r.owner.TenantID, r.fixedA)
	code, run, _ = req(t, r.api, "POST", "/api/v1/reports/summary", "Bearer "+r.ownerToken, "", "", r.window(map[string]any{"report_id": ws["id"]}))
	if omitted := listOf(run, "omitted"); code != 200 || len(listOf(run, "sensors")) != 1 || len(omitted) != 1 || omitted[0]["reason"] != "personal_sensor" || omitted[0]["external_id"] != r.fixedA {
		t.Fatalf("saved report with a roaming sensor: %d %v", code, run)
	}
	r.exec(`UPDATE core.devices SET roaming=false WHERE tenant_id=$1 AND external_id=$2`, r.owner.TenantID, r.fixedA)
	// A viewer reads but never writes; another workspace sees nothing.
	vEmail := memberEmail()
	addMember(t, r.api, r.ownerToken, vEmail, "viewer", nil)
	vAuth, _ := changeInitialPassword(t, r.f, r.api, vEmail, r.owner.TenantID)
	if code, out := get(t, r.api, "/api/v1/reports/definitions", vAuth.AccessToken); code != 200 || len(rows(out)) != 3 {
		t.Fatalf("viewer list: %d %v", code, out)
	}
	if code, _, _ := req(t, r.api, "POST", "/api/v1/reports/definitions/"+mine["id"].(string)+"/delete", "Bearer "+vAuth.AccessToken, "", "", nil); code != 403 {
		t.Fatalf("viewer delete: %d", code)
	}
	_, otherAuth, other := r.f.account(t)
	if code, out := get(t, r.api, "/api/v1/reports/definitions", otherAuth.AccessToken); code != 200 || len(rows(out)) != 0 {
		t.Fatalf("cross-tenant list: %d %v", code, out)
	}
	if code, _ := get(t, r.api, "/api/v1/reports/definitions/"+ws["id"].(string), otherAuth.AccessToken); code != 404 {
		t.Fatalf("cross-tenant get: %d", code)
	}
	if code, _, _ := req(t, r.api, "POST", "/api/v1/reports/definitions/"+ws["id"].(string)+"/delete", "Bearer "+otherAuth.AccessToken, "", "", nil); code != 404 {
		t.Fatalf("cross-tenant delete: %d", code)
	}
	// RLS, not only the handlers: the runtime role in the other workspace counts zero rows, and cannot insert into ours.
	tx := asRuntime(t, r.f, other.UserID, other.TenantID)
	var n int
	if e := tx.QueryRow(`SELECT count(*) FROM core.report_definitions`).Scan(&n); e != nil || n != 0 {
		t.Fatalf("cross-tenant rows: %d %v", n, e)
	}
	if _, e := tx.Exec(`INSERT INTO core.report_definitions(tenant_id,id,name,sensors) VALUES($1,$2,'x','[{}]')`, r.owner.TenantID, uuid.NewString()); e == nil {
		t.Fatal("RLS let a foreign workspace insert a report")
	}
	tx.Rollback()
	if code, _, _ := req(t, r.api, "POST", "/api/v1/reports/definitions/"+mine["id"].(string)+"/delete", "Bearer "+op, "", "", nil); code != 204 {
		t.Fatalf("operator delete: %d", code)
	}
	if code, _ := get(t, r.api, "/api/v1/reports/definitions/"+mine["id"].(string), r.ownerToken); code != 404 {
		t.Fatalf("deleted report: %d", code)
	}
	var audits int
	if e := r.f.admin.QueryRowContext(ctx, `SELECT count(*) FROM core.audit_logs WHERE tenant_id=$1 AND action LIKE 'report.%'`, r.owner.TenantID).Scan(&audits); e != nil || audits != 5 {
		t.Fatalf("audit rows: %d %v", audits, e)
	}
}

// A tag that was roaming (a person's movements, never rolled up) and is a fixed sensor now: a raw export covers only
// the time since it is one, never the roaming period, although the samples are still stored.
func TestReportRawSkipsPersonalPeriods(t *testing.T) {
	r := newReportRig(t)
	ctx := context.Background()
	ext := "f1" + r.fixedA[2:]
	dev, e := r.f.service.CreateDevice(ctx, r.owner, r.gwA, "Was a wristband", ext, "generic-environment@1")
	if e != nil {
		t.Fatal(e)
	}
	if e := r.f.repo.SetDeviceRoaming(ctx, r.owner, dev.ID, true); e != nil {
		t.Fatal(e)
	}
	r.exec(`INSERT INTO core.sensor_streams(tenant_id,gateway_id,external_id,name,last_seen) VALUES($1,$2,$3,'stream',now())`, r.owner.TenantID, r.gwA, ext)
	sample := func(at time.Time, temp int) {
		r.exec(`INSERT INTO core.sensor_samples(tenant_id,gateway_id,external_id,event_key,received_at,decoder_id,reading) VALUES($1,$2,$3,$4,$5,'test',$6::jsonb)`,
			r.owner.TenantID, r.gwA, ext, uuid.NewString(), at, `{"temperature":`+strconv.Itoa(temp)+`}`)
	}
	roaming := r.from.Add(3 * time.Hour)
	sample(roaming, 30)
	sample(roaming.Add(10*time.Minute), 31)
	if _, e := r.f.repo.RollupRange(ctx, r.owner.TenantID, roaming, roaming.Add(15*time.Minute)); e != nil {
		t.Fatal(e)
	}
	if e := r.f.repo.SetDeviceRoaming(ctx, r.owner, dev.ID, false); e != nil {
		t.Fatal(e)
	}
	fixed := r.from.Add(4 * time.Hour)
	sample(fixed, 20)
	if _, e := r.f.repo.RollupRange(ctx, r.owner.TenantID, fixed, fixed.Add(time.Minute)); e != nil {
		t.Fatal(e)
	}
	for _, resolution := range []string{"raw", "5m"} {
		code, body, _ := r.export(r.ownerToken, []map[string]string{reportSensor(r.gwA, ext)}, resolution)
		rows := readCSV(t, body)
		if code != 200 || len(rows) != 2 || rows[1][0] != fixed.Format("2006-01-02 15:04:05") {
			t.Fatalf("%s export of a former roaming tag: %d %v", resolution, code, rows)
		}
	}
	// Only the fixed period's sample is exportable, and the summary sees only that period too.
	_, out, _ := req(t, r.api, "POST", "/api/v1/reports/summary", "Bearer "+r.ownerToken, "", "", r.window(map[string]any{"sensors": []map[string]string{reportSensor(r.gwA, ext)}}))
	if items := listOf(out, "sensors"); len(items) != 1 || items[0]["buckets"] != 1.0 || items[0]["temperature"].(map[string]any)["max"] != 20.0 {
		t.Fatalf("summary of a former roaming tag: %v", out)
	}
}

func TestReportRawBudget(t *testing.T) {
	r := newReportRig(t)
	sensors := []map[string]string{reportSensor(r.gwA, r.fixedA), reportSensor(r.gwB, r.fixedB)}
	month := map[string]any{"from": r.from.Format(time.RFC3339), "to": r.from.Add(31 * 24 * time.Hour).Format(time.RFC3339), "sensors": sensors}
	month["resolution"] = "raw"
	// 2 sensors x 31 days is within 120 sensor-days ...
	if code, body, _ := postRaw(t, r.api, "/api/v1/reports/export", r.ownerToken, month); code != 200 {
		t.Fatalf("raw within budget: %d %s", code, body)
	}
	// ... 5 sensors x 31 days is not; the same request aggregated is.
	for i := 0; i < 3; i++ {
		ext := "ab" + strconv.Itoa(i) + r.fixedA[3:]
		if _, e := r.f.service.CreateDevice(context.Background(), r.owner, r.gwA, "Extra "+strconv.Itoa(i), ext, "generic-environment@1"); e != nil {
			t.Fatal(e)
		}
		sensors = append(sensors, reportSensor(r.gwA, ext))
	}
	month["sensors"] = sensors
	if code, out, _ := req(t, r.api, "POST", "/api/v1/reports/export", "Bearer "+r.ownerToken, "", "", month); code != 400 || out["error"] != "raw_too_large" {
		t.Fatalf("raw over budget: %d %v", code, out)
	}
	month["resolution"] = "5m"
	if code, body, _ := postRaw(t, r.api, "/api/v1/reports/export", r.ownerToken, month); code != 200 {
		t.Fatalf("aggregated month: %d %s", code, body)
	}
}

// A project report whose gateway later moved to another project: a member limited to the first project edits it
// without seeing that sensor, and saving keeps it instead of dropping it.
func TestReportDefinitionKeepsSensorsTheEditorCannotSee(t *testing.T) {
	r := newReportRig(t)
	ctx := context.Background()
	ga2, _, e := r.f.service.CreateGatewayIn(ctx, r.owner, "Gateway A2", "minew-mg3", &r.projectA)
	if e != nil {
		t.Fatal(e)
	}
	ext := "c2" + r.fixedA[2:]
	if _, e := r.f.service.CreateDevice(ctx, r.owner, ga2.ID, "Fridge A2", ext, "generic-environment@1"); e != nil {
		t.Fatal(e)
	}
	both := []map[string]string{reportSensor(r.gwA, r.fixedA), reportSensor(ga2.ID, ext)}
	code, def, _ := req(t, r.api, "POST", "/api/v1/reports/definitions", "Bearer "+r.ownerToken, "", "", map[string]any{"name": "A", "project_id": r.projectA, "sensors": both})
	if code != 201 {
		t.Fatal(code, def)
	}
	if e := r.f.repo.SetGatewayProject(ctx, r.owner, ga2.ID, &r.projB); e != nil {
		t.Fatal(e)
	}
	email := memberEmail()
	addMember(t, r.api, r.ownerToken, email, "operator", []string{r.projectA})
	auth, _ := changeInitialPassword(t, r.f, r.api, email, r.owner.TenantID)
	id := def["id"].(string)
	if code, out := get(t, r.api, "/api/v1/reports/definitions/"+id, auth.AccessToken); code != 200 || len(out["sensors"].([]any)) != 1 {
		t.Fatalf("editor's view: %d %v", code, out)
	}
	code, saved, _ := req(t, r.api, "POST", "/api/v1/reports/definitions/"+id+"/update", "Bearer "+auth.AccessToken, "", "", map[string]any{"name": "A, renamed", "project_id": r.projectA, "sensors": []map[string]string{reportSensor(r.gwA, r.fixedA)}})
	if code != 200 || saved["name"] != "A, renamed" || len(saved["sensors"].([]any)) != 1 {
		t.Fatalf("editor's save: %d %v", code, saved)
	}
	code, owner, _ := req(t, r.api, "GET", "/api/v1/reports/definitions/"+id, "Bearer "+r.ownerToken, "", "", nil)
	kept := map[string]bool{}
	for _, raw := range owner["sensors"].([]any) {
		kept[raw.(map[string]any)["external_id"].(string)] = true
	}
	if code != 200 || owner["name"] != "A, renamed" || len(kept) != 2 || !kept[ext] || !kept[r.fixedA] {
		t.Fatalf("the hidden sensor was dropped: %d %v", code, owner)
	}
}
