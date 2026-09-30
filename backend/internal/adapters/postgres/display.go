package postgres

import (
	"context"
	"encoding/json"
	"net"
	"sort"
	"strings"
	"time"

	"aether/backend/internal/domain"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Display links (migration 00044, docs/platform/display.md). Members manage displays; a display reads through
// transactions marked with domain.WithDisplay (no user, the display's project scope, READ ONLY).

const displayColumns = `id::text AS id,name,project_ids::text AS project_ids,playlist::text AS playlist,show_names,allow_ack,
 pairing_expires_at,paired_at,last_seen_at,host(last_ip) AS last_ip,revoked_at,created_at`

type displayRow struct {
	ID, Name, ProjectIDs, Playlist string
	ShowNames, AllowAck            bool
	PairingExpiresAt, PairedAt     *time.Time
	LastSeenAt                     *time.Time
	LastIP                         *string
	RevokedAt                      *time.Time
	CreatedAt                      time.Time
}

// uuidList parses a Postgres uuid[] text ("{a,b}").
func uuidList(s string) []string {
	s = strings.Trim(s, "{}")
	if s == "" {
		return []string{}
	}
	return strings.Split(s, ",")
}

func (r displayRow) display() domain.Display {
	d := domain.Display{ID: r.ID, Name: r.Name, ProjectIDs: uuidList(r.ProjectIDs), ShowNames: r.ShowNames, AllowAck: r.AllowAck,
		PairingExpiresAt: r.PairingExpiresAt, PairedAt: r.PairedAt, LastSeenAt: r.LastSeenAt, LastIP: r.LastIP, RevokedAt: r.RevokedAt, CreatedAt: r.CreatedAt}
	d.Paired = r.PairedAt != nil && r.RevokedAt == nil && r.PairingExpiresAt == nil
	d.Playlist = []domain.DisplayView{}
	_ = json.Unmarshal([]byte(r.Playlist), &d.Playlist)
	if d.PairingExpiresAt != nil && !d.PairingExpiresAt.After(time.Now()) {
		d.PairingExpiresAt = nil // an expired code is as good as none
	}
	return d
}

// manageDisplays admits owners and admins who see every project (a display may show several projects; the
// table's policy says the same).
func manageDisplays(tx *gorm.DB, p domain.Principal) error {
	if !p.CanManageDevices() {
		return domain.ErrForbidden
	}
	all, e := scopeAll(tx)
	if e != nil {
		return e
	}
	if !all {
		return domain.ErrForbidden
	}
	return nil
}

// displayProjects checks every project of a display exists in this workspace and is active.
func displayProjects(tx *gorm.DB, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	var n int64
	if e := tx.Raw(`SELECT count(DISTINCT id) FROM core.projects WHERE id = ANY(?::uuid[]) AND archived_at IS NULL`, pgArray(ids)).Scan(&n).Error; e != nil {
		return e
	}
	if int(n) != len(ids) {
		return domain.Because(domain.ErrInvalid, "unknown_project")
	}
	return nil
}

func displaySignal(tx *gorm.DB, tenant, id string) error { return signal(tx, tenant, "display", id) }

// displaySwitches applies the owner-only rule to the two switches that widen what a TV does (show wearer names,
// acknowledge from the screen): only an owner may turn either on; an admin may turn them off. Each change is its own
// audit action.
func displaySwitches(tx *gorm.DB, p domain.Principal, id string, was, now domain.DisplaySettings) error {
	if (now.ShowNames && !was.ShowNames) || (now.AllowAck && !was.AllowAck) {
		if role, e := roleOf(tx, p.TenantID, p.UserID); e != nil || role != "owner" {
			return domain.Because(domain.ErrForbidden, "owner_only")
		}
	}
	for _, c := range []struct {
		was, now bool
		on, off  string
	}{{was.ShowNames, now.ShowNames, "display.names_on", "display.names_off"}, {was.AllowAck, now.AllowAck, "display.ack_on", "display.ack_off"}} {
		switch {
		case c.now && !c.was:
			if e := audit(tx, p, c.on, id); e != nil {
				return e
			}
		case c.was && !c.now:
			if e := audit(tx, p, c.off, id); e != nil {
				return e
			}
		}
	}
	return nil
}

func (r *Repository) ListDisplays(ctx context.Context, p domain.Principal) ([]domain.Display, error) {
	out := []domain.Display{}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if e := manageDisplays(tx, p); e != nil {
			return e
		}
		var rows []displayRow
		if e := tx.Raw(`SELECT ` + displayColumns + ` FROM core.displays WHERE revoked_at IS NULL ORDER BY created_at,id LIMIT 100`).Scan(&rows).Error; e != nil {
			return e
		}
		for _, row := range rows {
			out = append(out, row.display())
		}
		return nil
	})
	return out, e
}

