package tests

import (
	"aether/backend/internal/adapters/httpapi"
	"aether/backend/internal/adapters/postgres"
	"aether/backend/internal/domain"
	"aether/backend/internal/realtime"
	"aether/backend/internal/simulation"
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestProjectsGroupGatewaysWithinTenant(t *testing.T) {
	f := setup(t)
	_, authA, a := f.account(t)
	_, _, b := f.account(t)
	ctx := context.Background()
	api := httpapi.New(f.cfg, f.service, f.repo)

	code, out, _ := req(t, api, "POST", "/api/v1/projects", "Bearer "+authA.AccessToken, "", "", map[string]any{"name": "โรงพยาบาล A", "description": "ตึกผู้ป่วยใน", "color": "blue"})
	if code != 201 || out["id"] == nil {
		t.Fatalf("create project: %d %v", code, out)
	}
	hospital := out["id"].(string)
	code, _, _ = req(t, api, "POST", "/api/v1/projects", "Bearer "+authA.AccessToken, "", "", map[string]any{"name": "โรงพยาบาล a", "color": "mint"})
	if code != 409 {
		t.Fatalf("duplicate project name (case-insensitive): %d", code)
	}
	code, _, _ = req(t, api, "POST", "/api/v1/projects", "Bearer "+authA.AccessToken, "", "", map[string]any{"name": "x", "color": "neon"})
	if code != 400 {
		t.Fatalf("invalid color accepted: %d", code)
	}
	factory, e := f.service.CreateProject(ctx, a, "Factory", "", "amber")
	if e != nil {
		t.Fatal(e)
	}
	foreign, e := f.service.CreateProject(ctx, b, "Other tenant", "", "mint")
	if e != nil {
		t.Fatal(e)
	}

	// Gateways can be created inside a project, moved between projects, and never into a foreign one.
	code, out, _ = req(t, api, "POST", "/api/v1/gateways", "Bearer "+authA.AccessToken, "", "", map[string]any{"name": "Ward MG3", "model": "minew-mg3", "project_id": hospital})
	if code != 201 {
		t.Fatalf("gateway in project: %d %v", code, out)
	}
	gw := out["gateway"].(map[string]any)["id"].(string)
	if _, _, e := f.service.CreateGatewayIn(ctx, a, "Bad", "minew-mg3", &foreign.ID); !errors.Is(e, domain.ErrNotFound) {
		t.Fatalf("gateway created in a foreign project: %v", e)
	}
	if e := f.repo.SetGatewayProject(ctx, a, gw, &foreign.ID); !errors.Is(e, domain.ErrNotFound) {
		t.Fatalf("gateway moved into a foreign project: %v", e)
	}
	if e := f.repo.SetGatewayProject(ctx, b, gw, nil); !errors.Is(e, domain.ErrNotFound) {
		t.Fatalf("foreign tenant changed a gateway's project: %v", e)
	}
	code, _, _ = req(t, api, "POST", "/api/v1/gateways/"+gw+"/project", "Bearer "+authA.AccessToken, "", "", map[string]any{"project_id": factory.ID})
	if code != 204 {
		t.Fatalf("move gateway: %d", code)
	}
	gateways, e := f.repo.ListGateways(ctx, a)
	if e != nil || len(gateways) != 1 || gateways[0].ProjectID == nil || *gateways[0].ProjectID != factory.ID {
		t.Fatalf("gateway project: %+v %v", gateways, e)
	}
	projects, e := f.repo.ListProjects(ctx, a, false)
	if e != nil || len(projects) != 2 || projects[1].GatewayCount != 1 || projects[0].GatewayCount != 0 {
		t.Fatalf("project counts: %+v %v", projects, e)
	}
	if other, e := f.repo.ListProjects(ctx, b, true); e != nil || len(other) != 1 || other[0].ID != foreign.ID {
		t.Fatalf("project list leaked across tenants: %+v", other)
	}
	// Archive: project disappears, its gateways become unassigned, the name can be reused.
	code, _, _ = req(t, api, "POST", "/api/v1/projects/"+factory.ID+"/archive", "Bearer "+authA.AccessToken, "", "", map[string]any{})
	if code != 204 {
		t.Fatalf("archive: %d", code)
	}
	gateways, _ = f.repo.ListGateways(ctx, a)
	if gateways[0].ProjectID != nil {
		t.Fatalf("gateway still assigned to an archived project: %+v", gateways[0])
	}
	if e := f.repo.SetGatewayProject(ctx, a, gw, &factory.ID); !errors.Is(e, domain.ErrNotFound) {
		t.Fatalf("assigned to an archived project: %v", e)
	}
	if _, e := f.service.CreateProject(ctx, a, "Factory", "", "amber"); e != nil {
		t.Fatalf("name of an archived project must be reusable: %v", e)
	}
}

func TestSignalsReachOnlyTheirTenant(t *testing.T) {
	f := setup(t)
	_, _, a := f.account(t)
	_, _, b := f.account(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := realtime.NewHub()
	go hub.Listen(ctx, os.Getenv("TEST_DATABASE_URL"), postgres.SignalChannel)
	ca, cb := hub.Register(a.TenantID), hub.Register(b.TenantID)
	defer hub.Unregister(ca)
	defer hub.Unregister(cb)
	g, _, e := f.service.CreateGateway(ctx, a, "Signal gateway", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	kinds := map[string]bool{}
	deadline := time.After(8 * time.Second)
	sent := false
	for !(kinds["packet"] && kinds["event"]) {
		if !sent && kinds["inventory"] { // listener is up (it delivered the gateway creation); now ingest
			if _, e := f.repo.CapturePacket(ctx, a.TenantID, g.ID, simulation.Packet(27, time.Now().UTC())); e != nil {
				t.Fatal(e)
			}
			sent = true
		}
		select {
		case m := <-ca.Send:
			kinds[m.Kind] = true
		case m := <-cb.Send:
			t.Fatalf("tenant B received tenant A's signal: %+v", m)
		case <-time.After(300 * time.Millisecond):
			if !kinds["inventory"] { // the LISTEN may not have been ready for the first NOTIFY; nudge again
				_ = f.repo.SetGatewayProject(ctx, a, g.ID, nil)
			}
		case <-deadline:
			t.Fatalf("signals not delivered: %+v", kinds)
		}
	}
}
