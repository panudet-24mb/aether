package postgres

import (
	"aether/backend/internal/domain"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
)

// twinCache shares one site's rows between the tabs and wall screens of the same principal for a moment: a twin
// refetches on every coalesced signal (at most one per 3 s), and several screens show the same site. It holds the rows
// as read, before any people mode is applied: pseudonyms are drawn per response, so two responses never share them.
type twinCache struct {
	mu   sync.Mutex
	rows map[string]twinCached
}

type twinCached struct {
	at  time.Time
	raw twinRaw
}

const twinCacheTTL = 2 * time.Second

var twinStates = &twinCache{rows: map[string]twinCached{}}

func (c *twinCache) get(key string, now time.Time) (twinRaw, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	row, ok := c.rows[key]
	if !ok || now.Sub(row.at) > twinCacheTTL {
		return twinRaw{}, false
	}
	return row.raw, true
}

func (c *twinCache) put(key string, raw twinRaw, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.rows) > 512 {
		for k, v := range c.rows {
			if now.Sub(v.at) > twinCacheTTL {
				delete(c.rows, k)
			}
		}
	}
	c.rows[key] = twinCached{at: now, raw: raw}
}

// twinReading is the part of a stored reading the twin reads (see frontend/app/live/measurements.ts: the same
// rules decide which reading carries a temperature).
type twinReading struct {
	Kind        string             `json:"kind"`
	Frames      []string           `json:"frames"`
	Temperature float64            `json:"temperature"`
	Humidity    float64            `json:"humidity"`
	Battery     float64            `json:"battery"`
	Metrics     map[string]float64 `json:"metrics"`
}

const zigbeeFrame = "z2m-state@1"

// twinControlled are the reading kinds of devices that can be commanded (Zigbee2MQTT and Tuya).
var twinControlled = map[string]bool{"switch": true, "lighting": true, "cover": true, "climate": true, "fan": true}

func (r twinReading) zigbee() bool { return slices.Contains(r.Frames, zigbeeFrame) }

func (r twinReading) environment() bool {
	if r.zigbee() {
		_, ok := r.Metrics["temperature"]
		return ok
	}
	return slices.Contains(r.Frames, "minew-ffe1-a101@1") || r.Kind == "environment" || (r.Kind == "" && len(r.Frames) == 0)
}

// mergeTwinReadings folds a device's recent readings (oldest first) into the numbers the twin draws: a BLE tag
// alternates frames and a Zigbee message reports only what changed, so each value is the last one reported.
func mergeTwinReadings(d *domain.TwinDevice, rows []twinSample) {
	for _, row := range rows {
		var r twinReading
		if json.Unmarshal(row.Reading, &r) != nil {
			continue
		}
		if r.zigbee() {
			if v, ok := r.Metrics["temperature"]; ok {
				d.T = ptr(v)
			}
			if v, ok := r.Metrics["humidity"]; ok {
				d.H = ptr(v)
			}
			if v, ok := r.Metrics["battery"]; ok {
				d.Battery = ptr(v)
			}
		} else {
			if r.environment() {
				d.T, d.H = ptr(r.Temperature), ptr(r.Humidity)
			}
			if r.Battery > 0 { // BLE: 0 means "not in this frame"
				d.Battery = ptr(r.Battery)
			}
		}
		if v, ok := r.Metrics["door"]; ok {
			open := 0
			if v >= 1 {
				open = 1
			}
			d.Door = &open
		}
		if twinControlled[r.Kind] {
			d.Class = r.Kind
			if r.Kind == "switch" || r.Kind == "lighting" || r.Kind == "fan" {
				on, seen := 0, false
				for _, k := range []string{"state", "sw1", "sw2", "sw3", "sw4", "fan_state"} {
					if v, ok := r.Metrics[k]; ok {
						seen = true
						if v >= 1 {
							on = 1
						}
					}
				}
				if seen {
					d.On = &on
				}
			}
			if v, ok := r.Metrics["power"]; ok {
				d.Power = ptr(v)
			}
		}
		if r.Metrics["motion"] >= 1 {
			at := row.ReceivedAt
			d.MotionAt = &at
		}
	}
}

