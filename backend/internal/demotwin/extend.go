package demotwin

import (
	"aether/backend/internal/adapters/edge"
	"aether/backend/internal/adapters/postgres"
	"aether/backend/internal/adapters/tuya"
	"aether/backend/internal/adapters/zigbee2mqtt"
	"aether/backend/internal/app"
	"aether/backend/internal/domain"
	"aether/backend/internal/security"
	"aether/backend/internal/simulation"
	"aether/backend/internal/studio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// MQTTGateway is a gateway that publishes over MQTT (the Zigbee2MQTT coordinator, the Aether Edge) and the broker
// account run connects with; the password is a secret.
type MQTTGateway struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// Names the building shows. Neutral and fictional: no real hospital's name or brand.
const (
	SiteName        = "โรงพยาบาลเอเธอร์ · อาคารหลัก"
	SiteDescription = "อาคารผู้ป่วยนอก ฉุกเฉิน หอผู้ป่วยใน และ ICU"
	ProjectName     = "โรงพยาบาลเอเธอร์"
	ProjectNote     = "อาคารหลัก 3 ชั้น"
	WorkspaceName   = "โรงพยาบาลเอเธอร์"
	OwnerName       = "ผู้ดูแลระบบ"
	DisplayName     = "จอห้องควบคุม"
)

// Extension is what extend reports back: the pairing code of a new or re-paired display (empty when the display
// is already paired), and a count of what it added.
type Extension struct {
	Added        int
	PairingCode  string
	PairingUntil string
	DisplayID    string
}

// Extend adds the extended building (simulation.TwinExtended) to an existing demo workspace: the lab MG4, the extra
// Minew tags, a Zigbee2MQTT coordinator with a device of every kind, an Aether Edge with Tuya Wi-Fi devices, their
// places on the plan, three Studio dashboards and a control-room display. It is idempotent: a second run adds only
// what is missing. save is called whenever the state gains a credential, so a failure halfway keeps them.
func Extend(ctx context.Context, s *app.Service, owner domain.Principal, st State, save func(State) error) (State, Extension, error) {
	out := Extension{}
	if owner.Role != "owner" || owner.TenantID != st.TenantID {
		return st, out, errors.New("demotwin: extend needs the demo workspace's owner")
	}
	repo, ok := s.Repo.(*postgres.Repository)
	if !ok {
		return st, out, errors.New("demotwin: extend needs the postgres repository")
	}
	b, x := simulation.TwinDemo(), simulation.TwinExtended()
	p := owner

	// Neutral names on everything a customer sees.
	project := domain.Project{ID: st.ProjectID, Name: ProjectName, Description: ProjectNote, Color: "mint"}
	if e := s.Repo.UpdateProject(ctx, p, project); e != nil {
		return st, out, fmt.Errorf("rename project: %w", e)
	}
	site, e := s.Repo.GetSite(ctx, p, st.SiteID)
	if e != nil {
		return st, out, fmt.Errorf("site: %w", e)
	}
	site.Name, site.Description = SiteName, SiteDescription
	if e := s.Repo.UpdateSite(ctx, p, site); e != nil {
		return st, out, fmt.Errorf("rename site: %w", e)
	}

	gateways, e := s.Repo.ListGateways(ctx, p)
	if e != nil {
		return st, out, e
	}
	live := map[string]bool{}
	for _, g := range gateways {
		live[g.ID] = true
	}
	newGateway := func(name, model string) (domain.Gateway, string, error) {
		return s.CreateGatewayIn(ctx, p, name, model, &st.ProjectID)
	}
	enroll := func(id string) (MQTTGateway, error) {
		password := security.RandomToken()
		hash, e := security.MQTTHash(password)
		if e != nil {
			return MQTTGateway{}, e
		}
		if e := s.Repo.EnrollMQTT(ctx, p, id, hash, false); e != nil {
			return MQTTGateway{}, e
		}
		return MQTTGateway{ID: id, Username: "gw-" + id, Password: password}, nil
	}
	if st.MG4 == nil || !live[st.MG4.ID] {
		g, token, e := newGateway(x.MG4Name, "minew-mg4")
		if e != nil {
			return st, out, fmt.Errorf("MG4: %w", e)
		}
		st.MG4 = &Gateway{ID: g.ID, Token: token, MAC: x.MG4MAC}
		out.Added++
		if e := save(st); e != nil {
			return st, out, e
		}
	}
	if st.Zigbee == nil || !live[st.Zigbee.ID] {
		g, _, e := newGateway(x.ZigbeeName, domain.Z2MGatewayModel)
		if e != nil {
			return st, out, fmt.Errorf("Zigbee coordinator: %w", e)
		}
		m, e := enroll(g.ID)
		if e != nil {
			return st, out, fmt.Errorf("Zigbee coordinator MQTT: %w", e)
		}
		st.Zigbee = &m
		out.Added++
		if e := save(st); e != nil {
			return st, out, e
		}
	}
	if st.Edge == nil || !live[st.Edge.ID] {
		g, _, e := newGateway(x.EdgeName, domain.EdgeGatewayModel)
		if e != nil {
			return st, out, fmt.Errorf("Aether Edge: %w", e)
		}
		m, e := enroll(g.ID)
		if e != nil {
			return st, out, fmt.Errorf("Aether Edge MQTT: %w", e)
		}
		st.Edge = &m
		out.Added++
		if e := save(st); e != nil {
			return st, out, e
		}
	}

	// The coordinator announces its network (as Zigbee2MQTT does on connect) so its devices can be registered, and
	// the Edge its LAN list, so the Tuya devices are known with their addresses.
	for _, m := range ZigbeeAnnounce(st.Zigbee.ID, x) {
		gateway, msg, e := zigbee2mqtt.Route(m.Topic)
		if e != nil {
			return st, out, e
		}
		if _, e := s.CaptureZ2M(ctx, st.TenantID, gateway, msg, m.Payload); e != nil {
			return st, out, fmt.Errorf("Zigbee announce %s: %w", m.Topic, e)
		}
	}
	imports := []postgres.TuyaImport{}
	for _, d := range x.Tuya {
		key, e := security.Seal(s.TuyaKeys, strings.ToLower(security.RandomToken())[:16])
		if e != nil {
			return st, out, e
		}
		imports = append(imports, postgres.TuyaImport{TuyaID: d.ID, Name: d.Name, Category: d.Category, ProductID: "aether" + d.Category, Spec: TuyaSpec(d.Category),
			LocalKeySealed: key, KeyFingerprint: fmt.Sprintf("%016x", uint64(len(d.ID))*0x9e3779b97f4a7c15)[:16]})
	}
	if _, e := repo.SaveTuyaDevices(ctx, p, st.Edge.ID, imports); e != nil {
		return st, out, fmt.Errorf("Tuya import: %w", e)
	}
	for _, m := range EdgeAnnounce(st.Edge.ID, x) {
		gateway, msg, e := edge.Route(m.Topic)
		if e != nil {
			return st, out, e
		}
		if _, e := s.CaptureEdge(ctx, st.TenantID, gateway, msg, m.Payload); e != nil {
			return st, out, fmt.Errorf("Edge announce %s: %w", m.Topic, e)
		}
	}

	// Registrations.
	devices, e := s.Repo.ListDevices(ctx, p)
	if e != nil {
		return st, out, e
	}
	registered := map[string]string{}
	for _, d := range devices {
		registered[strings.ToLower(d.ExternalID)] = d.ID
	}
	register := func(gateway, name, external, profile string, roaming bool) error {
		if _, ok := registered[strings.ToLower(external)]; ok {
			return nil
		}
		d, e := s.CreateDevice(ctx, p, gateway, name, external, profile)
		if e != nil {
			return fmt.Errorf("%s: %w", name, e)
		}
		if roaming {
			if e := s.Repo.SetDeviceRoaming(ctx, p, d.ID, true); e != nil {
				return fmt.Errorf("roaming %s: %w", name, e)
			}
		}
		registered[strings.ToLower(external)] = d.ID
		out.Added++
		return nil
	}
	for _, m := range x.Minew {
		gw := st.MG4.ID
		if m.Gateway >= 0 {
			gw = st.Gateways[m.Gateway].ID
		}
		if e := register(gw, m.Name, m.MAC, m.Profile, m.Wearable); e != nil {
			return st, out, e
		}
	}
	for _, d := range x.ZigbeeDev {
		if e := register(st.Zigbee.ID, d.Name, d.IEEE, domain.Z2MGenericProfile, false); e != nil {
			return st, out, e
		}
	}
	for _, d := range x.Tuya {
		if e := register(st.Edge.ID, d.Name, d.ID, domain.TuyaWiFiProfile, false); e != nil {
			return st, out, e
		}
	}

	// Places on the plan: the new gateways and every new device that stays put (people move, so they are not placed).
	type place struct {
		kind, id string
		at       simulation.TwinPlaced
	}
	places := []place{{"gateway", st.MG4.ID, x.MG4}, {"gateway", st.Zigbee.ID, x.Zigbee}, {"gateway", st.Edge.ID, x.Edge}}
	for _, m := range x.Minew {
		if !m.Wearable {
			places = append(places, place{"device", registered[strings.ToLower(m.MAC)], simulation.TwinPlaced{Floor: m.Floor, Zone: m.Zone, X: m.X, Y: m.Y, Z: m.Z}})
		}
	}
	for _, d := range x.ZigbeeDev {
		places = append(places, place{"device", registered[strings.ToLower(d.IEEE)], d.TwinPlaced})
	}
	for _, d := range x.Tuya {
		places = append(places, place{"device", registered[strings.ToLower(d.ID)], d.TwinPlaced})
	}
	site, e = s.Repo.GetSite(ctx, p, st.SiteID)
	if e != nil {
		return st, out, e
	}
	for fi := range b.Floors {
		var floor *domain.Floor
		for i := range site.Floors {
			if site.Floors[i].Level == b.Floors[fi].Level {
				floor = &site.Floors[i]
			}
		}
		if floor == nil {
			return st, out, fmt.Errorf("floor %d is missing from the site", b.Floors[fi].Level)
		}
		have := map[string]bool{}
		for _, pl := range floor.Placements {
			have[pl.AssetKind+pl.AssetID] = true
		}
		changed := false
		for _, pl := range places {
			if pl.at.Floor != fi || pl.id == "" || have[pl.kind+pl.id] {
				continue
			}
			floor.Placements = append(floor.Placements, domain.FloorPlacement{AssetKind: pl.kind, AssetID: pl.id, X: pl.at.X, Y: pl.at.Y, Z: pl.at.Z})
			changed = true
		}
		if changed {
			if _, _, e := s.Repo.SaveFloor(ctx, p, *floor); e != nil {
				return st, out, fmt.Errorf("place on %s: %w", floor.Name, e)
			}
		}
	}

	// Studio dashboards for the control room, then the display that rotates through everything.
	if e := dashboards(ctx, s, p, &st, registered, x); e != nil {
		return st, out, e
	}
	if e := save(st); e != nil {
		return st, out, e
	}
	if e := display(ctx, s, repo, p, &st, &out); e != nil {
		return st, out, e
	}
	st.Extended = true
	return st, out, save(st)
}

func dashboards(ctx context.Context, s *app.Service, p domain.Principal, st *State, registered map[string]string, x simulation.TwinExtras) error {
	b := simulation.TwinDemo()
	existing, e := s.Repo.StudioList(ctx, p, "dashboard")
	if e != nil {
		return e
	}
	have := map[string]string{}
	for _, it := range existing {
		have[it.Name] = it.ID
	}
	gatewayOf := map[string]string{}
	for _, sn := range b.Sensors {
		gatewayOf[sn.MAC] = st.Gateways[sn.Gateway].ID
	}
	for _, m := range x.Minew {
		if m.Gateway >= 0 {
			gatewayOf[m.MAC] = st.Gateways[m.Gateway].ID
		} else {
			gatewayOf[m.MAC] = st.MG4.ID
		}
	}
	for _, pw := range b.People {
		gatewayOf[pw.MAC] = st.Gateways[pw.Route[0]].ID
	}
	for _, d := range x.ZigbeeDev {
		gatewayOf[d.IEEE] = st.Zigbee.ID
	}
	for _, d := range x.Tuya {
		gatewayOf[d.ID] = st.Edge.ID
	}
	sensor := func(name string) string {
		for _, sn := range b.Sensors {
			if sn.Name == name {
				return sn.MAC
			}
		}
		return ""
	}
	zigbee := func(name string) string {
		for _, d := range x.ZigbeeDev {
			if d.Name == name {
				return d.IEEE
			}
		}
		return ""
	}
	tuyaID := func(name string) string {
		for _, d := range x.Tuya {
			if d.Name == name {
				return d.ID
			}
		}
		return ""
	}
	minew := func(name string) string {
		for _, m := range x.Minew {
			if m.Name == name {
				return m.MAC
			}
		}
		return ""
	}
	person := func(i int) string { return b.People[i].MAC }
	type panel struct {
		title, widget, external string
		w, h                    int
		devices                 []string // a controls panel's devices, by external id
	}
	controls := func(title string, w, h int, externals ...string) panel {
		return panel{title: title, widget: studio.ControlsWidget, w: w, h: h, devices: externals}
	}
	boards := []struct {
		name   string
		panels []panel
	}{
		{"ห้องควบคุม · สภาพแวดล้อม", []panel{
			{"ห้องเย็นเก็บยาและวัคซีน", "official-environment-v1", sensor("อุณหภูมิ ห้องเย็นเก็บยาและวัคซีน #1"), 2, 1, nil},
			{"แนวโน้มอุณหภูมิ ICU A", "official-temperature-v1", sensor("อุณหภูมิ ICU A #1"), 2, 1, nil},
			{"คุณภาพอากาศ หอผู้ป่วย 1", "official-metrics-v1", zigbee("คุณภาพอากาศ หอผู้ป่วย 1"), 2, 1, nil},
			{"คุณภาพอากาศ ห้องฉุกเฉิน", "official-metrics-v1", zigbee("คุณภาพอากาศ ห้องฉุกเฉิน"), 2, 1, nil},
			{"คุณภาพอากาศ หอผู้ป่วย 3", "official-metrics-v1", tuyaID("คุณภาพอากาศ หอผู้ป่วย 3 (Wi‑Fi)"), 1, 1, nil},
			{"เครื่องฟอกอากาศ ICU B", "official-metrics-v1", zigbee("เครื่องฟอกอากาศ ICU B"), 1, 1, nil},
			{"วาล์วอุณหภูมิ ICU A", "official-metrics-v1", zigbee("วาล์วอุณหภูมิ ICU A"), 1, 1, nil},
			{"แอร์ ICU B", "official-metrics-v1", tuyaID("แอร์ ICU B"), 1, 1, nil},
			{"ตู้บ่มเชื้อ ห้องแล็บ", "official-environment-v1", minew("อุณหภูมิตู้บ่มเชื้อ ห้องแล็บ"), 2, 1, nil},
			{"ห้องเซิร์ฟเวอร์", "official-metrics-v1", zigbee("อุณหภูมิ ห้องเซิร์ฟเวอร์"), 2, 1, nil},
		}},
		{"ห้องควบคุม · พลังงานและความปลอดภัย", []panel{
			{"ตู้แช่วัคซีน", "official-metrics-v1", tuyaID("ตู้แช่วัคซีน (Wi‑Fi)"), 1, 1, nil},
			{"ตู้เย็นเก็บเลือด", "official-metrics-v1", zigbee("ตู้เย็นเก็บเลือด"), 1, 1, nil},
			{"เครื่องให้ยา ICU A", "official-metrics-v1", zigbee("เครื่องให้ยา ICU A"), 1, 1, nil},
			{"เครื่องชงกาแฟ", "official-metrics-v1", zigbee("เครื่องชงกาแฟ เคาน์เตอร์พยาบาล"), 1, 1, nil},
			{"ประตูห้องยา", "official-metrics-v1", zigbee("ประตูห้องยา"), 1, 1, nil},
			{"ประตูห้องเย็น", "official-metrics-v1", zigbee("ประตูห้องเย็น"), 1, 1, nil},
			{"ประตูห้องเซิร์ฟเวอร์", "official-metrics-v1", zigbee("ประตูห้องเซิร์ฟเวอร์"), 1, 1, nil},
			{"ทางเดินชั้น 1", "official-metrics-v1", zigbee("PIR ทางเดินชั้น 1"), 1, 1, nil},
			{"ควัน ห้องแล็บ", "official-metrics-v1", zigbee("ตรวจจับควันห้องแล็บ"), 1, 1, nil},
			{"แก๊ส ห้องแล็บ", "official-metrics-v1", zigbee("ตรวจจับแก๊สห้องแล็บ"), 1, 1, nil},
			{"CO โถงต้อนรับ", "official-metrics-v1", zigbee("ตรวจจับ CO โถงต้อนรับ"), 1, 1, nil},
			{"น้ำรั่ว ห้องแล็บ", "official-metrics-v1", zigbee("น้ำรั่วใต้อ่างห้องแล็บ"), 1, 1, nil},
			{"ไฟทางเดินชั้น 2", "official-metrics-v1", zigbee("สวิตช์ไฟทางเดินชั้น 2"), 1, 1, nil},
			{"ไฟ ICU B (Wi‑Fi)", "official-metrics-v1", tuyaID("สวิตช์ไฟหน้าห้อง ICU B (Wi‑Fi)"), 1, 1, nil},
			{"ตู้ยาควบคุมพิเศษ", "official-tamper-v1", minew("ตู้ยาควบคุมพิเศษ"), 1, 1, nil},
			{"ปั๊มให้ยา ICU A", "official-motion-v1", minew("ปั๊มให้ยา ICU A"), 1, 1, nil},
		}},
		{"ห้องควบคุม · ผู้คนและเหตุการณ์", []panel{
			{"การแจ้งเตือนที่เปิดอยู่", "official-alerts-v1", sensor("อุณหภูมิ หอผู้ป่วย 3 #1"), 2, 2, nil},
			{"เหตุการณ์ล่าสุด", "official-events-v1", sensor("อุณหภูมิ หอผู้ป่วย 3 #2"), 2, 2, nil},
			{"ปุ่มฉุกเฉิน หอผู้ป่วย 3", "official-button-v1", person(30), 1, 1, nil},
			{"ตำแหน่ง " + b.People[0].Name, "official-presence-v1", person(0), 1, 1, nil},
			{"ตำแหน่ง " + b.People[16].Name, "official-presence-v1", person(16), 1, 1, nil},
			{"ตำแหน่ง รปภ. สมหมาย", "official-presence-v1", minew("รปภ. สมหมาย"), 1, 1, nil},
		}},
		{"ห้องควบคุม · สวิตช์และปลั๊ก", []panel{
			controls("ICU", 2, 2, zigbee("สวิตช์ไฟ ICU A"), zigbee("สวิตช์ไฟ ICU B"), tuyaID("สวิตช์ไฟหน้าห้อง ICU B (Wi‑Fi)"), zigbee("เครื่องให้ยา ICU A"), zigbee("เครื่องให้ยา ICU B"), tuyaID("แอร์ ICU B")),
			controls("หอผู้ป่วย", 2, 2, zigbee("สวิตช์ไฟหอผู้ป่วย 1"), zigbee("สวิตช์ไฟหอผู้ป่วย 2"), zigbee("สวิตช์ไฟหอผู้ป่วย 3"), zigbee("ทีวีห้องพักผู้ป่วย 2"), tuyaID("เครื่องฟอกอากาศ หอผู้ป่วย 3")),
			controls("โถง ทางเดิน และห้องฉุกเฉิน", 2, 2, zigbee("สวิตช์ไฟโถงต้อนรับ"), zigbee("สวิตช์ไฟทางเดินชั้น 1"), zigbee("สวิตช์ไฟทางเดินชั้น 2"), zigbee("ม่านโถงต้อนรับ"), zigbee("โคมไฟโถงต้อนรับ"), zigbee("เครื่องติดตามสัญญาณชีพ ห้องฉุกเฉิน")),
			controls("ห้องยา ห้องเย็น ห้องแล็บ", 2, 2, zigbee("สวิตช์ไฟห้องยา"), zigbee("สวิตช์ไฟห้องเย็น"), zigbee("ตู้เย็นเก็บเลือด"), tuyaID("ตู้แช่วัคซีน (Wi‑Fi)"), tuyaID("ไฟห้องแล็บ"), zigbee("สวิตช์ไฟห้องเซิร์ฟเวอร์")),
		}},
	}
	st.Dashboards = st.Dashboards[:0]
	for _, board := range boards {
		if id, ok := have[board.name]; ok {
			st.Dashboards = append(st.Dashboards, id)
			continue
		}
		panels := []studio.Panel{}
		for i, pn := range board.panels {
			if pn.widget == studio.ControlsWidget {
				ids := []string{}
				for _, ext := range pn.devices {
					id, ok := registered[strings.ToLower(ext)]
					if ext == "" || !ok {
						return fmt.Errorf("dashboard %s: panel %s names a device that is not registered", board.name, pn.title)
					}
					ids = append(ids, id)
				}
				panels = append(panels, studio.Panel{ID: fmt.Sprintf("p%d", i+1), Title: pn.title, WidgetID: pn.widget, Devices: ids, Width: pn.w, Height: pn.h})
				continue
			}
			if pn.external == "" {
				return fmt.Errorf("dashboard %s: panel %s has no device", board.name, pn.title)
			}
			panels = append(panels, studio.Panel{ID: fmt.Sprintf("p%d", i+1), Title: pn.title, WidgetID: pn.widget, GatewayID: gatewayOf[pn.external], ExternalID: pn.external, Width: pn.w, Height: pn.h})
		}
		def, _ := json.Marshal(studio.Definition{Panels: panels, ProjectID: st.ProjectID})
		item := studio.Item{ID: uuid.NewString(), Kind: "dashboard", Name: board.name, Brand: "Aether", Model: "Control room", Version: 1, Visibility: "private", Definition: def, Revision: 1}
		if e := studio.Validate(item); e != nil {
			return fmt.Errorf("dashboard %s: %w", board.name, e)
		}
		if e := s.Repo.StudioCreate(ctx, p, item); e != nil {
			return fmt.Errorf("dashboard %s: %w", board.name, e)
		}
		st.Dashboards = append(st.Dashboards, item.ID)
	}
	return nil
}

// Playlist is the control-room display's rotation.
func Playlist(st State) []domain.DisplayView {
	views := []domain.DisplayView{{Kind: "overview", Seconds: 30}, {Kind: "twin", Seconds: 45, Ref: st.SiteID}, {Kind: "alerts", Seconds: 20}}
	for _, id := range st.Dashboards {
		views = append(views, domain.DisplayView{Kind: "studio", Seconds: 30, Ref: id})
	}
	return append(views, domain.DisplayView{Kind: "floorplan", Seconds: 40, Ref: st.SiteID}, domain.DisplayView{Kind: "presence", Seconds: 25}, domain.DisplayView{Kind: "devices", Seconds: 25})
}

func display(ctx context.Context, s *app.Service, repo *postgres.Repository, p domain.Principal, st *State, out *Extension) error {
	settings := domain.DisplaySettings{Name: DisplayName, ProjectIDs: []string{}, Playlist: Playlist(*st), ShowNames: true, AllowAck: true}
	if st.DisplayID != "" {
		list, e := repo.ListDisplays(ctx, p)
		if e != nil {
			return e
		}
		for _, d := range list {
			if d.ID != st.DisplayID || d.RevokedAt != nil {
				continue
			}
			if _, e := s.UpdateDisplay(ctx, p, d.ID, settings); e != nil {
				return fmt.Errorf("display: %w", e)
			}
			out.DisplayID = d.ID
			if d.Paired {
				return nil
			}
			pairing, e := s.RepairDisplay(ctx, p, d.ID)
			if e != nil {
				return fmt.Errorf("display pairing: %w", e)
			}
			out.PairingCode, out.PairingUntil = pairing.Code, pairing.ExpiresAt.Local().Format("15:04")
			return nil
		}
	}
	pairing, e := s.CreateDisplay(ctx, p, settings)
	if e != nil {
		return fmt.Errorf("display: %w", e)
	}
	st.DisplayID, out.DisplayID = pairing.Display.ID, pairing.Display.ID
	out.PairingCode, out.PairingUntil = pairing.Code, pairing.ExpiresAt.Local().Format("15:04")
	return nil
}

// Pair issues a new pairing code for the control-room display (the old TV, if any, is signed out).
func Pair(ctx context.Context, s *app.Service, owner domain.Principal, st State) (domain.DisplayPairing, error) {
	if st.DisplayID == "" {
		return domain.DisplayPairing{}, errors.New("demotwin: no display yet (run extend first)")
	}
	return s.RepairDisplay(ctx, owner, st.DisplayID)
}

// TuyaSpec is the data point specification of each demo Tuya category, as a Tuya import would store it.
func TuyaSpec(category string) []tuya.DP {
	i := func(v int64) *int64 { return &v }
	switch category {
	case "cz":
		return []tuya.DP{{ID: 1, Code: "switch_1", Type: "bool", Access: "rw"},
			{ID: 17, Code: "add_ele", Type: "value", Access: "ro", Min: i(0), Max: i(5000000), Scale: 3, Unit: "kwh"},
			{ID: 18, Code: "cur_current", Type: "value", Access: "ro", Min: i(0), Max: i(30000), Unit: "mA"},
			{ID: 19, Code: "cur_power", Type: "value", Access: "ro", Min: i(0), Max: i(80000), Scale: 1, Unit: "W"},
			{ID: 20, Code: "cur_voltage", Type: "value", Access: "ro", Min: i(0), Max: i(5000), Scale: 1, Unit: "V"}}
	case "kg":
		return []tuya.DP{{ID: 1, Code: "switch_1", Type: "bool", Access: "rw"}, {ID: 2, Code: "switch_2", Type: "bool", Access: "rw"}, {ID: 3, Code: "switch_3", Type: "bool", Access: "rw"}}
	case "dj":
		return []tuya.DP{{ID: 20, Code: "switch_led", Type: "bool", Access: "rw"},
			{ID: 21, Code: "work_mode", Type: "enum", Access: "rw", Range: []string{"white", "colour", "scene", "music"}},
			{ID: 22, Code: "bright_value_v2", Type: "value", Access: "rw", Min: i(10), Max: i(1000)},
			{ID: 23, Code: "temp_value_v2", Type: "value", Access: "rw", Min: i(0), Max: i(1000)}}
	case "wk":
		return []tuya.DP{{ID: 1, Code: "switch", Type: "bool", Access: "rw"},
			{ID: 2, Code: "temp_set", Type: "value", Access: "rw", Min: i(160), Max: i(300), Step: i(5), Scale: 1, Unit: "℃"},
			{ID: 3, Code: "temp_current", Type: "value", Access: "ro", Min: i(-200), Max: i(600), Scale: 1, Unit: "℃"},
			{ID: 4, Code: "mode", Type: "enum", Access: "rw", Range: []string{"cold", "hot", "wind", "auto"}}}
	case "hjjcy":
		return []tuya.DP{{ID: 2, Code: "pm25_value", Type: "value", Access: "ro", Min: i(0), Max: i(999), Unit: "ug/m3"},
			{ID: 18, Code: "va_temperature", Type: "value", Access: "ro", Min: i(-200), Max: i(600), Scale: 1, Unit: "℃"},
			{ID: 19, Code: "humidity_value", Type: "value", Access: "ro", Min: i(0), Max: i(100), Unit: "%"},
			{ID: 20, Code: "ch2o_value", Type: "value", Access: "ro", Min: i(0), Max: i(1000), Unit: "ug/m3"},
			{ID: 21, Code: "voc_value", Type: "value", Access: "ro", Min: i(0), Max: i(2000), Unit: "ppb"},
			{ID: 22, Code: "co2_value", Type: "value", Access: "ro", Min: i(0), Max: i(5000), Unit: "ppm"}}
	}
	return nil
}