func getDisplay(tx *gorm.DB, id string) (domain.Display, error) {
	var rows []displayRow
	if e := tx.Raw(`SELECT `+displayColumns+` FROM core.displays WHERE id=? AND revoked_at IS NULL`, id).Scan(&rows).Error; e != nil {
		return domain.Display{}, e
	}
	if len(rows) != 1 {
		return domain.Display{}, domain.ErrNotFound
	}
	return rows[0].display(), nil
}

// CreateDisplay stores a new display with its first pairing code (digest only).
func (r *Repository) CreateDisplay(ctx context.Context, p domain.Principal, id string, s domain.DisplaySettings, pairingHash string, expires time.Time) (domain.Display, error) {
	var out domain.Display
	playlist, _ := json.Marshal(s.Playlist)
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if e := manageDisplays(tx, p); e != nil {
			return e
		}
		if e := tx.Exec(`SELECT pg_advisory_xact_lock(hashtextextended(?,11))`, p.TenantID).Error; e != nil {
			return e
		}
		var n int64
		if e := tx.Raw(`SELECT count(*) FROM core.displays WHERE revoked_at IS NULL`).Scan(&n).Error; e != nil {
			return e
		}
		if n >= domain.MaxDisplays {
			return domain.Because(domain.ErrConflict, "display_limit")
		}
		if e := displayProjects(tx, s.ProjectIDs); e != nil {
			return e
		}
		if e := displaySwitches(tx, p, id, domain.DisplaySettings{}, s); e != nil {
			return e
		}
		if e := tx.Exec(`INSERT INTO core.displays(tenant_id,id,name,project_ids,playlist,show_names,allow_ack,pairing_hash,pairing_expires_at,created_by)
      VALUES(core.tenant_id(),?,?,?::uuid[],?::jsonb,?,?,?,?,identity.user_id())`, id, s.Name, pgArray(s.ProjectIDs), string(playlist), s.ShowNames, s.AllowAck, pairingHash, expires).Error; e != nil {
			return e
		}
		if e := audit(tx, p, "display.created", id); e != nil {
			return e
		}
		var e error
		out, e = getDisplay(tx, id)
		return e
	})
	return out, e
}

// UpdateDisplay changes a display's name, scope, playlist and switches. The TV picks them up on its next request.
func (r *Repository) UpdateDisplay(ctx context.Context, p domain.Principal, id string, s domain.DisplaySettings) (domain.Display, error) {
	var out domain.Display
	playlist, _ := json.Marshal(s.Playlist)
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if e := manageDisplays(tx, p); e != nil {
			return e
		}
		if e := displayProjects(tx, s.ProjectIDs); e != nil {
			return e
		}
		var was []struct{ ShowNames, AllowAck bool }
		if e := tx.Raw(`SELECT show_names,allow_ack FROM core.displays WHERE id=? AND revoked_at IS NULL FOR UPDATE`, id).Scan(&was).Error; e != nil {
			return e
		}
		if len(was) != 1 {
			return domain.ErrNotFound
		}
		if e := displaySwitches(tx, p, id, domain.DisplaySettings{ShowNames: was[0].ShowNames, AllowAck: was[0].AllowAck}, s); e != nil {
			return e
		}
		res := tx.Exec(`UPDATE core.displays SET name=?,project_ids=?::uuid[],playlist=?::jsonb,show_names=?,allow_ack=?,updated_at=now() WHERE id=? AND revoked_at IS NULL`,
			s.Name, pgArray(s.ProjectIDs), string(playlist), s.ShowNames, s.AllowAck, id)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return domain.ErrNotFound
		}
		if e := displaySignal(tx, p.TenantID, id); e != nil {
			return e
		}
		if e := audit(tx, p, "display.updated", id); e != nil {
			return e
		}
		var e error
		out, e = getDisplay(tx, id)
		return e
	})
	return out, e
}

