package tests

import (
	"aether/backend/internal/adapters/httpapi"
	"aether/backend/internal/domain"
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
)

// The asset registry lists every gateway and every active device, carries the owner's bookkeeping, and
// turns a maintenance log bound to a plan into the next due date. Everything stays inside one workspace.
func TestAssetRegistryMaintenanceAndIsolation(t *testing.T) {
	f := setup(t)
	_, authA, a := f.account(t)
	_, authB, _ := f.account(t)
	ctx := context.Background()
	api := httpapi.New(f.cfg, f.service, f.repo)

	gw, _, e := f.service.CreateGateway(ctx, a, "ตึก A ชั้น 1", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	device, e := f.service.CreateDevice(ctx, a, gw.ID, "ตู้เย็นวัคซีน", "f00000000011", "generic-environment@1")
	if e != nil {
		t.Fatal(e)
	}

	// The list carries both assets before anything is recorded, and only inside this workspace.
	code, out, _ := req(t, api, "GET", "/api/v1/assets", "Bearer "+authA.AccessToken, "", "", nil)
	if code != 200 || len(out["items"].([]any)) != 2 {
		t.Fatalf("list assets: %d %v", code, out)
	}
	if code, out, _ = req(t, api, "GET", "/api/v1/assets", "Bearer "+authB.AccessToken, "", "", nil); code != 200 || len(out["items"].([]any)) != 0 {
		t.Fatalf("registry leaked across tenants: %d %v", code, out)
	}

	// Tenant B can neither read nor write tenant A's asset.
	path := "/api/v1/assets/device/" + device.ID
	if code, _, _ = req(t, api, "GET", path, "Bearer "+authB.AccessToken, "", "", nil); code != 404 {
		t.Fatalf("foreign tenant read an asset: %d", code)
	}
	if code, _, _ = req(t, api, "POST", path, "Bearer "+authB.AccessToken, "", "", map[string]any{"serial_no": "STOLEN"}); code != 404 {
		t.Fatalf("foreign tenant wrote an asset record: %d", code)
	}

	// Bad input is rejected before it reaches the database.
	if code, _, _ = req(t, api, "POST", path, "Bearer "+authA.AccessToken, "", "", map[string]any{"status": "broken"}); code != 400 {
		t.Fatalf("invalid status accepted: %d", code)
	}
	if code, _, _ = req(t, api, "POST", path, "Bearer "+authA.AccessToken, "", "", map[string]any{"warranty_until": "2026-13-01"}); code != 400 {
		t.Fatalf("invalid date accepted: %d", code)
	}

	// Upsert and read back, including a note that Excel would otherwise execute.
	formula := "=cmd|' /C calc'!A0"
	record := map[string]any{"serial_no": "SN-0099", "asset_tag": "AET-001", "location_note": "ห้องยา ชั้น 1", "vendor": "Minew", "purchased_at": "2025-02-01", "warranty_until": "2099-02-01", "status": "in_service", "notes": formula}
	if code, _, _ = req(t, api, "POST", path, "Bearer "+authA.AccessToken, "", "", record); code != 200 {
		t.Fatalf("upsert record: %d", code)
	}
	code, out, _ = req(t, api, "GET", path, "Bearer "+authA.AccessToken, "", "", nil)
	rec, _ := out["record"].(map[string]any)
	if code != 200 || rec["serial_no"] != "SN-0099" || rec["asset_tag"] != "AET-001" || rec["purchased_at"] != "2025-02-01" || rec["vendor"] != "Minew" {
		t.Fatalf("record not stored: %d %v", code, out)
	}

	// A plan due yesterday is overdue in the list summary.
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	code, out, _ = req(t, api, "POST", path+"/plans", "Bearer "+authA.AccessToken, "", "", map[string]any{"title": "สอบเทียบเซ็นเซอร์", "kind": "calibration", "interval_days": 90, "next_due": yesterday})
	if code != 201 || out["id"] == nil {
		t.Fatalf("create plan: %d %v", code, out)
	}
	plan := out["id"].(string)
	if code, _, _ = req(t, api, "POST", path+"/plans", "Bearer "+authA.AccessToken, "", "", map[string]any{"title": "ช่วงเวลาเกินพิกัด", "kind": "calibration", "interval_days": 4000, "next_due": yesterday}); code != 400 {
		t.Fatalf("out of range interval accepted: %d", code)
	}
	if code, _, _ = req(t, api, "POST", "/api/v1/plans/"+plan+"/update", "Bearer "+authB.AccessToken, "", "", map[string]any{"title": "hijack", "kind": "pm", "interval_days": 7, "next_due": yesterday}); code != 404 {
		t.Fatalf("foreign tenant updated a plan: %d", code)
	}
	code, out, _ = req(t, api, "GET", "/api/v1/assets", "Bearer "+authA.AccessToken, "", "", nil)
	if code != 200 || assetRow(t, out, device.ID)["overdue_count"].(float64) != 1 {
		t.Fatalf("overdue count missing: %v", out)
	}

	// Completing the plan through a log advances next_due by exactly interval_days.
	done := time.Now().UTC().Format("2006-01-02")
	code, out, _ = req(t, api, "POST", path+"/logs", "Bearer "+authA.AccessToken, "", "", map[string]any{"plan_id": plan, "kind": "calibration", "title": "สอบเทียบแล้ว", "detail": "ค่าคลาดเคลื่อน 0.2C", "performed_at": done, "performed_by": "ช่างเอ", "cost": 850.5})
	if code != 201 {
		t.Fatalf("add log: %d %v", code, out)
	}
	want := time.Now().UTC().AddDate(0, 0, 90).Format("2006-01-02")
	plans, _ := out["plans"].([]any)
	if len(plans) != 1 || plans[0].(map[string]any)["next_due"] != want || plans[0].(map[string]any)["last_done"] != done {
		t.Fatalf("plan must advance to %s: %v", want, plans)
	}
	logs, _ := out["logs"].([]any)
	if len(logs) != 1 || logs[0].(map[string]any)["performed_by"] != "ช่างเอ" || logs[0].(map[string]any)["cost"].(float64) != 850.5 {
		t.Fatalf("log not stored: %v", out["logs"])
	}
	if code, _, _ = req(t, api, "POST", path+"/logs", "Bearer "+authA.AccessToken, "", "", map[string]any{"kind": "note", "title": "ค่าซ่อมติดลบ", "performed_at": done, "cost": -1}); code != 400 {
		t.Fatalf("negative cost accepted: %d", code)
	}

	// A battery log stamps the record, so "เปลี่ยนแบตเมื่อไร" never has to be typed twice.
	if code, _, _ = req(t, api, "POST", path+"/logs", "Bearer "+authA.AccessToken, "", "", map[string]any{"kind": "battery", "title": "เปลี่ยนถ่าน CR2477", "performed_at": done}); code != 201 {
		t.Fatalf("battery log: %d", code)
	}
	code, out, _ = req(t, api, "GET", path, "Bearer "+authA.AccessToken, "", "", nil)
	if code != 200 || out["record"].(map[string]any)["battery_changed_at"] != done {
		t.Fatalf("battery log did not stamp the record: %v", out["record"])
	}

	// Plans can be deleted; the history they produced stays.
	if code, _, _ = req(t, api, "POST", "/api/v1/plans/"+plan+"/delete", "Bearer "+authA.AccessToken, "", "", map[string]any{}); code != 204 {
		t.Fatalf("delete plan: %d", code)
	}
	code, out, _ = req(t, api, "GET", path, "Bearer "+authA.AccessToken, "", "", nil)
	if code != 200 || len(out["plans"].([]any)) != 0 || len(out["logs"].([]any)) != 2 {
		t.Fatalf("history must survive plan deletion: %v", out)
	}

	// A viewer reads the registry but cannot change it.
	if _, e := f.admin.ExecContext(ctx, `UPDATE core.memberships SET role='viewer' WHERE tenant_id=$1 AND user_id=$2`, a.TenantID, a.UserID); e != nil {
		t.Fatal(e)
	}
	if code, _, _ = req(t, api, "GET", "/api/v1/assets", "Bearer "+authA.AccessToken, "", "", nil); code != 200 {
		t.Fatalf("viewer cannot read the registry: %d", code)
	}
	if code, _, _ = req(t, api, "POST", path, "Bearer "+authA.AccessToken, "", "", map[string]any{"serial_no": "SN-VIEWER"}); code != 403 {
		t.Fatalf("viewer wrote an asset record: %d", code)
	}
	if _, e := f.admin.ExecContext(ctx, `UPDATE core.memberships SET role='owner' WHERE tenant_id=$1 AND user_id=$2`, a.TenantID, a.UserID); e != nil {
		t.Fatal(e)
	}

	// The export opens in Excel (BOM) and never hands it a formula.
	status, csv := fetchText(t, api, "/api/v1/assets/export.csv", "Bearer "+authA.AccessToken)
	if status != 200 {
		t.Fatalf("export: %d", status)
	}
	if !strings.HasPrefix(csv, "\ufeff") {
		t.Fatal("CSV must start with a UTF-8 BOM so Excel reads Thai")
	}
	if !strings.Contains(csv, "'"+formula) || strings.Contains(csv, ","+formula) {
		t.Fatalf("a note starting with = must be neutralised: %q", csv)
	}
	if !strings.Contains(csv, "SN-0099") || !strings.Contains(csv, "ตึก A ชั้น 1") {
		t.Fatalf("export is missing rows: %q", csv)
	}

	// A withdrawn registration leaves the registry and can no longer be written to.
	if e := f.repo.RemoveDevice(ctx, a, device.ID); e != nil {
		t.Fatal(e)
	}
	code, out, _ = req(t, api, "GET", "/api/v1/assets", "Bearer "+authA.AccessToken, "", "", nil)
	if code != 200 || len(out["items"].([]any)) != 1 {
		t.Fatalf("removed device still listed: %v", out)
	}
	if code, _, _ = req(t, api, "GET", path, "Bearer "+authA.AccessToken, "", "", nil); code != 404 {
		t.Fatalf("removed device still addressable: %d", code)
	}
	if e := f.repo.UpsertAssetRecord(ctx, a, domain.AssetRecord{AssetKind: "device", AssetID: device.ID, Status: "retired"}); !errors.Is(e, domain.ErrNotFound) {
		t.Fatalf("record written for a removed device: %v", e)
	}
}

// assetRow finds one asset in a list response by its id.
func assetRow(t *testing.T, out map[string]any, id string) map[string]any {
	t.Helper()
	for _, raw := range out["items"].([]any) {
		row := raw.(map[string]any)
		if row["asset_id"] == id {
			return row
		}
	}
	t.Fatalf("asset %s not in the registry: %v", id, out["items"])
	return nil
}

// text fetches a non-JSON response body (req decodes JSON and would fail on the CSV export).
func fetchText(t *testing.T, api *fiber.App, path, token string) (int, string) {
	t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	r.Header.Set("Authorization", token)
	res, e := api.Test(r, fiber.TestConfig{Timeout: 15 * time.Second})
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	raw, e := io.ReadAll(res.Body)
	if e != nil {
		t.Fatal(e)
	}
	return res.StatusCode, string(raw)
}
