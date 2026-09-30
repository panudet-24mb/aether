// Package demotwin builds and drives the digital twin demo workspace (cmd/demo-twin,
// docs/platform/digital-twin.md). The building and its 30-minute script live in internal/simulation; this
// package turns them into a real workspace through the same service and repository calls an owner's clicks make,
// and posts the script's uplinks through the real HTTP ingest, so what the twin shows comes out of the real
// pipeline (presence, events, alerts).
package demotwin

import (
	"aether/backend/internal/alerts"
	"aether/backend/internal/app"
	"aether/backend/internal/domain"
	"aether/backend/internal/simulation"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	gatewayModel = "minew-mg3"
	envProfile   = "minew-s1-pending@1"
	pirProfile   = "minew-msp01-pending@1"
	wearProfile  = "minew-b10-pending@1"
)

// Gateway is one virtual gateway and the ingest credential run needs; the token is a secret.
type Gateway struct {
	ID    string `json:"id"`
	Token string `json:"token"`
	MAC   string `json:"mac"`
}

// State is what setup leaves behind for run and trigger (a 0600 file: it holds the owner password and the
// gateways' ingest tokens).
type State struct {
	TenantID   string    `json:"tenant_id"`
	OwnerEmail string    `json:"owner_email"`
	Password   string    `json:"owner_password"`
	ProjectID  string    `json:"project_id"`
	SiteID     string    `json:"site_id"`
	Gateways   []Gateway `json:"gateways"`
	CreatedAt  time.Time `json:"created_at"`
}