// RepairDisplay issues a new pairing code and ends the display's current token at once (a lost or replaced TV).
func (r *Repository) RepairDisplay(ctx context.Context, p domain.Principal, id, pairingHash string, expires time.Time) (domain.Display, error) {
	var out domain.Display
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if e := manageDisplays(tx, p); e != nil {
			return e
		}
		res := tx.Exec(`UPDATE core.displays SET pairing_hash=?,pairing_expires_at=?,token_hash=NULL,paired_at=NULL,updated_at=now() WHERE id=? AND revoked_at IS NULL`, pairingHash, expires, id)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return domain.ErrNotFound
		}
		if e := displaySignal(tx, p.TenantID, id); e != nil {
			return e
		}
		if e := audit(tx, p, "display.repaired", id); e != nil {
			return e
		}
		var e error
		out, e = getDisplay(tx, id)
		return e
	})
	return out, e
}

// RevokeDisplay ends a display for good: its token and any pairing code stop working, and its open stream closes.
func (r *Repository) RevokeDisplay(ctx context.Context, p domain.Principal, id string) error {
	return r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if e := manageDisplays(tx, p); e != nil {
			return e
		}
		res := tx.Exec(`UPDATE core.displays SET revoked_at=now(),token_hash=NULL,pairing_hash=NULL,pairing_expires_at=NULL,updated_at=now() WHERE id=? AND revoked_at IS NULL`, id)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return domain.ErrNotFound
		}
		if e := displaySignal(tx, p.TenantID, id); e != nil {
			return e
		}
		return audit(tx, p, "display.revoked", id)
	})
}

// PairDisplay turns a valid pairing code into the display's token (both as digests). Unknown, expired or used
// codes are all ErrUnauthorized.
func (r *Repository) PairDisplay(ctx context.Context, codeHash, tokenHash string) (string, string, error) {
	var row struct{ DisplayID, TenantID string }
	e := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Raw(`SELECT display_id::text AS display_id,tenant_id::text AS tenant_id FROM core.pair_display(?,?)`, codeHash, tokenHash).Scan(&row)
		if res.Error != nil {
			return res.Error
		}
		if row.DisplayID == "" {
			return domain.ErrUnauthorized
		}
		// The workspace is known now: the audit row is written in it, with the display as the actor.
		if e := tx.Exec(`SELECT set_config('app.tenant_id',?,true),set_config('app.user_id','',true)`, row.TenantID).Error; e != nil {
			return e
		}
		if e := tx.Exec(`INSERT INTO core.audit_logs(id,tenant_id,actor_id,action,target_id) VALUES(?,?,?,?,?)`, uuid.NewString(), row.TenantID, row.DisplayID, "display.paired", row.DisplayID).Error; e != nil {
			return e
		}
		return signal(tx, row.TenantID, "display", row.DisplayID)
	})
	return row.DisplayID, row.TenantID, e
}

