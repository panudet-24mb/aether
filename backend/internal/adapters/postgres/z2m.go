package postgres

import (
	"aether/backend/internal/adapters/minew"
	"aether/backend/internal/adapters/zigbee2mqtt"
	"aether/backend/internal/alerts"
	"aether/backend/internal/domain"
	"aether/backend/internal/security"
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// CaptureZ2M stores one message a Zigbee2MQTT bridge published under its gateway's base topic. Like
// CapturePacket it runs in one transaction under the gateway's row lock, keeps a bounded diagnostic copy in
// core.gateway_packets and feeds the same sample, stream-state and event pipeline, so a wall switch shows up in
// the live view, the event log and alerts exactly like a BLE tag does.
func (r *Repository) CaptureZ2M(ctx context.Context, tenant, gateway string, m zigbee2mqtt.Message, payload []byte) (string, error) {
	id := uuid.NewString()
	e := r.tx(ctx, "", tenant, func(tx *gorm.DB) error {
		var models []string
		if e := tx.Raw(`SELECT model FROM core.gateways WHERE id=? AND revoked_at IS NULL FOR UPDATE`, gateway).Scan(&models).Error; e != nil {
			return e
		}
		if len(models) != 1 {
			return domain.ErrUnauthorized
		}
		if models[0] != domain.Z2MGatewayModel {
			return domain.ErrForbidden // the ACL already confines the tree; this is the second lock on the door
		}
		now := time.Now().UTC()
		if e := tx.Exec(`INSERT INTO core.gateway_packets(id,tenant_id,gateway_id,payload) VALUES(?,?,?,?::jsonb)`, id, tenant, gateway, z2mDiagnostic(m, payload)).Error; e != nil {
			return e
		}
		var e error
		bridgeDown := false
		if m.Kind == zigbee2mqtt.BridgeState {
			online, ok := zigbee2mqtt.ParseOnline(payload)
			bridgeDown = ok && !online
		}
		// Any message but the bridge's own "offline" proves the bridge is up again.
		if !bridgeDown {
			if e := r.bridgeOnline(tx, tenant, gateway, now); e != nil {
				return e
			}
		}
		switch m.Kind {
		case zigbee2mqtt.BridgeDevices:
			e = saveZ2MDevices(tx, tenant, gateway, payload, now)
		case zigbee2mqtt.BridgeState:
			if bridgeDown {
				e = r.bridgeOffline(tx, tenant, gateway, "bridge_offline", now)
			} else if _, ok := zigbee2mqtt.ParseOnline(payload); ok {
				e = tx.Exec(`INSERT INTO core.z2m_bridges(tenant_id,gateway_id,state,state_at,updated_at) VALUES(?,?,'online',?,?)
    ON CONFLICT(tenant_id,gateway_id) DO UPDATE SET state='online',state_at=EXCLUDED.state_at,updated_at=EXCLUDED.updated_at`, tenant, gateway, now, now).Error
			}
		case zigbee2mqtt.BridgeInfo, zigbee2mqtt.Heartbeat:
			version := ""
			if m.Kind == zigbee2mqtt.BridgeInfo {
				version = zigbee2mqtt.ParseBridgeVersion(payload)
			}
			e = tx.Exec(`INSERT INTO core.z2m_bridges(tenant_id,gateway_id,version,updated_at) VALUES(?,?,?,?)
    ON CONFLICT(tenant_id,gateway_id) DO UPDATE SET version=CASE WHEN EXCLUDED.version<>'' THEN EXCLUDED.version ELSE core.z2m_bridges.version END,updated_at=EXCLUDED.updated_at`, tenant, gateway, version, now).Error
		case zigbee2mqtt.State:
			e = r.saveZ2MState(tx, tenant, gateway, m, payload, now)
		case zigbee2mqtt.Availability:
			e = r.saveZ2MAvailability(tx, tenant, gateway, m, payload, now)
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

// z2mDiagnostic is the copy kept in core.gateway_packets: the topic with its payload (a JSON value, or the
// legacy bare string), and only a count for bridge/devices, which can be large. It is an object, so the Minew
// projection over stored packets (which reads arrays of rows) skips it.
func z2mDiagnostic(m zigbee2mqtt.Message, payload []byte) string {
	d := map[string]any{"z2m_topic": m.Topic}
	switch {
	case m.Kind == zigbee2mqtt.BridgeDevices:
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

func saveZ2MDevices(tx *gorm.DB, tenant, gateway string, payload []byte, now time.Time) error {
	devices, truncated, e := zigbee2mqtt.ParseBridgeDevices(payload)
	if e != nil {
		return domain.ErrInvalid
	}
	present := make([]string, 0, len(devices))
	for _, d := range devices {
		gangs, _ := json.Marshal(d.Gangs)
		exposes := d.Exposes
		if len(exposes) == 0 {
			exposes = json.RawMessage("[]")
		}
		if e := tx.Exec(`INSERT INTO core.z2m_devices(tenant_id,gateway_id,ieee,friendly_name,type,model,vendor,model_id,manufacturer,power_source,supported,gangs,exposes,category,sos,description,updated_at,removed_at)
    VALUES(?,?,?,?,?,?,?,?,?,?,?,?::jsonb,?::jsonb,?,?,?,?,NULL)
    ON CONFLICT(tenant_id,gateway_id,ieee) DO UPDATE SET friendly_name=EXCLUDED.friendly_name,type=EXCLUDED.type,model=EXCLUDED.model,vendor=EXCLUDED.vendor,
      model_id=EXCLUDED.model_id,manufacturer=EXCLUDED.manufacturer,power_source=EXCLUDED.power_source,supported=EXCLUDED.supported,gangs=EXCLUDED.gangs,
      exposes=EXCLUDED.exposes,category=EXCLUDED.category,sos=EXCLUDED.sos,description=EXCLUDED.description,updated_at=EXCLUDED.updated_at,removed_at=NULL`,
			tenant, gateway, d.IEEE, d.FriendlyName, d.Type, d.Model, d.Vendor, d.ModelID, d.Manufacturer, d.PowerSource, d.Supported, string(gangs), string(exposes), d.Category, d.SOS, d.Description, now).Error; e != nil {
			return e
		}
		present = append(present, d.IEEE)
	}
	// The document is the whole network: anything no longer in it was removed from the coordinator. A list cut
	// at MaxDevices is not the whole network, so nothing is swept then.
	if truncated {
		slog.Warn("Zigbee2MQTT device list truncated; removals not applied", "gateway", gateway, "max", zigbee2mqtt.MaxDevices)
		return nil
	}
	var removed []string
	q := `UPDATE core.z2m_devices SET removed_at=?,updated_at=? WHERE gateway_id=? AND removed_at IS NULL RETURNING ieee`
	args := []any{now, now, gateway}
	if len(present) > 0 {
		q = `UPDATE core.z2m_devices SET removed_at=?,updated_at=? WHERE gateway_id=? AND removed_at IS NULL AND ieee NOT IN ? RETURNING ieee`
		args = append(args, present)
	}
	if e := tx.Raw(q, args...).Scan(&removed).Error; e != nil {
		return e
	}
	if len(removed) == 0 {
		return nil
	}
	// A device unpaired from the coordinator will never report availability again: its stream goes back to
	// silence-based liveness, so the ordinary offline scan ages it out like any tag that stopped transmitting.
	return tx.Exec(`UPDATE core.sensor_streams SET liveness='silence' WHERE gateway_id=? AND external_id IN ?`, gateway, removed).Error
}

// z2mDevice finds the device a topic names: by friendly name, else by the IEEE address the message carries
// (include_device_information) or the topic itself is (the default friendly name).
func z2mDevice(tx *gorm.DB, gateway, name string, payload []byte) (zigbee2mqtt.Device, bool, error) {
	ieee := zigbee2mqtt.DeviceIEEE(payload)
	if ieee == "" && zigbee2mqtt.ValidIEEE(strings.ToLower(name)) {
		ieee = strings.ToLower(name)
	}
	var rows []struct {
		IEEE         string          `gorm:"column:ieee"`
		FriendlyName string          `gorm:"column:friendly_name"`
		Model        string          `gorm:"column:model"`
		Gangs        json.RawMessage `gorm:"column:gangs"`
		Exposes      json.RawMessage `gorm:"column:exposes"`
		Category     string          `gorm:"column:category"`
		SOS          bool            `gorm:"column:sos"`
	}
	if e := tx.Raw(`SELECT ieee,friendly_name,model,gangs,exposes,category,sos FROM core.z2m_devices WHERE gateway_id=? AND removed_at IS NULL AND (friendly_name=? OR ieee=?)
    ORDER BY (ieee=?) DESC LIMIT 1`, gateway, name, ieee, ieee).Scan(&rows).Error; e != nil || len(rows) == 0 {
		return zigbee2mqtt.Device{}, false, e
	}
	d := zigbee2mqtt.Device{IEEE: rows[0].IEEE, FriendlyName: rows[0].FriendlyName, Model: rows[0].Model, Exposes: rows[0].Exposes, Category: rows[0].Category, SOS: rows[0].SOS}
	if e := json.Unmarshal(rows[0].Gangs, &d.Gangs); e != nil {
		return zigbee2mqtt.Device{}, false, e
	}
	// A device stored before categories existed (migration 00030) has its definition but no category or SOS flag
	// yet: derive them from the stored exposes on first touch, so an SOS button paired before the upgrade rings
	// without waiting for the bridge to republish bridge/devices. The button gate reads the flag in this same
	// transaction.
	if d.Category == "" && len(d.Exposes) > 2 {
		features, _ := zigbee2mqtt.Features(d.Exposes)
		profile := zigbee2mqtt.Summarize(features)
		d.Category, d.SOS = profile.Category(), profile.SOS
		if len(d.Gangs) > 0 && d.Category == zigbee2mqtt.CategoryInfo {
			d.Category = zigbee2mqtt.CategorySwitch
		}
		if e := tx.Exec(`UPDATE core.z2m_devices SET category=?,sos=? WHERE gateway_id=? AND ieee=? AND category=''`, d.Category, d.SOS, gateway, d.IEEE).Error; e != nil {
			return zigbee2mqtt.Device{}, false, e
		}
	}
	return d, true, nil
}

func (r *Repository) saveZ2MState(tx *gorm.DB, tenant, gateway string, m zigbee2mqtt.Message, payload []byte, now time.Time) error {
	d, ok, e := z2mDevice(tx, gateway, m.Device, payload)
	if e != nil || !ok {
		return e // a group or a device bridge/devices has not listed yet: the diagnostic copy is all there is
	}
	// The last reported value of every settable property (a toggle is resolved from it), and the commands this
	// report answers, in the same transaction as the report itself. This holds for any device, not only switches.
	// The definition is parsed once per message; the stored state is only written when a value changed.
	features, _ := zigbee2mqtt.Features(d.Exposes)
	settable := zigbee2mqtt.SettableValuesIn(features, payload)
	if len(settable) > 0 {
		merged, _ := json.Marshal(settable)
		if e := tx.Exec(`UPDATE core.z2m_devices SET state=state||?::jsonb WHERE gateway_id=? AND ieee=? AND state IS DISTINCT FROM state||?::jsonb`, string(merged), gateway, d.IEEE, string(merged)).Error; e != nil {
			return e
		}
	}
	return r.ingestDeviceState(tx, tenant, gateway, d, features, settable, payload, m.Topic, "", now)
}

// ingestDeviceState is the part of a state report shared by every definition-backed device (a Zigbee2MQTT device,
// or a Tuya device reached through Aether Edge, whose data points were converted into the same property form):
// the commands the report answers, the reading built from the definition, thinning, the sample, reported liveness
// and the events. The caller has already stored the settable values it carries. frame, when set, is stored next
// to the generic zigbee2mqtt.FrameState so the source of the reading stays visible.
func (r *Repository) ingestDeviceState(tx *gorm.DB, tenant, gateway string, d zigbee2mqtt.Device, features []zigbee2mqtt.Feature, settable map[string]json.RawMessage, payload []byte, topic, frame string, now time.Time) error {
	confirmed, e := confirmCommands(tx, tenant, gateway, d.IEEE, features, settable, now)
	if e != nil {
		return e
	}
	reading, ok := zigbee2mqtt.ParseStateWith(payload, d, features, now)
	if !ok {
		return nil
	}
	if frame != "" {
		reading.Frames = append(reading.Frames, frame)
	}
	for _, id := range confirmed {
		reading.Confirmed = append(reading.Confirmed, id)
	}
	sort.Strings(reading.Confirmed)
	// A confirmed command on a gang's property makes that gang's switch event name the command.
	for _, g := range d.Gangs {
		if id := confirmed[g.Property]; id != "" {
			if reading.Commands == nil {
				reading.Commands = map[int]string{}
			}
			reading.Commands[g.Gang] = id
		}
	}
	view := minew.View{Sensors: []minew.Sensor{{ID: d.IEEE, Name: d.FriendlyName, Kind: reading.Kind, Model: d.Model, Latest: reading}}}
	thin, e := z2mThinned(tx, tenant, gateway, d.IEEE, reading, settable, len(confirmed) > 0, now, r.opts)
	if e != nil {
		return e
	}
	if thin {
		if e := tx.Exec(`UPDATE core.sensor_streams SET last_seen=greatest(last_seen,?) WHERE gateway_id=? AND external_id=?`, now, gateway, d.IEEE).Error; e != nil {
			return e
		}
	} else {
		// A switch legitimately repeats ON, OFF, ON with identical payloads, and a button's action arrives once
		// per press: the key includes the receive time.
		key := security.Digest(gateway + "\x00" + topic + "\x00" + string(payload) + "\x00" + strconv.FormatInt(now.UnixNano(), 10))
		if e := saveSamples(tx, tenant, gateway, view, key, now, r.opts); e != nil {
			return e
		}
	}
	if e := tx.Exec(`UPDATE core.sensor_streams SET liveness='reported' WHERE gateway_id=? AND external_id=? AND liveness<>'reported'`, gateway, d.IEEE).Error; e != nil {
		return e
	}
	if e := tx.SavePoint("alert_events").Error; e != nil {
		return e
	}
	if e := saveEvents(tx, tenant, gateway, view, nil, now, r.opts); e != nil {
		slog.Warn("alert evaluation failed; Zigbee2MQTT state stored without events", "gateway", gateway, "error", e.Error())
		return tx.RollbackTo("alert_events").Error
	}
	return nil
}

// saveZ2MAvailability records Z2M's own verdict on a device (availability must be enabled in Z2M). The device's
// stream_state.offline follows it and each change is one offline / online event.
func (r *Repository) saveZ2MAvailability(tx *gorm.DB, tenant, gateway string, m zigbee2mqtt.Message, payload []byte, now time.Time) error {
	online, ok := zigbee2mqtt.ParseOnline(payload)
	if !ok {
		return nil
	}
	d, found, e := z2mDevice(tx, gateway, m.Device, payload)
	if e != nil || !found {
		return e
	}
	if e := tx.Exec(`UPDATE core.z2m_devices SET available=?,available_at=? WHERE gateway_id=? AND ieee=?`, online, now, gateway, d.IEEE).Error; e != nil {
		return e
	}
	return r.setReportedLiveness(tx, tenant, gateway, d.IEEE, d.FriendlyName, online, "availability", now)
}

// setReportedLiveness moves one reported-liveness stream online or offline and raises the event, once per
// change. The first report of a device only records an online state; a first "offline" is still an event. A
// device without a stream (it never reported state) has nothing registered that could be offline.
func (r *Repository) setReportedLiveness(tx *gorm.DB, tenant, gateway, ieee, fallbackName string, online bool, source string, now time.Time) error {
	var prev []stateRow
	if e := tx.Raw(`SELECT `+stateColumns+` FROM core.stream_state WHERE gateway_id=? AND external_id=? FOR UPDATE`, gateway, ieee).Scan(&prev).Error; e != nil {
		return e
	}
	st := alerts.State{Known: true}
	if len(prev) == 1 {
		st = prev[0].state()
		if st.Offline == !online {
			return nil
		}
	} else if online {
		_, e := upsertState(tx, tenant, gateway, ieee, st)
		return e
	}
	st.Offline = !online
	tracked, e := upsertState(tx, tenant, gateway, ieee, st)
	if e != nil || !tracked {
		return e
	}
	var names []string
	if e := tx.Raw(`SELECT name FROM core.sensor_streams WHERE gateway_id=? AND external_id=?`, gateway, ieee).Scan(&names).Error; e != nil {
		return e
	}
	name := fallbackName
	if len(names) == 1 && names[0] != "" {
		name = names[0]
	}
	ev := domain.DeviceEvent{GatewayID: gateway, ExternalID: ieee, DeviceName: name, EventType: domain.EventOffline, Detail: map[string]any{"source": source}, OccurredAt: now}
	if online {
		ev.EventType = domain.EventOnline
	}
	if e := insertEvent(tx, tenant, &ev); e != nil {
		return e
	}
	if r.opts.AlertsShadow {
		return nil
	}
	rules, e := loadRules(tx, true)
	if e != nil {
		return e
	}
	return createAlerts(tx, tenant, ev, rules)
}

// Z2MSilentAfter is how long a Zigbee2MQTT gateway may send nothing before its devices count as offline even
// though no last will arrived (the site host lost power or its network, and the broker has not noticed yet).
// Zigbee2MQTT 2.x publishes bridge/health every health.interval minutes (default 10, set explicitly in the
// configuration snippet), so 25 minutes tolerates one missed heartbeat.
const Z2MSilentAfter = 25 * time.Minute

// agentKind describes a site agent that publishes on behalf of devices whose liveness it reports: a Zigbee2MQTT
// bridge, or an Aether Edge. Both keep their own state row (the last will sets it offline) and a device table
// with the devices' own last availability; the logic below is the same for both.
type agentKind struct {
	table   string // the agent's state row: state, state_at, updated_at per gateway
	model   string // the gateway model
	devices string // the device table, with available per device
	key     string // the device table's id column (the stream's external_id)
	silent  time.Duration
}

var (
	z2mAgent  = agentKind{table: "core.z2m_bridges", model: domain.Z2MGatewayModel, devices: "core.z2m_devices", key: "ieee", silent: Z2MSilentAfter}
	edgeAgent = agentKind{table: "core.edge_agents", model: domain.EdgeGatewayModel, devices: "core.tuya_devices", key: "tuya_id", silent: EdgeSilentAfter}
)

// bridgeOffline records that the bridge itself is gone (its last will, or silence) and takes every device it
// reports for offline with it: nothing else would, because reported-liveness streams are not aged by silence.
func (r *Repository) bridgeOffline(tx *gorm.DB, tenant, gateway, source string, now time.Time) error {
	return r.agentOffline(tx, tenant, gateway, z2mAgent, source, now)
}

func (r *Repository) agentOffline(tx *gorm.DB, tenant, gateway string, k agentKind, source string, now time.Time) error {
	if e := tx.Exec(`INSERT INTO `+k.table+`(tenant_id,gateway_id,state,state_at,updated_at) VALUES(?,?,'offline',?,?)
    ON CONFLICT(tenant_id,gateway_id) DO UPDATE SET state='offline',state_at=EXCLUDED.state_at,updated_at=EXCLUDED.updated_at`, tenant, gateway, now, now).Error; e != nil {
		return e
	}
	var streams []struct{ ExternalID, Name string }
	if e := tx.Raw(`SELECT s.external_id,s.name FROM core.sensor_streams s WHERE s.gateway_id=? AND s.liveness='reported' ORDER BY s.external_id LIMIT 1000`, gateway).Scan(&streams).Error; e != nil {
		return e
	}
	for _, s := range streams {
		if e := r.setReportedLiveness(tx, tenant, gateway, s.ExternalID, s.Name, false, source, now); e != nil {
			return e
		}
	}
	return nil
}

// bridgeOnline runs for every message of a gateway whose bridge was recorded offline: the bridge is evidently
// back. Devices whose last own availability report was not "offline" are restored at once; the others wait for
// their availability report, which Zigbee2MQTT publishes (retained) again when it reconnects.
func (r *Repository) bridgeOnline(tx *gorm.DB, tenant, gateway string, now time.Time) error {
	return r.agentOnline(tx, tenant, gateway, z2mAgent, "bridge", now)
}

func (r *Repository) agentOnline(tx *gorm.DB, tenant, gateway string, k agentKind, source string, now time.Time) error {
	res := tx.Exec(`UPDATE `+k.table+` SET state='online',state_at=?,updated_at=? WHERE gateway_id=? AND state='offline'`, now, now, gateway)
	if res.Error != nil || res.RowsAffected == 0 {
		return res.Error
	}
	var streams []struct{ ExternalID, Name string }
	if e := tx.Raw(`SELECT s.external_id,s.name FROM core.sensor_streams s
    JOIN core.stream_state st ON st.tenant_id=s.tenant_id AND st.gateway_id=s.gateway_id AND st.external_id=s.external_id AND st.offline
    LEFT JOIN `+k.devices+` z ON z.gateway_id=s.gateway_id AND z.`+k.key+`=s.external_id
    WHERE s.gateway_id=? AND s.liveness='reported' AND z.available IS DISTINCT FROM false ORDER BY s.external_id LIMIT 1000`, gateway).Scan(&streams).Error; e != nil {
		return e
	}
	for _, s := range streams {
		if e := r.setReportedLiveness(tx, tenant, gateway, s.ExternalID, s.Name, true, source, now); e != nil {
			return e
		}
	}
	return nil
}

// scanSilentBridges is the part of ScanOffline for Zigbee2MQTT gateways that stopped sending anything without
// their last will reaching the broker. It runs whether or not any offline rule exists, because it keeps the
// device state honest (the UI reads it), exactly like an availability report does.
func (r *Repository) scanSilentBridges(tx *gorm.DB, tenant string, now time.Time) (int, error) {
	return r.scanSilentAgents(tx, tenant, z2mAgent, "bridge_silent", now)
}

func (r *Repository) scanSilentAgents(tx *gorm.DB, tenant string, k agentKind, source string, now time.Time) (int, error) {
	var gateways []string
	if e := tx.Raw(`SELECT g.id FROM core.gateways g
    WHERE g.model=? AND g.revoked_at IS NULL
      AND NOT EXISTS(SELECT 1 FROM `+k.table+` b WHERE b.gateway_id=g.id AND b.state='offline')
      AND EXISTS(SELECT 1 FROM core.sensor_streams s WHERE s.gateway_id=g.id AND s.liveness='reported')
      AND (SELECT max(p.received_at) FROM core.gateway_packets p WHERE p.gateway_id=g.id) BETWEEN ? AND ?
    ORDER BY g.id LIMIT 100`, k.model, now.Add(-7*24*time.Hour), now.Add(-k.silent)).Scan(&gateways).Error; e != nil {
		return 0, e
	}
	for _, g := range gateways {
		if e := r.agentOffline(tx, tenant, g, k, source, now); e != nil {
			return 0, e
		}
	}
	return len(gateways), nil
}

// z2mThinned decides whether a Zigbee2MQTT state message may be left out of the sample history, like Minew
// environment readings: SAMPLE_MIN_INTERVAL_SEC is set, the last sample of the device is younger than it, and
// nothing that carries state changed since that sample — no action, no confirmed command, and every safety flag,
// switch gang, settable property and enum value equal to the stored one. A metering plug reporting power every
// second keeps one sample per interval; a door opening is always stored. Events are evaluated either way.
func z2mThinned(tx *gorm.DB, tenant, gateway, ieee string, r minew.Reading, settable map[string]json.RawMessage, confirmed bool, now time.Time, opts Options) (bool, error) {
	if opts.SampleMinIntervalSec <= 0 || r.Action != "" || confirmed {
		return false, nil
	}
	var last []struct {
		Reading    json.RawMessage
		ReceivedAt time.Time
	}
	if e := tx.Raw(`SELECT reading,received_at FROM core.sensor_samples WHERE tenant_id=? AND gateway_id=? AND external_id=? ORDER BY received_at DESC LIMIT 1`, tenant, gateway, ieee).Scan(&last).Error; e != nil {
		return false, e
	}
	if len(last) == 0 || now.Sub(last[0].ReceivedAt) >= time.Duration(opts.SampleMinIntervalSec)*time.Second {
		return false, nil
	}
	var prev minew.Reading
	if json.Unmarshal(last[0].Reading, &prev) != nil {
		return false, nil
	}
	for name, v := range r.Metrics {
		if _, isSettable := settable[name]; !isSettable && !stateMetric(name) {
			continue
		}
		if before, ok := prev.Metrics[name]; !ok || before != v {
			return false, nil
		}
	}
	for k, v := range r.Values {
		if prev.Values[k] != v {
			return false, nil
		}
	}
	return true, nil
}

// stateMetric names the metrics whose change is an event (alarms, door, occupancy, gangs), never thinned away.
func stateMetric(name string) bool {
	switch name {
	case "door", "leak", "motion", "tamper", "sos", "smoke", "gas", "carbon_monoxide", "vibration", "battery_low", "state":
		return true
	}
	return strings.HasPrefix(name, "sw") && len(name) == 3
}