func ptr[T any](v T) *T { return &v }

// pgTextArray quotes every element, so a value with a comma, brace or quote stays one element.
func pgTextArray(values []string) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v) + `"`
	}
	return "{" + strings.Join(parts, ",") + "}"
}

type twinSample struct {
	DeviceID   string
	ReceivedAt time.Time
	Reading    json.RawMessage
}

// twinRaw is one site as read under the caller's row level security, with real names and identities; render turns
// it into what the people mode allows. Never serialised.
type twinRaw struct {
	serverTime time.Time
	demo       bool
	settings   domain.TwinSettings
	revisions  map[string]int
	devices    []domain.TwinDevice
	worn       []twinWorn
	alerts     []twinAlertRow
	// impersonal: tags not carried by people (core.twin_impersonal_tags). Every other tag is personal.
	impersonal map[string]bool
}

type twinWorn struct {
	External, Name     string
	GatewayID          *string
	Since              *time.Time
	CandidateGatewayID *string
	LastAt             *time.Time
}

type twinAlertRow struct {
	ID, Severity, Status, EventType, Title, GatewayID, External string
	OpenedAt                                                    time.Time
}

// TwinPersonalName is what a personal tag is called outside named mode.
const TwinPersonalName = "แท็กบุคคล"

// TwinState is the live state of one site: every placed gateway and device with its latest values, the worn tags
// whose zone is on this site (as the people mode allows), and the open alerts of all of them. A site outside the
// caller's projects, or of another workspace, is ErrNotFound (row level security hides it).
func (r *Repository) TwinState(ctx context.Context, p domain.Principal, siteID, people string) (domain.TwinState, error) {
	display := ""
	if d, ok := domain.DisplayFrom(ctx); ok {
		display = d.ID
	}
	key := strings.Join([]string{p.TenantID, p.UserID, display, p.Role, siteID}, "|")
	now := time.Now()
	raw, ok := twinStates.get(key, now)
	if !ok {
		var e error
		if raw, e = r.twinRaw(ctx, p, siteID); e != nil {
			return domain.TwinState{}, e
		}
		twinStates.put(key, raw, now)
	}
	return renderTwin(raw, siteID, domain.TwinPeopleFor(p, people, raw.settings, display != "", true)), nil
}

// loadTwinSettings reads the workspace's twin settings, or the defaults when its owner saved none.
func loadTwinSettings(tx *gorm.DB) (domain.TwinSettings, error) {
	var rows []struct {
		Demo             bool
		PeopleReplay     *string
		DisplayPeople    *string
		PeopleReplayDays *int
		UpdatedAt        *time.Time
		HistoryDays      int
	}
	if e := tx.Raw(`SELECT coalesce((SELECT demo FROM core.tenants WHERE id=core.tenant_id()),false) AS demo,
       s.people_replay, s.display_people, s.people_replay_days, s.updated_at, core.presence_history_days() AS history_days
     FROM (SELECT 1) one LEFT JOIN core.twin_settings s ON s.tenant_id=core.tenant_id()`).Scan(&rows).Error; e != nil {
		return domain.TwinSettings{}, e
	}
	if len(rows) != 1 {
		return domain.DefaultTwinSettings(false), nil
	}
	r := rows[0]
	out := domain.DefaultTwinSettings(r.Demo)
	out.HistoryDays = r.HistoryDays
	if r.PeopleReplay != nil {
		out.PeopleReplay, out.Stored, out.UpdatedAt = *r.PeopleReplay, true, r.UpdatedAt
	}
	if r.DisplayPeople != nil {
		out.DisplayPeople = *r.DisplayPeople
	}
	if r.PeopleReplayDays != nil {
		out.PeopleReplayDays = *r.PeopleReplayDays
	}
	return out, nil
}