// Setup creates the demo workspace: owner, project, site with three floors, gateways, sensors, wearables
// (roaming B10 wristbands), zones bound to gateways, placements and the demo's alert rules. markDemo flags the
// workspace demo with the migration role (aether_app cannot).
func Setup(ctx context.Context, s *app.Service, email, password, workspace string, markDemo func(tenant string) error) (State, error) {
	if !s.Registration {
		return State{}, errors.New("demotwin: the service must allow registration")
	}
	b := simulation.TwinDemo()
	acct, e := s.Register(ctx, email, password, "ผู้ดูแลเดโม", workspace, false)
	if e != nil {
		return State{}, fmt.Errorf("register the demo owner: %w", e)
	}
	st := State{TenantID: acct.TenantID, OwnerEmail: acct.User.Email, Password: password, CreatedAt: time.Now().UTC()}
	if e := markDemo(acct.TenantID); e != nil {
		return st, fmt.Errorf("mark the workspace demo: %w", e)
	}
	p := domain.Principal{UserID: acct.User.ID, TenantID: acct.TenantID, Role: "owner"}
	project, e := s.CreateProject(ctx, p, "โรงพยาบาลเดโม", "อาคารสมมติสำหรับสาธิต digital twin · ข้อมูลทั้งหมดเป็นข้อมูลจำลอง", "mint")
	if e != nil {
		return st, fmt.Errorf("project: %w", e)
	}
	st.ProjectID = project.ID

	gwIDs := make([]string, len(b.Gateways))
	for i, g := range b.Gateways {
		gw, token, e := s.CreateGatewayIn(ctx, p, g.Name, gatewayModel, &project.ID)
		if e != nil {
			return st, fmt.Errorf("gateway %s: %w", g.Name, e)
		}
		gwIDs[i] = gw.ID
		st.Gateways = append(st.Gateways, Gateway{ID: gw.ID, Token: token, MAC: g.MAC})
	}
	sensorIDs := make([]string, len(b.Sensors))
	coldExt := []string{}
	for i, sn := range b.Sensors {
		profile := envProfile
		if sn.Kind == "pir" {
			profile = pirProfile
		}
		d, e := s.CreateDevice(ctx, p, gwIDs[sn.Gateway], sn.Name, sn.MAC, profile)
		if e != nil {
			return st, fmt.Errorf("sensor %s: %w", sn.Name, e)
		}
		sensorIDs[i] = d.ID
		if sn.Cold {
			coldExt = append(coldExt, sn.MAC)
		}
	}
	for _, w := range b.People {
		d, e := s.CreateDevice(ctx, p, gwIDs[w.Route[0]], w.Name, w.MAC, wearProfile)
		if e != nil {
			return st, fmt.Errorf("wearable %s: %w", w.Name, e)
		}
		if e := s.Repo.SetDeviceRoaming(ctx, p, d.ID, true); e != nil {
			return st, fmt.Errorf("roaming %s: %w", w.Name, e)
		}
	}

	// The building: CreateSite adds a first floor, which becomes the ground floor.
	site, e := s.Repo.CreateSite(ctx, p, domain.Site{Name: "โรงพยาบาลเดโม · อาคารหลัก", Description: "ข้อมูลจำลอง (demo-twin)", ProjectID: &project.ID})
	if e != nil {
		return st, fmt.Errorf("site: %w", e)
	}
	st.SiteID = site.ID
	floorIDs := make([]string, len(b.Floors))
	for i, f := range b.Floors {
		if i == 0 && len(site.Floors) > 0 {
			floorIDs[0] = site.Floors[0].ID
			continue
		}
		made, e := s.Repo.CreateFloor(ctx, p, domain.Floor{SiteID: site.ID, Name: f.Name, Level: f.Level, WidthM: f.Width, DepthM: f.Depth, CeilingM: 3.2, Layout: domain.FloorLayout{}})
		if e != nil {
			return st, fmt.Errorf("floor %s: %w", f.Name, e)
		}
		floorIDs[i] = made.ID
	}
	for fi, f := range b.Floors {
		layout := domain.FloorLayout{Walls: []domain.FloorWall{}, Zones: []domain.FloorZone{}, Items: []domain.FloorItem{}}
		layout.Walls = append(layout.Walls, domain.FloorWall{ID: "outer", Points: []domain.Point{{0, 0}, {f.Width, 0}, {f.Width, f.Depth}, {0, f.Depth}}, Thickness: 0.3, Closed: true})
		for zi, z := range f.Zones {
			zone := domain.FloorZone{ID: fmt.Sprintf("z%d", zi+1), Name: z.Name, Kind: z.Kind, Color: z.Color, Comfort: z.Comfort}
			for _, pt := range z.Rect.Points() {
				zone.Points = append(zone.Points, domain.Point{pt[0], pt[1]})
			}
			if z.Gateway >= 0 {
				zone.GatewayIDs = []string{gwIDs[z.Gateway]}
			}
			layout.Zones = append(layout.Zones, zone)
			if z.Kind != "corridor" {
				layout.Walls = append(layout.Walls, domain.FloorWall{ID: fmt.Sprintf("w%d", zi+1), Points: []domain.Point{{z.Rect[0], z.Rect[1]}, {z.Rect[2], z.Rect[1]}, {z.Rect[2], z.Rect[3]}, {z.Rect[0], z.Rect[3]}}, Thickness: 0.12, Closed: true})
			}
		}
		for i, bed := range f.Beds {
			layout.Items = append(layout.Items, domain.FloorItem{ID: fmt.Sprintf("bed%d", i), Type: "bed", X: bed[0], Y: bed[1], W: 1, H: 2})
		}
		for i, desk := range f.Desks {
			layout.Items = append(layout.Items, domain.FloorItem{ID: fmt.Sprintf("desk%d", i), Type: "desk", X: desk[0], Y: desk[1], W: 1.6, H: 0.7})
		}
		for i, door := range f.Doors {
			layout.Items = append(layout.Items, domain.FloorItem{ID: fmt.Sprintf("door%d", i), Type: "door", X: door[0], Y: door[1] - 0.07, W: 0.9, H: 0.15, Rot: door[2]})
		}
		if fi == 0 {
			layout.Items = append(layout.Items, domain.FloorItem{ID: "exit1", Type: "exit", X: 0.2, Y: 11.6, W: 0.4, H: 1}, domain.FloorItem{ID: "stairs1", Type: "stairs", X: 24, Y: 11, W: 2.4, H: 1.2})
		}
		placements := []domain.FloorPlacement{}
		for gi, g := range b.Gateways {
			if g.Floor == fi {
				placements = append(placements, domain.FloorPlacement{AssetKind: "gateway", AssetID: gwIDs[gi], X: g.X, Y: g.Y, Z: 2.8})
			}
		}
		for si, sn := range b.Sensors {
			if sn.Floor == fi {
				placements = append(placements, domain.FloorPlacement{AssetKind: "device", AssetID: sensorIDs[si], X: sn.X, Y: sn.Y, Z: sn.Z})
			}
		}
		if _, _, e := s.Repo.SaveFloor(ctx, p, domain.Floor{ID: floorIDs[fi], SiteID: site.ID, Name: f.Name, Level: f.Level, WidthM: f.Width, DepthM: f.Depth, CeilingM: 3.2,
			Revision: 1, Layout: layout, Placements: placements}); e != nil {
			return st, fmt.Errorf("save floor %s: %w", f.Name, e)
		}
	}

	// The script's alerts: the cold store above 8 °C, anyone entering the server room. SOS needs no rule (built in).
	limit := simulation.TwinColdAlertAt
	rules := []domain.AlertRule{
		{Name: "ห้องเย็นอุณหภูมิเกิน 8 °C", Enabled: true, EventType: "threshold", Severity: "critical", DedupeSec: 600,
			Scope: domain.RuleScope{ExternalIDs: coldExt, Metric: "temperature", Op: ">", Value: &limit}},
		{Name: "มีคนเข้าห้องเซิร์ฟเวอร์ (หวงห้าม)", Enabled: true, EventType: "zone", Severity: "warning", DedupeSec: 300,
			Scope: domain.RuleScope{GatewayIDs: []string{gwIDs[b.RestrictedGateway]}}},
	}
	for _, r := range rules {
		r.ID, r.Channels = uuid.NewString(), []string{}
		if e := alerts.ValidateRule(r); e != nil {
			return st, fmt.Errorf("rule %s: %w", r.Name, e)
		}
		if e := s.Repo.SaveRule(ctx, p, r, true); e != nil {
			return st, fmt.Errorf("rule %s: %w", r.Name, e)
		}
	}
	return st, nil
}

