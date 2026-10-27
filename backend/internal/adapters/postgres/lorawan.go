package postgres

import (
	"aether/backend/internal/adapters/milesight"
	"aether/backend/internal/adapters/zigbee2mqtt"
	"aether/backend/internal/domain"
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// CaptureLoRaWAN stores one message a Milesight LoRaWAN gateway published under its tree. Like CaptureZ2M it runs in
// one transaction under the gateway's row lock, keeps a bounded diagnostic copy in core.gateway_packets, records the
// device for discovery and feeds the decoded values into the pipeline every definition-backed device shares, so a
// leak sensor raises leak events and a PIR drives occupancy exactly like their BLE and Zigbee counterparts.
func (r *Repository) CaptureLoRaWAN(ctx context.Context, tenant, gateway string, m milesight.Message, payload []byte) (string, error) {
	id := uuid.NewString()
	e := r.tx(ctx, "", tenant, func(tx *gorm.DB) error {
		var models []string
		if e := tx.Raw(`SELECT model FROM core.gateways WHERE id=? AND revoked_at IS NULL FOR UPDATE`, gateway).Scan(&models).Error; e != nil {
			return e
		}
		if len(models) != 1 {
			return domain.ErrUnauthorized
		}
		if models[0] != domain.LoRaWANGatewayModel {
			return domain.ErrForbidden // the ACL already confines the tree; this is the second lock on the door
		}
		diagnostic, _ := json.Marshal(map[string]any{"lora_topic": m.Topic, "payload": json.RawMessage(payload)})
		if e := tx.Exec(`INSERT INTO core.gateway_packets(id,tenant_id,gateway_id,payload) VALUES(?,?,?,?::jsonb)`, id, tenant, gateway, string(diagnostic)).Error; e != nil {
			return e
		}
		if m.Kind == milesight.Up {
			if e := r.saveLoRaWANUplink(tx, tenant, gateway, m, payload, time.Now().UTC()); e != nil {
				return e
			}
		}
		if e := signal(tx, tenant, "packet", gateway); e != nil {
			return e
		}
		return tx.Exec(`DELETE FROM core.gateway_packets WHERE gateway_id=? AND id IN (SELECT id FROM core.gateway_packets WHERE gateway_id=? ORDER BY received_at DESC,id DESC OFFSET 100)`, gateway, gateway).Error
	})
	return id, dataError(e)
}

// milesightDataPort is the LoRaWAN port Milesight devices send their readings on.
const milesightDataPort = 85

func (r *Repository) saveLoRaWANUplink(tx *gorm.DB, tenant, gateway string, m milesight.Message, payload []byte, now time.Time) error {
	ev, e := milesight.ParseUplink(m, payload)
	if e != nil {
		return e
	}
	model, e := loraModel(tx, gateway, ev)
	if e != nil {
		return e
	}
	// Without a codec on the gateway the envelope carries the raw bytes: decode the models Aether knows.
	if ev.Object == nil && ev.Raw != nil && (ev.FPort == nil || *ev.FPort == milesightDataPort) {
		ev.Object = milesight.Decode(model, ev.Raw)
	}
	properties, features := milesight.State(ev)
	if e := tx.Exec(`INSERT INTO core.lorawan_devices(tenant_id,gateway_id,dev_eui,name,application_name,model,f_port,f_cnt,rssi,snr,decoded,last_seen,updated_at)
    VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
    ON CONFLICT(tenant_id,gateway_id,dev_eui) DO UPDATE SET
      name=CASE WHEN EXCLUDED.name<>'' THEN EXCLUDED.name ELSE core.lorawan_devices.name END,
      application_name=CASE WHEN EXCLUDED.application_name<>'' THEN EXCLUDED.application_name ELSE core.lorawan_devices.application_name END,
      model=CASE WHEN EXCLUDED.model<>'' THEN EXCLUDED.model ELSE core.lorawan_devices.model END,
      f_port=coalesce(EXCLUDED.f_port,core.lorawan_devices.f_port),f_cnt=coalesce(EXCLUDED.f_cnt,core.lorawan_devices.f_cnt),
      rssi=coalesce(EXCLUDED.rssi,core.lorawan_devices.rssi),snr=coalesce(EXCLUDED.snr,core.lorawan_devices.snr),
      decoded=core.lorawan_devices.decoded OR EXCLUDED.decoded,last_seen=EXCLUDED.last_seen,updated_at=EXCLUDED.updated_at`,
		tenant, gateway, ev.DevEUI, ev.DeviceName, ev.ApplicationName, model, ev.FPort, ev.FCnt, ev.RSSI, ev.SNR, len(properties) > 0, now, now).Error; e != nil {
		return e
	}
	if len(properties) == 0 {
		return nil // an unknown model without a codec: the device is listed, there is nothing to plot
	}
	converted, e := json.Marshal(properties)
	if e != nil {
		return e
	}
	d := zigbee2mqtt.Device{IEEE: ev.DevEUI, FriendlyName: ev.DeviceName, Model: model}
	if d.FriendlyName == "" {
		d.FriendlyName = ev.DevEUI
	}
	if e := r.ingestDefinedState(tx, tenant, gateway, d, features, nil, converted, m.Topic, milesight.FrameUplink, false, now); e != nil {
		return e
	}
	// A stream first created before the gateway sent a name is named after its DevEUI; the name replaces it once known.
	if ev.DeviceName == "" {
		return nil
	}
	return tx.Exec(`UPDATE core.sensor_streams SET name=? WHERE tenant_id=? AND gateway_id=? AND external_id=? AND name=external_id`,
		ev.DeviceName, tenant, gateway, ev.DevEUI).Error
}

// loraModel is the catalog model of a device: the profile it is registered with wins, then the model an earlier uplink
// identified, then what this uplink shows (milesight.Model). Raw frames are only decoded for a known model.
func loraModel(tx *gorm.DB, gateway string, ev milesight.Event) (string, error) {
	var rows []struct{ ProfileID, Model string }
	if e := tx.Raw(`SELECT coalesce((SELECT d.profile_id FROM core.devices d WHERE d.gateway_id=? AND lower(d.external_id)=? AND d.removed_at IS NULL LIMIT 1),'') AS profile_id,
    coalesce((SELECT w.model FROM core.lorawan_devices w WHERE w.gateway_id=? AND w.dev_eui=?),'') AS model`, gateway, ev.DevEUI, gateway, ev.DevEUI).Scan(&rows).Error; e != nil {
		return "", e
	}
	if len(rows) == 1 {
		if p := domain.DeviceProfileByID(rows[0].ProfileID); p != nil && p.Radio == "lorawan" && p.ID != domain.LoRaWANGenericProfile {
			return p.Model, nil
		}
		if rows[0].Model != "" {
			return rows[0].Model, nil
		}
	}
	return milesight.Model(ev), nil
}
