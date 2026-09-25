package tests

import (
	"aether/backend/internal/adapters/zigbee2mqtt"
	"aether/backend/internal/domain"
	"aether/backend/internal/simulation"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

// armed creates and enables a "door opens -> bulb on" flow as the member holding `token`.
func (r *automationRig) armed(token string, project *string) string {
	r.t.Helper()
	code, out := r.create(token, commandFlow([]string{"door_open"}, r.door.IEEE, r.devices["light"], "brightness", 120), true, project)
	if code != 201 || out["enabled"] != true {
		r.t.Fatalf("arm: %d %v", code, out)
	}
	return out["id"].(string)
}

// fire forgets the flow's previous firings (the 60 s de-duplication) and opens the door once more.
func (r *automationRig) fire(flow string) {
	r.t.Helper()
	if _, e := r.f.admin.ExecContext(r.ctx, `DELETE FROM core.automation_runs WHERE automation_id=$1`, flow); e != nil {
		r.t.Fatal(e)
	}
	r.closeDoor()
	r.openDoor()
}

func (r *automationRig) post(token, path string, body map[string]any) int {
	r.t.Helper()
	code, _, _ := req(r.t, r.api, "POST", path, "Bearer "+token, "", "", body)
	return code
}

func (r *automationRig) drain() {
	r.t.Helper()
	if _, e := r.f.repo.ProcessAutomationRequests(r.ctx, r.owner.TenantID, time.Now().UTC()); e != nil {
		r.t.Fatal(e)
	}
}

// The member who armed a flow answers for its commands, so their authority is re-checked when the flow fires and
// again when the commander queues the command; and taking it away through the team page switches the flow off.
func TestAutomationCommandArmerAuthority(t *testing.T) {
	r := newAutomationRig(t, true)
	admin, token := r.member("admin", []string{})
	flow := r.armed(token, nil)
	setRole := func(role string) {
		if _, e := r.f.admin.ExecContext(r.ctx, `UPDATE core.memberships SET role=$1 WHERE tenant_id=$2 AND user_id=$3`, role, r.owner.TenantID, admin); e != nil {
			t.Fatal(e)
		}
	}
	// Demoted behind the team page's back (so nothing disarmed the flow): the firing itself refuses.
	setRole("viewer")
	r.fire(flow)
	if got := r.runReason(flow); got != "blocked:arming_member_lacks_control" {
		t.Fatalf("fire after demotion: %s", got)
	}
	setRole("admin")
	// Demoted between the firing and the commander: the commander refuses.
	r.fire(flow)
	setRole("viewer")
	r.drain()
	if got := r.requestReason(flow); got != "blocked:arming_member_lacks_control" {
		t.Fatalf("drain after demotion: %s", got)
	}
	if n := r.automationCommands(flow); n != 0 {
		t.Fatalf("queued %d commands for a demoted armer", n)
	}
	setRole("admin")

	disarmed := func(how string) {
		t.Helper()
		if r.enabled(flow) {
			t.Fatalf("%s: the flow is still armed", how)
		}
		if n := r.count(t, `SELECT count(*) FROM core.audit_logs WHERE tenant_id=$1 AND action='automation.disarmed' AND target_id=$2`, r.owner.TenantID, flow); n == 0 {
			t.Fatalf("%s: no disarm audit row", how)
		}
		code, out, _ := req(t, r.api, "GET", "/api/v1/automations/"+flow, "Bearer "+r.ownerAuth.AccessToken, "", "", nil)
		if code != 200 || out["enabled"] != false {
			t.Fatalf("%s: API still shows it armed: %d %v", how, code, out)
		}
	}
	rearm := func() {
		t.Helper()
		if code := r.post(token, "/api/v1/automations/"+flow+"/enable", map[string]any{"enabled": true}); code != 200 {
			t.Fatalf("re-arm: %d", code)
		}
	}
	// Demoted on the team page.
	if code := r.post(r.ownerAuth.AccessToken, "/api/v1/members/"+admin+"/update", map[string]any{"role": "viewer", "project_ids": []string{}}); code >= 300 {
		t.Fatalf("demote: %d", code)
	}
	disarmed("demotion")
	if code := r.post(r.ownerAuth.AccessToken, "/api/v1/members/"+admin+"/update", map[string]any{"role": "admin", "project_ids": []string{}}); code >= 300 {
		t.Fatalf("promote: %d", code)
	}
	rearm()
	// The control module closed.
	if code := r.post(r.ownerAuth.AccessToken, "/api/v1/members/"+admin+"/access", map[string]any{"control": "none"}); code != 204 {
		t.Fatalf("access: %d", code)
	}
	disarmed("control none")
	if code := r.post(r.ownerAuth.AccessToken, "/api/v1/members/"+admin+"/access", map[string]any{}); code != 204 {
		t.Fatalf("access reset: %d", code)
	}
	rearm()
	// Restricted to project P while the gateway sits in P: still armed. The gateway moves to Q: disarmed.
	p, e := r.f.service.CreateProject(r.ctx, r.owner, "P", "", "mint")
	if e != nil {
		t.Fatal(e)
	}
	q, e := r.f.service.CreateProject(r.ctx, r.owner, "Q", "", "blue")
	if e != nil {
		t.Fatal(e)
	}
	if code := r.post(r.ownerAuth.AccessToken, "/api/v1/gateways/"+r.gateway+"/project", map[string]any{"project_id": p.ID}); code >= 300 {
		t.Fatalf("gateway to P: %d", code)
	}
	if code := r.post(r.ownerAuth.AccessToken, "/api/v1/members/"+admin+"/update", map[string]any{"role": "admin", "project_ids": []string{p.ID}}); code >= 300 {
		t.Fatalf("restrict to P: %d", code)
	}
	if !r.enabled(flow) {
		t.Fatal("an armer whose project holds the target must stay armed")
	}
	if code := r.post(r.ownerAuth.AccessToken, "/api/v1/gateways/"+r.gateway+"/project", map[string]any{"project_id": q.ID}); code >= 300 {
		t.Fatalf("gateway to Q: %d", code)
	}
	disarmed("gateway moved out of the armer's projects")
	// Back to an unrestricted admin, re-armed, then removed from the workspace.
	if code := r.post(r.ownerAuth.AccessToken, "/api/v1/members/"+admin+"/update", map[string]any{"role": "admin", "project_ids": []string{}}); code >= 300 {
		t.Fatalf("unrestrict: %d", code)
	}
	rearm()
	if code := r.post(r.ownerAuth.AccessToken, "/api/v1/members/"+admin+"/remove", nil); code >= 300 {
		t.Fatalf("remove: %d", code)
	}
	disarmed("removal")
}

// A project flow's target is re-checked against the flow's project when the command is queued.
func TestAutomationCommandTargetLeftTheProject(t *testing.T) {
	r := newAutomationRig(t, true)
	p, e := r.f.service.CreateProject(r.ctx, r.owner, "P", "", "mint")
	if e != nil {
		t.Fatal(e)
	}
	q, e := r.f.service.CreateProject(r.ctx, r.owner, "Q", "", "blue")
	if e != nil {
		t.Fatal(e)
	}
	if e := r.f.service.Repo.SetGatewayProject(r.ctx, r.owner, r.gateway, &p.ID); e != nil {
		t.Fatal(e)
	}
	flow := r.armed(r.ownerAuth.AccessToken, &p.ID)
	r.openDoor()
	if n := r.requests(flow, "requested"); n != 1 {
		t.Fatalf("requests: %d", n)
	}
	if e := r.f.service.Repo.SetGatewayProject(r.ctx, r.owner, r.gateway, &q.ID); e != nil {
		t.Fatal(e)
	}
	r.drain()
	if got := r.requestReason(flow); got != "blocked:out_of_project" {
		t.Fatalf("outcome: %s", got)
	}
}

// Automations get at most half of the workspace budget, so a person can always still send commands by hand.
func TestAutomationCommandBudgetLeavesManualHeadroom(t *testing.T) {
	r := newAutomationRig(t, true)
	flow := r.armed(r.ownerAuth.AccessToken, nil)
	cover := r.devices["cover"]
	for i := 0; i < domain.AutomationCommandsPerMinute; i++ {
		if _, e := r.f.admin.ExecContext(r.ctx, `INSERT INTO core.device_commands(tenant_id,id,gateway_id,device_id,ieee,property,requested,value,source,status,created_at,expires_at)
      VALUES($1,$2,$3,$4,$5,'position','set','10','automation','confirmed',now(),now()+interval '10 seconds')`,
			r.owner.TenantID, uuid.NewString(), r.gateway, cover, simulation.Z2MActuators[1].IEEE); e != nil {
			t.Fatal(e)
		}
	}
	r.openDoor()
	r.drain()
	if got := r.requestReason(flow); got != "blocked:automation_budget" {
		t.Fatalf("automation over its budget: %s", got)
	}
	// Manual control still has the other half.
	if code, out := r.send(t, r.ownerAuth.AccessToken, uuid.NewString(), map[string]any{"device_id": r.devices["light"], "property": "brightness", "value": 30}); code != 202 {
		t.Fatalf("manual command with automations at their budget: %d %v", code, out)
	}
}

// Two commander sweeps at once cannot both take the last per-device slot: the count and the insert happen under the
// tenant command lock.
func TestAutomationCommandPerDeviceCapIsAtomic(t *testing.T) {
	r := newAutomationRig(t, true)
	light, bulb := r.devices["light"], simulation.Z2MActuators[0]
	flow := r.armed(r.ownerAuth.AccessToken, nil)
	for i := 0; i < domain.AutomationCommandsPerDevice-1; i++ {
		if _, e := r.f.admin.ExecContext(r.ctx, `INSERT INTO core.device_commands(tenant_id,id,gateway_id,device_id,ieee,property,requested,value,source,status,created_at,expires_at)
      VALUES($1,$2,$3,$4,$5,'brightness','set','10','automation','confirmed',now(),now()+interval '10 seconds')`,
			r.owner.TenantID, uuid.NewString(), r.gateway, light, bulb.IEEE); e != nil {
			t.Fatal(e)
		}
	}
	for _, prop := range []string{"brightness", "color_temp"} {
		value := "100"
		if prop == "color_temp" {
			value = "300"
		}
		if _, e := r.f.admin.ExecContext(r.ctx, `INSERT INTO core.automation_command_requests(tenant_id,id,automation_id,run_node,device_id,property,value,actor_id)
      VALUES($1,$2,$3,'k1',$4,$5,$6::jsonb,$7)`, r.owner.TenantID, uuid.NewString(), flow, light, prop, value, r.owner.UserID); e != nil {
			t.Fatal(e)
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, e := r.f.repo.ProcessAutomationRequests(r.ctx, r.owner.TenantID, time.Now().UTC()); e != nil {
				errs <- e
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	queued, capped := r.requests(flow, "queued"), r.count(t, `SELECT count(*) FROM core.automation_command_requests WHERE automation_id=$1 AND reason='automation_cap'`, flow)
	if queued != 1 || capped != 1 {
		t.Fatalf("one slot left: %d queued, %d capped", queued, capped)
	}
}

// Lock order: ingest (holding its gateway row) firing a command flow, commands from the web on the same gateway, and
// the commander turning requests into commands, all at once and repeatedly. Before the outbox, ingest took the tenant
// command lock after its gateway row and the web took them the other way round: a deadlock (40P01) under load.
func TestAutomationCommandLockOrderUnderConcurrency(t *testing.T) {
	r := newAutomationRig(t, true)
	flow := r.armed(r.ownerAuth.AccessToken, nil)
	const rounds = 25
	var wg sync.WaitGroup
	errs := make(chan string, rounds*4)
	capture := func(msgs []simulation.Z2MMessage) error {
		for _, m := range msgs {
			gateway, msg, e := zigbee2mqtt.Route(m.Topic)
			if e != nil {
				return e
			}
			if msg.Kind == zigbee2mqtt.Ignore {
				continue
			}
			if _, e := r.f.service.CaptureZ2M(r.ctx, r.owner.TenantID, gateway, msg, m.Payload); e != nil {
				return e
			}
		}
		return nil
	}
	wg.Add(3)
	go func() { // ingest: the door flaps and the flow fires every time
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			if _, e := r.f.admin.ExecContext(r.ctx, `DELETE FROM core.automation_runs WHERE automation_id=$1`, flow); e != nil {
				errs <- "reset runs: " + e.Error()
				return
			}
			for _, closed := range []bool{true, false} {
				if e := capture(r.bridge.Set(r.door.IEEE, "contact", closed)); e != nil {
					errs <- "ingest: " + e.Error()
					return
				}
			}
		}
	}()
	go func() { // the web: commands on the same gateway's devices
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			device, prop := r.devices["light"], "brightness"
			if i%2 == 1 {
				device, prop = r.devices["cover"], "position"
			}
			b, _ := json.Marshal(map[string]any{"device_id": device, "property": prop, "value": 10 + i})
			q := httptest.NewRequest("POST", "/api/v1/commands", bytes.NewReader(b))
			q.Header.Set("Content-Type", "application/json")
			q.Header.Set("Authorization", "Bearer "+r.ownerAuth.AccessToken)
			q.Header.Set("Idempotency-Key", uuid.NewString())
			res, e := r.api.Test(q, fiber.TestConfig{Timeout: 30 * time.Second})
			if e != nil {
				errs <- "web: " + e.Error()
				return
			}
			res.Body.Close()
			if res.StatusCode != 202 && res.StatusCode != 409 && res.StatusCode != 429 {
				errs <- fmt.Sprintf("web: status %d", res.StatusCode)
			}
		}
	}()
	go func() { // the commander's half: requests -> commands, lock first
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			if _, e := r.f.repo.ProcessAutomationRequests(r.ctx, r.owner.TenantID, time.Now().UTC()); e != nil {
				errs <- "drain: " + e.Error()
				return
			}
		}
	}()
	wg.Wait()
	close(errs)
	var failures []string
	for e := range errs {
		failures = append(failures, e)
	}
	if len(failures) > 0 {
		t.Fatalf("%d failures, e.g. %s", len(failures), strings.Join(failures[:min(3, len(failures))], " | "))
	}
	if n := r.requests(flow, ""); n == 0 {
		t.Fatal("the flow never fired during the run")
	}
}