// DisplaySession authenticates a display token (digest) and returns the display's current settings.
func (r *Repository) DisplaySession(ctx context.Context, tokenHash, ip string) (domain.DisplaySession, error) {
	var rows []struct {
		DisplayID, TenantID, Name, ProjectIDs, Playlist, TenantName string
		ShowNames, AllowAck                                         bool
	}
	if parsed := net.ParseIP(ip); parsed != nil {
		ip = parsed.String()
	} else {
		ip = ""
	}
	e := r.db.WithContext(ctx).Raw(`SELECT display_id::text AS display_id,tenant_id::text AS tenant_id,name,project_ids::text AS project_ids,playlist::text AS playlist,show_names,allow_ack,tenant_name
    FROM core.display_session(?,NULLIF(?,'')::inet)`, tokenHash, ip).Scan(&rows).Error
	if e != nil {
		return domain.DisplaySession{}, e
	}
	if len(rows) != 1 {
		return domain.DisplaySession{}, domain.ErrUnauthorized
	}
	s := domain.DisplaySession{ID: rows[0].DisplayID, TenantID: rows[0].TenantID, TenantName: rows[0].TenantName, Name: rows[0].Name, ProjectIDs: uuidList(rows[0].ProjectIDs), ShowNames: rows[0].ShowNames, AllowAck: rows[0].AllowAck}
	s.Playlist = []domain.DisplayView{}
	_ = json.Unmarshal([]byte(rows[0].Playlist), &s.Playlist)
	return s, nil
}

const (
	gatewayOnlineWindow = 2 * time.Minute
	deviceOnlineWindow  = 10 * time.Minute
)

var readingKeys = []string{"temperature", "humidity", "battery", "rssi"}
var metricKeys = []string{"tamper", "leak", "motion", "occupancy", "contact", "door", "smoke", "gas", "co", "carbon_monoxide", "vibration", "co2", "pm25", "voc", "power", "energy",
	"illuminance", "sw1", "sw2", "sw3", "sw4", "position", "brightness", "local_temperature"}

// readingSummary keeps the handful of numbers a TV draws from a stored reading.
func readingSummary(raw string) (map[string]float64, string) {
	out := map[string]float64{}
	var r map[string]any
	if raw == "" || json.Unmarshal([]byte(raw), &r) != nil {
		return out, ""
	}
	kind, _ := r["kind"].(string)
	for _, k := range readingKeys {
		if v, ok := r[k].(float64); ok {
			out[k] = v
		}
	}
	if kind != "" && kind != "environment" {
		delete(out, "temperature")
		delete(out, "humidity")
	}
	if m, ok := r["metrics"].(map[string]any); ok {
		for _, k := range metricKeys {
			if v, ok := m[k].(float64); ok {
				out[k] = v
			}
		}
		if v, ok := m["temperature"].(float64); ok {
			out["temperature"] = v
		}
		if v, ok := m["humidity"].(float64); ok {
			out["humidity"] = v
		}
		if v, ok := m["battery"].(float64); ok {
			out["battery"] = v
		}
	}
	return out, kind
}