func (r *Repository) twinRaw(ctx context.Context, p domain.Principal, siteID string) (twinRaw, error) {
	out := twinRaw{revisions: map[string]int{}, impersonal: map[string]bool{}}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		var head []struct {
			ServerTime time.Time
			Demo       bool
			Found      bool
		}
		if e := tx.Raw(`SELECT now() AS server_time,
       coalesce((SELECT demo FROM core.tenants WHERE id=core.tenant_id()),false) AS demo,
       EXISTS(SELECT 1 FROM core.sites WHERE id=? AND archived_at IS NULL) AS found`, siteID).Scan(&head).Error; e != nil {
			return e
		}
		if len(head) != 1 || !head[0].Found {
			return domain.ErrNotFound
		}
		out.serverTime, out.demo = head[0].ServerTime, head[0].Demo
		settings, e := loadTwinSettings(tx)
		if e != nil {
			return e
		}
		out.settings = settings

		var floors []struct {
			ID       string
			Revision int
		}
		if e := tx.Raw(`SELECT id,revision FROM core.floors WHERE site_id=? ORDER BY level,created_at LIMIT 60`, siteID).Scan(&floors).Error; e != nil {
			return e
		}
		for _, f := range floors {
			out.revisions[f.ID] = f.Revision
		}

		// Placed gateways and devices (revoked or removed ones are not drawn, as in GetSite).
		var placed []struct {
			FloorID, AssetKind, AssetID, Name string
			External, Profile                 string
			X, Y, Z                           float64
			LastAt                            *time.Time
		}
		if e := tx.Raw(`SELECT pl.floor_id,pl.asset_kind,pl.asset_id,pl.x,pl.y,pl.z,
         coalesce(g.name,d.name) AS name, coalesce(lower(d.external_id),'') AS external, coalesce(d.profile_id,'') AS profile,
         CASE WHEN pl.asset_kind='gateway' THEN greatest(
                (SELECT max(gp.received_at) FROM core.gateway_packets gp WHERE gp.gateway_id=g.id),
                (SELECT max(s.last_seen) FROM core.sensor_streams s WHERE s.gateway_id=g.id))
              ELSE (SELECT max(s.last_seen) FROM core.sensor_streams s WHERE s.external_id=lower(d.external_id)) END AS last_at
       FROM core.floor_placements pl
       JOIN core.floors f ON f.tenant_id=pl.tenant_id AND f.id=pl.floor_id AND f.site_id=?
       LEFT JOIN core.gateways g ON pl.asset_kind='gateway' AND g.tenant_id=pl.tenant_id AND g.id=pl.asset_id AND g.revoked_at IS NULL
       LEFT JOIN core.devices d ON pl.asset_kind='device' AND d.tenant_id=pl.tenant_id AND d.id=pl.asset_id AND d.removed_at IS NULL
       WHERE g.id IS NOT NULL OR d.id IS NOT NULL
       ORDER BY pl.asset_kind,pl.asset_id LIMIT ?`, siteID, domain.TwinMaxPlacements).Scan(&placed).Error; e != nil {
			return e
		}
		gateways, externals, deviceIDs := []string{}, []string{}, []string{}
		byID := map[string]int{}
		for _, pl := range placed {
			d := domain.TwinDevice{ID: pl.AssetID, Kind: pl.AssetKind, External: pl.External, Name: pl.Name, Profile: pl.Profile,
				FloorID: pl.FloorID, X: pl.X, Y: pl.Y, Z: pl.Z, LastAt: pl.LastAt}
			d.Online = pl.LastAt != nil && out.serverTime.Sub(*pl.LastAt) <= domain.TwinFreshSec*time.Second
			byID[d.ID] = len(out.devices)
			out.devices = append(out.devices, d)
			if pl.AssetKind == "gateway" {
				gateways = append(gateways, pl.AssetID)
			} else {
				externals = append(externals, pl.External)
				deviceIDs = append(deviceIDs, pl.AssetID)
			}
		}

		// The last few readings of each placed device from the last day, by the identity index.
		if len(deviceIDs) > 0 {
			var samples []twinSample
			if e := tx.Raw(`SELECT d.id AS device_id, s.received_at, s.reading
         FROM core.devices d CROSS JOIN LATERAL (
           SELECT sm.received_at, sm.reading FROM core.sensor_samples sm
           WHERE sm.external_id=lower(d.external_id) AND sm.received_at > now()-interval '1 day' AND sm.received_at <= now()
           ORDER BY sm.received_at DESC LIMIT 8) s
         WHERE d.id = ANY(?::uuid[])
         ORDER BY d.id, s.received_at`, pgArray(deviceIDs)).Scan(&samples).Error; e != nil {
				return e
			}
			for i := 0; i < len(samples); {
				j := i
				for j < len(samples) && samples[j].DeviceID == samples[i].DeviceID {
					j++
				}
				if k, ok := byID[samples[i].DeviceID]; ok {
					mergeTwinReadings(&out.devices[k], samples[i:j])
				}
				i = j
			}
		}

		// Worn tags whose zone (the gateway the ingest decided they are near) is placed on this site.
		if len(gateways) > 0 {
			if e := tx.Raw(`SELECT lower(d.external_id) AS external, d.name, ps.gateway_id::text AS gateway_id, ps.since,
           ps.candidate_gateway_id::text AS candidate_gateway_id,
           (SELECT max(s.last_seen) FROM core.sensor_streams s WHERE s.external_id=lower(d.external_id)) AS last_at
         FROM core.devices d JOIN core.presence_state ps ON ps.tenant_id=d.tenant_id AND ps.external_id=lower(d.external_id)
         WHERE d.roaming AND d.removed_at IS NULL AND ps.gateway_id = ANY(?::uuid[])
         ORDER BY d.external_id LIMIT ?`, pgArray(gateways), domain.TwinMaxPeople).Scan(&out.worn).Error; e != nil {
				return e
			}
		}

		// Open alerts of anything on this site: placed gateways and devices, and the worn tags in its zones.
		wornExt := []string{}
		for _, w := range out.worn {
			wornExt = append(wornExt, w.External)
		}
		if len(gateways)+len(externals)+len(wornExt) > 0 {
			if e := tx.Raw(`SELECT id,severity,status,event_type,title,gateway_id::text AS gateway_id,lower(external_id) AS external,opened_at
         FROM core.alerts WHERE status IN ('open','acknowledged')
           AND (gateway_id = ANY(?::uuid[]) OR lower(external_id) = ANY(?::text[]))
         ORDER BY opened_at DESC LIMIT ?`, pgArray(gateways), pgTextArray(append(slices.Clone(externals), wornExt...)), domain.TwinMaxAlerts).Scan(&out.alerts).Error; e != nil {
				return e
			}
		}

		// Which of the tags this response mentions are not carried by people, across the whole workspace (a tag may be
		// worn in a project the caller cannot see). Everything not listed is personal.
		mentioned := slices.Clone(externals)
		for _, a := range out.alerts {
			if a.External != "" {
				mentioned = append(mentioned, a.External)
			}
		}
		if len(mentioned) > 0 {
			var ids []string
			if e := tx.Raw(`SELECT t FROM core.twin_impersonal_tags(?::text[], ?::text[]) t`, pgArray(domain.WornProfileIDs()), pgTextArray(mentioned)).Scan(&ids).Error; e != nil {
				return e
			}
			for _, id := range ids {
				out.impersonal[id] = true
			}
		}
		return nil
	})
	return out, e
}

