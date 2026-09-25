package postgres

import (
	"aether/backend/internal/adapters/edge"
	"aether/backend/internal/adapters/tuya"
	"aether/backend/internal/adapters/zigbee2mqtt"
	"aether/backend/internal/domain"
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// EdgeSilentAfter is how long an Aether Edge may send nothing before its devices count as offline although no
// last will arrived. The agent publishes health every 60 s, so three minutes tolerates two missed heartbeats.
const EdgeSilentAfter = 3 * time.Minute

// EdgeDiscoveryEvery is the least time between two LAN lists accepted from one Edge (the agent sends one a minute).
const EdgeDiscoveryEvery = 30 * time.Second

// CaptureEdge stores one message an Aether Edge published under its gateway's topic tree. Like CaptureZ2M it runs
// in one transaction under the gateway's row lock, keeps a bounded diagnostic copy and feeds the same sample,
// stream-state, command-confirmation and event pipeline, so a Tuya plug shows up exactly like a Zigbee one.
func (r *Repository) CaptureEdge(ctx context.Context, tenant, gateway string, m edge.Message, payload []byte) (string, error) {
	id := uuid.NewString()
	e := r.tx(ctx, "", tenant, func(tx *gorm.DB) error {
		var models []string
		if e := tx.Raw(`SELECT model FROM core.gateways WHERE id=? AND revoked_at IS NULL FOR UPDATE`, gateway).Scan(&models).Error; e != nil {
			return e
		}
		if len(models) != 1 {
			return domain.ErrUnauthorized
		}
		if models[0] != domain.EdgeGatewayModel {
			return domain.ErrForbidden // the ACL already confines the tree; this is the second lock on the door
		}
		now := time.Now().UTC()
		if e := tx.Exec(`INSERT INTO core.gateway_packets(id,tenant_id,gateway_id,payload) VALUES(?,?,?,?::jsonb)`, id, tenant, gateway, edgeDiagnostic(m, payload)).Error; e != nil {
			return e
		}
		agentDown := false
		online, _, statusOK := edge.ParseOnline(payload)
		if m.Kind == edge.Status {
			agentDown = statusOK && !online
		}
		// Any message but the agent's own "offline" proves it is up.
		if !agentDown {
			if e := r.agentOnline(tx, tenant, gateway, edgeAgent, "edge", now); e != nil {
				return e
			}
		}
		var e error
		switch m.Kind {
		case edge.Status:
			if agentDown {
				e = r.agentOffline(tx, tenant, gateway, edgeAgent, "edge_offline", now)
			} else if statusOK {
				e = tx.Exec(`INSERT INTO core.edge_agents(tenant_id,gateway_id,state,state_at,updated_at) VALUES(?,?,'online',?,?)
    ON CONFLICT(tenant_id,gateway_id) DO UPDATE SET state='online',state_at=EXCLUDED.state_at,updated_at=EXCLUDED.updated_at`, tenant, gateway, now, now).Error
			}
		case edge.Health:
			h, ok := edge.ParseHealth(payload)
			if !ok {
				return domain.ErrInvalid
			}
			e = tx.Exec(`INSERT INTO core.edge_agents(tenant_id,gateway_id,version,last_health_at,devices_connected,lan_seen,updated_at) VALUES(?,?,?,?,?,?,?)
    ON CONFLICT(tenant_id,gateway_id) DO UPDATE SET version=EXCLUDED.version,last_health_at=EXCLUDED.last_health_at,devices_connected=EXCLUDED.devices_connected,
      lan_seen=EXCLUDED.lan_seen,updated_at=EXCLUDED.updated_at`, tenant, gateway, h.Version, now, h.DevicesConnected, h.LANSeen, now).Error
		case edge.Discovery:
			e = saveLANDevices(tx, tenant, gateway, payload, now)
		case edge.State:
			e = r.saveTuyaState(tx, tenant, gateway, m, payload, now)
		case edge.Availability:
			e = r.saveTuyaAvailability(tx, tenant, gateway, m, payload, now)
		}
		if e != nil {
			return e
		}
		if e := signal(tx, tenant, "packet", gateway); e != nil {
			return e
		}
		return tx.Exec(`DELETE FROM core.gateway_packets WHERE gateway_id=? AND id IN (SELECT id FROM core.gateway_packets WHERE gateway_id=? ORDER BY received_at DESC,id DESC OFFSET 100)`, gateway, gateway).Error
	})
	return id, dataError(e)
}

// edgeDiagnostic is the copy kept in core.gateway_packets: the topic with its payload, and only a count for the
// LAN list. It is an object, so the Minew projection over stored packets skips it.
func edgeDiagnostic(m edge.Message, payload []byte) string {
	d := map[string]any{"edge_topic": m.Topic}
	switch {
	case m.Kind == edge.Discovery:
		var list []json.RawMessage
		_ = json.Unmarshal(payload, &list)
		d["devices"] = len(list)
	case json.Valid(payload):
		d["payload"] = json.RawMessage(payload)
	default:
		d["payload"] = string(payload)
	}
	b, _ := json.Marshal(d)
	return string(b)
}

// saveLANDevices records the Tuya devices the agent sees broadcasting. An imported device whose address or
// protocol version changed gets the new one, and the agent's configuration revision moves so it reconnects there.
// Entries unseen for a week are forgotten, which bounds the table.
func saveLANDevices(tx *gorm.DB, tenant, gateway string, payload []byte, now time.Time) error {
	devices, e := edge.ParseDiscovery(payload)
	if e != nil {
		return e
	}
	// At most one list per EdgeDiscoveryEvery per gateway: a chatty or hostile agent cannot turn discovery into a
	// write storm. The row is locked by the upsert, so two collectors cannot both pass.
	var accepted []bool
	if e := tx.Raw(`INSERT INTO core.edge_agents(tenant_id,gateway_id,last_discovery_at,updated_at) VALUES(?,?,?,?)
    ON CONFLICT(tenant_id,gateway_id) DO UPDATE SET last_discovery_at=EXCLUDED.last_discovery_at,updated_at=EXCLUDED.updated_at
      WHERE core.edge_agents.last_discovery_at IS NULL OR core.edge_agents.last_discovery_at<=?
    RETURNING true`, tenant, gateway, now, now, now.Add(-EdgeDiscoveryEvery)).Scan(&accepted).Error; e != nil {
		return e
	}
	if len(accepted) == 0 {
		return nil
	}
	changed := false
	for _, d := range devices {
		if e := tx.Exec(`INSERT INTO core.edge_lan_devices(tenant_id,gateway_id,device_id,ip,version,product_key,last_seen) VALUES(?,?,?,?,?,?,?)
    ON CONFLICT(tenant_id,gateway_id,device_id) DO UPDATE SET ip=EXCLUDED.ip,version=EXCLUDED.version,product_key=EXCLUDED.product_key,last_seen=EXCLUDED.last_seen`,
			tenant, gateway, d.ID, d.IP, d.Version, d.ProductKey, now).Error; e != nil {
			return e
		}
		res := tx.Exec(`UPDATE core.tuya_devices SET ip=?,version=?,updated_at=? WHERE gateway_id=? AND tuya_id=? AND (ip<>? OR version<>?)`, d.IP, d.Version, now, gateway, d.ID, d.IP, d.Version)
		if res.Error != nil {
			return res.Error
		}
		changed = changed || res.RowsAffected > 0
	}
	if e := tx.Exec(`DELETE FROM core.edge_lan_devices WHERE gateway_id=? AND last_seen<?`, gateway, now.Add(-7*24*time.Hour)).Error; e != nil {
		return e
	}
	// MaxLAN bounds one message; this bounds the gateway: only its newest MaxLAN devices are kept.
	if e := tx.Exec(`DELETE FROM core.edge_lan_devices WHERE gateway_id=? AND device_id IN (
    SELECT device_id FROM core.edge_lan_devices WHERE gateway_id=? ORDER BY last_seen DESC,device_id OFFSET ?)`, gateway, gateway, edge.MaxLAN).Error; e != nil {
		return e
	}
	if changed {
		return bumpEdgeConfig(tx, gateway)
	}
	return nil
}

// bumpEdgeConfig moves an Aether Edge's configuration revision, so the agent fetches what it must connect to
// again. It does nothing for a gateway of any other model.
func bumpEdgeConfig(tx *gorm.DB, gateway string) error {
	return tx.Exec(`INSERT INTO core.edge_agents(tenant_id,gateway_id,config_revision,updated_at)
    SELECT g.tenant_id,g.id,1,now() FROM core.gateways g WHERE g.id=? AND g.model=?
    ON CONFLICT(tenant_id,gateway_id) DO UPDATE SET config_revision=core.edge_agents.config_revision+1,updated_at=now()`, gateway, domain.EdgeGatewayModel).Error
}

// bumpEdgeConfigOfDevice is bumpEdgeConfig for the gateway a registration belongs to.
func bumpEdgeConfigOfDevice(tx *gorm.DB, device string) error {
	var gateways []string
	if e := tx.Raw(`SELECT gateway_id::text FROM core.devices WHERE id=?`, device).Scan(&gateways).Error; e != nil || len(gateways) != 1 {
		return e
	}
	return bumpEdgeConfig(tx, gateways[0])
}

// tuyaDevice is one imported Tuya device as the ingest needs it, in the shape of a Zigbee2MQTT device (the
// exposes translated from its specification), plus its dp map.
func tuyaDevice(tx *gorm.DB, gateway, id string) (zigbee2mqtt.Device, map[string]tuya.Ref, bool, error) {
	var rows []struct {
		TuyaID, Name, TuyaCategory, Category string
		Gangs, Exposes, DPMap                json.RawMessage
	}
	if e := tx.Raw(`SELECT tuya_id,name,tuya_category,category,gangs,exposes,dp_map FROM core.tuya_devices WHERE gateway_id=? AND tuya_id=? AND removed_at IS NULL`, gateway, id).Scan(&rows).Error; e != nil || len(rows) == 0 {
		return zigbee2mqtt.Device{}, nil, false, e
	}
	row := rows[0]
	name := row.Name
	if name == "" {
		name = row.TuyaID
	}
	model := "Tuya"
	if row.TuyaCategory != "" {
		model = "Tuya " + row.TuyaCategory
	}
	d := zigbee2mqtt.Device{IEEE: row.TuyaID, FriendlyName: name, Model: model, Vendor: "Tuya", Exposes: row.Exposes, Category: row.Category}
	if e := json.Unmarshal(row.Gangs, &d.Gangs); e != nil {
		return zigbee2mqtt.Device{}, nil, false, e
	}
	dpMap := map[string]tuya.Ref{}
	if e := json.Unmarshal(row.DPMap, &dpMap); e != nil {
		return zigbee2mqtt.Device{}, nil, false, e
	}
	return d, dpMap, true, nil
}

// saveTuyaState converts the device's raw data points into property values through its dp map and hands them
// to the pipeline every definition-backed device shares (ingestDeviceState).
func (r *Repository) saveTuyaState(tx *gorm.DB, tenant, gateway string, m edge.Message, payload []byte, now time.Time) error {
	dps, _, e := edge.ParseState(payload)
	if e != nil {
		return e
	}
	d, dpMap, ok, e := tuyaDevice(tx, gateway, m.Device)
	if e != nil || !ok {
		return e // a device that was not imported: the diagnostic copy is all there is
	}
	properties := tuya.State(dpMap, dps)
	if len(properties) == 0 {
		return nil
	}
	converted, e := json.Marshal(properties)
	if e != nil {
		return e
	}
	features, _ := zigbee2mqtt.Features(d.Exposes)
	settable := zigbee2mqtt.SettableValuesIn(features, converted)
	if len(settable) > 0 {
		merged, _ := json.Marshal(settable)
		if e := tx.Exec(`UPDATE core.tuya_devices SET state=state||?::jsonb WHERE gateway_id=? AND tuya_id=? AND state IS DISTINCT FROM state||?::jsonb`, string(merged), gateway, d.IEEE, string(merged)).Error; e != nil {
			return e
		}
	}
	return r.ingestDeviceState(tx, tenant, gateway, d, features, settable, converted, m.Topic, edge.FrameState, now)
}

// saveTuyaAvailability records the agent's verdict on one device. A local key the device refused (3.4/3.5
// negotiation, definitive) or that does not decrypt its replies (3.1/3.3, a suspicion) is flagged on the device,
// and the offline event names it, so the UI can ask for a new import instead of showing a plain outage.
func (r *Repository) saveTuyaAvailability(tx *gorm.DB, tenant, gateway string, m edge.Message, payload []byte, now time.Time) error {
	online, reason, ok := edge.ParseOnline(payload)
	if !ok {
		return nil
	}
	d, _, found, e := tuyaDevice(tx, gateway, m.Device)
	if e != nil || !found {
		return e
	}
	// Only a device that HAS a key can have it confirmed, refused or suspected: a device without one stays
	// 'missing' whatever the agent says (only an import gives it a key).
	keyStatus := `key_status`
	switch {
	case online:
		// Connected with the key it has: whatever was suspected is settled.
		keyStatus = `CASE WHEN key_status IN ('rejected','suspect') AND local_key_sealed IS NOT NULL THEN 'ok' ELSE key_status END`
	case reason == edge.ReasonAuthFailed:
		keyStatus = `CASE WHEN local_key_sealed IS NOT NULL THEN 'rejected' ELSE key_status END`
	case reason == edge.ReasonKeySuspect:
		keyStatus = `CASE WHEN local_key_sealed IS NOT NULL THEN 'suspect' ELSE key_status END`
	}
	if e := tx.Exec(`UPDATE core.tuya_devices SET available=?,available_at=?,reason=?,key_status=`+keyStatus+`,updated_at=? WHERE gateway_id=? AND tuya_id=?`,
		online, now, reason, now, gateway, d.IEEE).Error; e != nil {
		return e
	}
	source := "availability"
	if reason == edge.ReasonAuthFailed {
		source = "key_rejected"
	}
	return r.setReportedLiveness(tx, tenant, gateway, d.IEEE, d.FriendlyName, online, source, now)
}

// TuyaImport is one device of a key import (phase D fills it from the Tuya cloud). The local key is already
// sealed; the repository never sees it in the clear.
type TuyaImport struct {
	TuyaID, Name, Category, ProductID string
	Sub                               bool
	Spec                              []tuya.DP
	LocalKeySealed                    string
	KeyFingerprint                    string
}

// SaveTuyaDevices stores imported devices under an Aether Edge gateway: their specification, its translation, and
// the sealed local key. A device imported again gets the new key and specification (a re-paired device changes
// key); devices missing from an import are left alone. The agent's configuration revision moves once.
func (r *Repository) SaveTuyaDevices(ctx context.Context, p domain.Principal, gateway string, devices []TuyaImport) (int, error) {
	saved := 0
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		var models []string
		if e := tx.Raw(`SELECT model FROM core.gateways WHERE id=? AND revoked_at IS NULL FOR UPDATE`, gateway).Scan(&models).Error; e != nil {
			return e
		}
		if len(models) != 1 {
			return domain.ErrNotFound
		}
		if models[0] != domain.EdgeGatewayModel {
			return domain.Because(domain.ErrInvalid, "not_an_edge_gateway")
		}
		now := time.Now().UTC()
		for _, d := range devices {
			id := strings.ToLower(strings.TrimSpace(d.TuyaID))
			if !edge.ValidDevice(id) || tuya.Validate(d.Spec) != nil || len(d.LocalKeySealed) > 512 || !fingerprintPattern.MatchString(d.KeyFingerprint) {
				return domain.ErrInvalid
			}
			tr := tuya.Translate(d.Category, d.Spec)
			spec, _ := json.Marshal(d.Spec)
			dpMap, _ := json.Marshal(tr.DPMap)
			gangs, _ := json.Marshal(tr.Gangs)
			if len(spec) > 65536 || len(tr.Exposes) > 65536 || len(dpMap) > 65536 {
				return domain.ErrInvalid
			}
			var key any
			status, fingerprint := "missing", ""
			if d.LocalKeySealed != "" {
				key, status, fingerprint = d.LocalKeySealed, "ok", d.KeyFingerprint
			}
			if e := tx.Exec(`INSERT INTO core.tuya_devices(tenant_id,gateway_id,tuya_id,name,tuya_category,product_id,sub,spec,exposes,dp_map,gangs,category,local_capable,
      local_key_sealed,key_fingerprint,key_status,ip,version,imported_at,updated_at,removed_at)
    SELECT ?,?,?,?,?,?,?,?::jsonb,?::jsonb,?::jsonb,?::jsonb,?,?,?,?,?,coalesce(l.ip,''),coalesce(l.version,''),?,?,NULL
    FROM (SELECT 1) one LEFT JOIN core.edge_lan_devices l ON l.gateway_id=? AND l.device_id=?
    ON CONFLICT(tenant_id,gateway_id,tuya_id) DO UPDATE SET name=EXCLUDED.name,tuya_category=EXCLUDED.tuya_category,product_id=EXCLUDED.product_id,sub=EXCLUDED.sub,
      spec=EXCLUDED.spec,exposes=EXCLUDED.exposes,dp_map=EXCLUDED.dp_map,gangs=EXCLUDED.gangs,category=EXCLUDED.category,local_capable=EXCLUDED.local_capable,
      local_key_sealed=EXCLUDED.local_key_sealed,key_fingerprint=EXCLUDED.key_fingerprint,key_status=EXCLUDED.key_status,imported_at=EXCLUDED.imported_at,
      updated_at=EXCLUDED.updated_at,removed_at=NULL`,
				p.TenantID, gateway, id, clipText(d.Name, 128), clipText(d.Category, 32), clipText(d.ProductID, 64), d.Sub, string(spec), string(tr.Exposes), string(dpMap), string(gangs),
				tr.Category, tr.LocalCapable && !d.Sub, key, fingerprint, status, now, now, gateway, id).Error; e != nil {
				return e
			}
			saved++
		}
		if e := bumpEdgeConfig(tx, gateway); e != nil {
			return e
		}
		if e := signal(tx, p.TenantID, "inventory", gateway); e != nil {
			return e
		}
		return audit(tx, p, "tuya.keys_imported", gateway)
	})
	return saved, classify(e)
}

// fingerprintPattern is a key fingerprint as stored: a short lower-case hex prefix of the key's hash.
var fingerprintPattern = regexp.MustCompile(`^[0-9a-f]{0,16}$`)

// clipText is edge.Clip: bounded, NUL-free, never split inside a UTF-8 character.
func clipText(s string, n int) string { return edge.Clip(s, n) }