// Uplinks are one step of the script, per gateway id, ready to post.
func Uplinks(st State, step int, at time.Time, o simulation.TwinOverrides) map[string][]byte {
	b := simulation.TwinDemo()
	out := map[string][]byte{}
	for gi, raw := range b.TwinPackets(step, at, o) {
		if gi < len(st.Gateways) {
			out[st.Gateways[gi].ID] = raw
		}
	}
	return out
}

// Post sends one uplink through the real HTTP ingest, as an MG3 does (HTTP Basic: gateway id and token).
func Post(ctx context.Context, client *http.Client, origin string, g Gateway, body []byte) error {
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(origin, "/")+"/ingest/gateways/"+g.ID+"/packets", bytes.NewReader(body))
	if e != nil {
		return e
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(g.ID+":"+g.Token)))
	res, e := client.Do(req)
	if e != nil {
		return e
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	if res.StatusCode != http.StatusAccepted {
		return fmt.Errorf("ingest %s: HTTP %d", g.ID, res.StatusCode)
	}
	return nil
}

// PostStep posts every gateway's uplink for a step, in gateway order.
func PostStep(ctx context.Context, client *http.Client, origin string, st State, step int, at time.Time, o simulation.TwinOverrides) error {
	up := Uplinks(st, step, at, o)
	ids := make([]string, 0, len(up))
	for id := range up {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var errs []error
	for _, id := range ids {
		for _, g := range st.Gateways {
			if g.ID == id {
				if e := Post(ctx, client, origin, g, up[id]); e != nil {
					errs = append(errs, e)
				}
			}
		}
	}
	return errors.Join(errs...)
}

// PostSOS sends the press of the script's SOS wearer: only the uplinks of the gateways that hear that wearer, so a
// trigger does not double the ingest rate of the whole building.
func PostSOS(ctx context.Context, client *http.Client, origin string, st State, step int, at time.Time) error {
	b := simulation.TwinDemo()
	mac := ""
	for _, p := range b.People {
		if p.SOS {
			mac = p.MAC
		}
	}
	var errs []error
	for id, raw := range Uplinks(st, step, at, simulation.TwinOverrides{SOS: true}) {
		if !bytes.Contains(raw, []byte(`"mac":"`+mac+`"`)) {
			continue
		}
		for _, g := range st.Gateways {
			if g.ID == id {
				errs = append(errs, Post(ctx, client, origin, g, raw))
			}
		}
	}
	return errors.Join(errs...)
}