// renderTwin applies a people mode to the rows. Outside named mode nothing in the response identifies a person: a
// personal tag (worn, a button, roaming, or unknown) is called TwinPersonalName with no external id, and an alert about
// one gets a title that names nobody. Pseudonyms (tracks) are drawn per response and people are sorted by them.
func renderTwin(raw twinRaw, siteID, mode string) domain.TwinState {
	out := domain.TwinState{ServerTime: raw.serverTime, SiteID: siteID, Demo: raw.demo, LayoutRevision: raw.revisions,
		Devices: make([]domain.TwinDevice, 0, len(raw.devices)), Alerts: []domain.TwinAlert{},
		Presence: domain.TwinPresence{Mode: mode, Counts: []domain.TwinGatewayCount{}}}
	named := mode == domain.TwinPeopleNamed
	personal := func(external string) bool { return !raw.impersonal[external] }

	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	pid := func(external string) string {
		if named {
			return external
		}
		m := hmac.New(sha256.New, salt)
		m.Write([]byte(external))
		return "p" + hex.EncodeToString(m.Sum(nil)[:6])
	}
	minute := func(t *time.Time) *time.Time {
		if t == nil {
			return nil
		}
		v := t.Truncate(time.Minute)
		return &v
	}

	zoneOf := map[string]string{}
	for _, w := range raw.worn {
		if w.GatewayID != nil {
			zoneOf[w.External] = *w.GatewayID
		}
	}
	deviceOf := map[string]int{}
	for _, d := range raw.devices {
		c := d
		if d.Kind == "device" {
			deviceOf[d.External] = len(out.Devices)
			if !named && personal(d.External) {
				c.Name, c.External = TwinPersonalName, ""
			}
		}
		out.Devices = append(out.Devices, c)
	}

	sosOf := map[string]bool{}
	for _, a := range raw.alerts {
		sos := a.EventType == domain.EventButton && a.Status == "open"
		at := a.GatewayID
		if g, ok := zoneOf[a.External]; ok {
			at = g
		}
		ta := domain.TwinAlert{ID: a.ID, Severity: a.Severity, Status: a.Status, Event: a.EventType, SOS: sos, Hazard: a.EventType == domain.EventHazard, OpenedAt: a.OpenedAt, GatewayID: at, Title: a.Title}
		if !named && personal(a.External) {
			ta.Title = domain.TwinEventTitle(a.EventType)
		}
		k, placed := deviceOf[a.External]
		if placed {
			ta.DeviceID = out.Devices[k].ID
		}
		if _, ok := zoneOf[a.External]; ok && mode != domain.TwinPeopleCounts {
			ta.PID = pid(a.External)
		}
		if a.Status == "open" {
			if placed {
				out.Devices[k].Alert = true
				out.Devices[k].SOS = out.Devices[k].SOS || sos
			}
			if a.External == "" {
				for i := range out.Devices {
					if out.Devices[i].Kind == "gateway" && out.Devices[i].ID == a.GatewayID {
						out.Devices[i].Alert = true
					}
				}
			}
			if sos {
				sosOf[a.External] = true
			}
		}
		out.Alerts = append(out.Alerts, ta)
	}

	counts := map[string]int{}
	for _, w := range raw.worn {
		fresh := w.LastAt != nil && raw.serverTime.Sub(*w.LastAt) <= domain.TwinFreshSec*time.Second
		// An SOS stays on the plan when the tag has gone quiet: the last place it was heard is where to run.
		if (!fresh && !sosOf[w.External]) || w.GatewayID == nil {
			continue
		}
		counts[*w.GatewayID]++
		switch mode {
		case domain.TwinPeopleTracks:
			// Times to the minute: exact seconds would link a pseudonym across responses.
			out.Presence.People = append(out.Presence.People, domain.TwinPerson{PID: pid(w.External), GatewayID: w.GatewayID, Since: minute(w.Since),
				CandidateGatewayID: w.CandidateGatewayID, LastAt: minute(w.LastAt), SOS: sosOf[w.External]})
		case domain.TwinPeopleNamed:
			out.Presence.People = append(out.Presence.People, domain.TwinPerson{PID: w.External, Name: w.Name, GatewayID: w.GatewayID, Since: w.Since,
				CandidateGatewayID: w.CandidateGatewayID, LastAt: w.LastAt, SOS: sosOf[w.External]})
		}
	}
	// Sorted by pseudonym, so the order says nothing about who is who.
	slices.SortFunc(out.Presence.People, func(a, b domain.TwinPerson) int { return strings.Compare(a.PID, b.PID) })
	for _, d := range raw.devices {
		if d.Kind == "gateway" && counts[d.ID] > 0 {
			out.Presence.Counts = append(out.Presence.Counts, domain.TwinGatewayCount{GatewayID: d.ID, N: counts[d.ID]})
		}
	}
	return out
}