// impersonalTags is the set of tags a display may name (core.display_impersonal_tags): a registered tag none of whose
// live registrations is roaming or worn. Anything not in it is personal, so an unknown tag fails closed.
func impersonalTags(tx *gorm.DB) (map[string]bool, error) {
	var ids []string
	if e := tx.Raw(`SELECT t FROM core.display_impersonal_tags(?::text[]) t`, pgArray(domain.WornProfileIDs())).Scan(&ids).Error; e != nil {
		return nil, e
	}
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

// DisplayImpersonal is impersonalTags for callers outside a board read (the studio view of a display).
func (r *Repository) DisplayImpersonal(ctx context.Context, s domain.DisplaySession) (map[string]bool, error) {
	if d, ok := domain.DisplayFrom(ctx); !ok || d.ID != s.ID {
		return nil, domain.ErrForbidden
	}
	var out map[string]bool
	e := r.tx(ctx, "", s.TenantID, func(tx *gorm.DB) error {
		var e error
		out, e = impersonalTags(tx)
		return e
	})
	return out, e
}

// DisplayBoard reads what the TV's views draw, as the display (the context must carry domain.WithDisplay). Names
// of worn tags are replaced unless the display may show them; namesShown says whether any were included.
func (r *Repository) DisplayBoard(ctx context.Context, s domain.DisplaySession) (domain.DisplayBoard, bool, error) {
	if d, ok := domain.DisplayFrom(ctx); !ok || d.ID != s.ID {
		return domain.DisplayBoard{}, false, domain.ErrForbidden
	}
	out := domain.DisplayBoard{Projects: []domain.DisplayProject{}, Gateways: []domain.DisplayGateway{}, Devices: []domain.DisplayDevice{}, Alerts: []domain.DisplayAlert{}, Recent: []domain.DisplayAlert{}, Presence: []domain.DisplayPresence{}}
	namesShown := false
	e := r.tx(ctx, "", s.TenantID, func(tx *gorm.DB) error {
		if e := tx.Raw(`SELECT now()`).Scan(&out.ServerTime).Error; e != nil {
			return e
		}
		now := out.ServerTime
		if e := tx.Raw(`SELECT id::text AS id,name,color FROM core.projects WHERE archived_at IS NULL ORDER BY created_at,id LIMIT 100`).Scan(&out.Projects).Error; e != nil {
			return e
		}
		if e := tx.Raw(`SELECT g.id::text AS id,g.name,g.model,g.project_id::text AS project_id,
       greatest((SELECT max(s.last_seen) FROM core.sensor_streams s WHERE s.tenant_id=g.tenant_id AND s.gateway_id=g.id),
                (SELECT max(p.received_at) FROM core.gateway_packets p WHERE p.tenant_id=g.tenant_id AND p.gateway_id=g.id)) AS last_seen
      FROM core.gateways g WHERE g.revoked_at IS NULL ORDER BY g.name,g.id LIMIT 200`).Scan(&out.Gateways).Error; e != nil {
			return e
		}
		gatewayName := map[string]string{}
		for i := range out.Gateways {
			g := &out.Gateways[i]
			g.Online = g.LastSeen != nil && now.Sub(*g.LastSeen) <= gatewayOnlineWindow
			gatewayName[g.ID] = g.Name
			out.Counts.Gateways++
			if g.Online {
				out.Counts.GatewaysUp++
			}
		}
		var devices []struct {
			ID, Name, ExternalID, ProfileID, GatewayID string
			Roaming                                    bool
			ZoneGatewayID                              *string
			LastSeen                                   *time.Time
			Reading                                    *string
		}
		if e := tx.Raw(`SELECT d.id::text AS id,d.name,lower(d.external_id) AS external_id,d.profile_id,d.gateway_id::text AS gateway_id,d.roaming,
       ps.gateway_id::text AS zone_gateway_id,st.last_seen,smp.reading
      FROM core.devices d
      LEFT JOIN core.presence_state ps ON ps.tenant_id=d.tenant_id AND ps.external_id=lower(d.external_id)
      LEFT JOIN LATERAL (SELECT max(s.last_seen) AS last_seen FROM core.sensor_streams s WHERE s.tenant_id=d.tenant_id AND s.external_id=lower(d.external_id)) st ON true
      LEFT JOIN LATERAL (SELECT x.reading::text AS reading FROM core.sensor_samples x
        WHERE x.tenant_id=d.tenant_id AND x.external_id=lower(d.external_id) AND x.received_at > now()-interval '2 days' AND x.received_at <= now()
        ORDER BY x.received_at DESC,x.event_key LIMIT 1) smp ON true
      WHERE d.removed_at IS NULL ORDER BY d.name,d.id LIMIT 500`).Scan(&devices).Error; e != nil {
			return e
		}
		impersonal, e := impersonalTags(tx)
		if e != nil {
			return e
		}
		// Without names a TV gets no MAC either: a per-process keyed tag that still matches alerts to devices.
		tag := func(ext string) string {
			if s.ShowNames {
				return ext
			}
			return domain.DisplayTag(s.TenantID, ext)
		}
		zones := map[string]*domain.DisplayPresence{}
		for _, d := range devices {
			profile := domain.DeviceProfileByID(d.ProfileID)
			worn := d.Roaming || (profile != nil && (profile.Wearable || profile.Button))
			name := d.Name
			if !impersonal[d.ExternalID] {
				if !s.ShowNames {
					name = domain.DisplayWearerLabel(d.ExternalID)
				} else {
					namesShown = true
				}
			}
			dev := domain.DisplayDevice{ID: d.ID, Name: name, ExternalID: tag(d.ExternalID), ProfileID: d.ProfileID, GatewayID: d.GatewayID, Wearable: worn, ZoneGatewayID: d.ZoneGatewayID, LastSeen: d.LastSeen}
			dev.Online = d.LastSeen != nil && now.Sub(*d.LastSeen) <= deviceOnlineWindow
			if d.Reading != nil {
				dev.Reading, dev.Kind = readingSummary(*d.Reading)
			} else {
				dev.Reading = map[string]float64{}
			}
			out.Devices = append(out.Devices, dev)
			out.Counts.Devices++
			if dev.Online {
				out.Counts.DevicesUp++
			}
			if b, ok := dev.Reading["battery"]; ok && b > 0 && b <= 20 {
				out.Counts.LowBattery++
			}
			// Worn tags are counted in the zone of the gateway that holds them now (decided at ingest).
			if worn && d.ZoneGatewayID != nil && d.LastSeen != nil && now.Sub(*d.LastSeen) <= deviceOnlineWindow {
				z := zones[*d.ZoneGatewayID]
				if z == nil {
					z = &domain.DisplayPresence{GatewayID: *d.ZoneGatewayID, GatewayName: gatewayName[*d.ZoneGatewayID]}
					zones[*d.ZoneGatewayID] = z
				}
				z.Count++
				if s.ShowNames {
					z.Names = append(z.Names, name)
				}
			}
		}
		for _, z := range zones {
			sort.Strings(z.Names)
			out.Presence = append(out.Presence, *z)
		}
		sort.Slice(out.Presence, func(i, j int) bool { return out.Presence[i].GatewayName < out.Presence[j].GatewayName })

		var alerts []struct {
			ID, GatewayID, ExternalID, DeviceName, EventType, Severity, Title, Status string
			OpenedAt                                                                  time.Time
			AckedAt                                                                   *time.Time
		}
		if e := tx.Raw(`SELECT a.id::text AS id,a.gateway_id::text AS gateway_id,lower(a.external_id) AS external_id,a.device_name,a.event_type,a.severity,a.title,a.status,a.opened_at,a.acked_at
      FROM core.alerts a WHERE a.status<>'resolved' ORDER BY (a.status='open') DESC,a.opened_at DESC,a.id DESC LIMIT 30`).Scan(&alerts).Error; e != nil {
			return e
		}
		open := len(alerts)
		// What was handled today, so a calm wall still shows the day's work (never a takeover).
		resolved := alerts[:0:0]
		if e := tx.Raw(`SELECT a.id::text AS id,a.gateway_id::text AS gateway_id,lower(a.external_id) AS external_id,a.device_name,a.event_type,a.severity,a.title,a.status,a.opened_at,a.acked_at
      FROM core.alerts a WHERE a.status='resolved' AND a.opened_at>now()-interval '24 hours' ORDER BY a.opened_at DESC,a.id DESC LIMIT 8`).Scan(&resolved).Error; e != nil {
			return e
		}
		alerts = append(alerts, resolved...)
		for i, a := range alerts {
			item := domain.DisplayAlert{ID: a.ID, GatewayID: a.GatewayID, ExternalID: tag(a.ExternalID), DeviceName: a.DeviceName, EventType: a.EventType, Severity: a.Severity,
				Title: a.Title, Status: a.Status, OpenedAt: a.OpenedAt, AckedAt: a.AckedAt, Takeover: domain.DisplayTakeover(a.EventType, a.Severity, a.Status), GatewayName: gatewayName[a.GatewayID]}
			// Personal unless the tag is known to be nobody's (fails closed: removed, unregistered, worn anywhere). An
			// emergency button is carried by a person whatever it was registered as.
			if !impersonal[a.ExternalID] || a.EventType == domain.EventButton {
				if !s.ShowNames {
					item.DeviceName, item.Title = domain.DisplayWearerLabel(a.ExternalID), domain.DisplayMaskedTitle(a.EventType)
				} else {
					namesShown = true
				}
			}
			if i >= open {
				item.Takeover = false
				out.Recent = append(out.Recent, item)
				continue
			}
			out.Alerts = append(out.Alerts, item)
		}
		var counts []struct {
			Status, Severity string
			N                int
		}
		if e := tx.Raw(`SELECT status,severity,count(*) AS n FROM core.alerts WHERE status<>'resolved' GROUP BY status,severity`).Scan(&counts).Error; e != nil {
			return e
		}
		for _, c := range counts {
			switch c.Status {
			case "open":
				out.Counts.Open += c.N
				if c.Severity == "critical" {
					out.Counts.Critical += c.N
				}
			case "acknowledged":
				out.Counts.Acknowledged += c.N
			}
		}
		return nil
	})
	return out, namesShown, e
}

// AckFromDisplay acknowledges an open alert on behalf of a display that may do so. The alert must be visible to
// the display (its projects); the acknowledgement names the display, never a member.
func (r *Repository) AckFromDisplay(ctx context.Context, s domain.DisplaySession, alertID string) error {
	if d, ok := domain.DisplayFrom(ctx); !ok || d.ID != s.ID {
		return domain.ErrForbidden
	}
	if !s.AllowAck {
		return domain.Because(domain.ErrForbidden, "display_ack_disabled")
	}
	return r.tx(domain.WithDisplayWrite(ctx), "", s.TenantID, func(tx *gorm.DB) error {
		var active bool
		if e := tx.Raw(`SELECT core.display_active()`).Scan(&active).Error; e != nil {
			return e
		}
		if !active {
			return domain.ErrUnauthorized
		}
		// Only what takes over the screen (an SOS or a hazard) may be acknowledged from it.
		res := tx.Exec(`UPDATE core.alerts SET status='acknowledged',acked_at=now(),acked_by_display=core.display_id()
      WHERE id=? AND status='open' AND ((event_type=? AND severity='critical') OR event_type=?)`, alertID, domain.EventButton, domain.EventHazard)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return domain.ErrNotFound
		}
		if e := signal(tx, s.TenantID, "alert", ""); e != nil {
			return e
		}
		return tx.Exec(`INSERT INTO core.audit_logs(id,tenant_id,actor_id,action,target_id) VALUES(?,core.tenant_id(),core.display_id(),'alert.acknowledged',?)`, uuid.NewString(), alertID).Error
	})
}

// DisplayStudioDashboard returns a dashboard's definition when it is in the display's playlist (a TV renders
// only what its owner chose).
func (r *Repository) DisplayStudioDashboard(ctx context.Context, s domain.DisplaySession, id string) (json.RawMessage, string, error) {
	listed := false
	for _, v := range s.Playlist {
		listed = listed || (v.Kind == "studio" && v.Ref == id)
	}
	if !listed {
		return nil, "", domain.ErrNotFound
	}
	var rows []struct{ Name, Definition string }
	e := r.tx(ctx, "", s.TenantID, func(tx *gorm.DB) error {
		return tx.Raw(`SELECT name,definition::text AS definition FROM core.studio_items WHERE id=? AND kind='dashboard' AND tenant_id=core.tenant_id()`, id).Scan(&rows).Error
	})
	if e != nil {
		return nil, "", e
	}
	if len(rows) != 1 {
		return nil, "", domain.ErrNotFound
	}
	return json.RawMessage(rows[0].Definition), rows[0].Name, nil
}
