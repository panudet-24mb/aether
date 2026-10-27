package milesight

import (
	"aether/backend/internal/domain"
	"encoding/base64"
	"encoding/json"
	"math"
	"strings"
	"unicode/utf8"
)

// Event is what Aether keeps of one uplink. Object is the decoded values: the gateway codec's output, or what decode.go
// made of the envelope's raw bytes. Raw and the radio figures are only known from an envelope (no codec).
type Event struct {
	DevEUI          string
	DeviceName      string
	ApplicationName string
	FPort           *int
	FCnt            *int64
	RSSI            *int
	SNR             *float64
	Raw             []byte
	Object          map[string]json.RawMessage
}

// envelope is the gateway's own uplink format, published when the device has no payload codec.
type envelope struct {
	DevEUI          string  `json:"devEUI"`
	DeviceName      string  `json:"deviceName"`
	ApplicationName string  `json:"applicationName"`
	FPort           *int    `json:"fPort"`
	FCnt            *int64  `json:"fCnt"`
	Data            *string `json:"data"`
	RxInfo          []struct {
		RSSI *int     `json:"rssi"`
		SNR  *float64 `json:"loRaSNR"`
	} `json:"rxInfo"`
}

// codecMeta are the fields a UG65 adds around a codec's output (seen on firmware 60.0.0.43: devEUI, deviceName,
// applicationID). They describe the uplink, not the device's readings.
var codecMeta = []string{"devEUI", "deviceName", "applicationID", "applicationName", "fPort", "fCnt", "time"}

// ParseUplink reads one uplink. A payload carrying `data` and `devEUI` is the gateway's envelope; its DevEUI must be
// the topic's. Anything else is a codec's output and is taken as the device's values, minus the metadata the gateway
// put next to them; a DevEUI among that metadata must be the topic's too.
func ParseUplink(m Message, b []byte) (Event, error) {
	var obj map[string]json.RawMessage
	if e := json.Unmarshal(b, &obj); e != nil || obj == nil {
		return Event{}, domain.ErrInvalid
	}
	ev := Event{DevEUI: m.DevEUI}
	if _, raw := obj["data"]; !raw || obj["devEUI"] == nil {
		var meta envelope
		_ = json.Unmarshal(b, &meta) // fields of another type than expected are left out, not fatal
		if eui := strings.ToLower(strings.TrimSpace(meta.DevEUI)); eui != "" && eui != m.DevEUI {
			return Event{}, domain.ErrInvalid
		}
		ev.DeviceName, ev.ApplicationName = clip(meta.DeviceName, 128), clip(meta.ApplicationName, 128)
		if meta.FPort != nil && *meta.FPort >= 0 && *meta.FPort <= 255 {
			ev.FPort = meta.FPort
		}
		if meta.FCnt != nil && *meta.FCnt >= 0 {
			ev.FCnt = meta.FCnt
		}
		for _, k := range codecMeta {
			delete(obj, k)
		}
		ev.Object = obj
		return ev, nil
	}
	var w envelope
	if e := json.Unmarshal(b, &w); e != nil || w.Data == nil || strings.ToLower(strings.TrimSpace(w.DevEUI)) != m.DevEUI {
		return Event{}, domain.ErrInvalid
	}
	raw, e := base64.StdEncoding.DecodeString(*w.Data)
	if e != nil || len(raw) > 242 { // 242 bytes is the largest LoRaWAN application payload
		return Event{}, domain.ErrInvalid
	}
	ev.Raw, ev.DeviceName, ev.ApplicationName = raw, clip(w.DeviceName, 128), clip(w.ApplicationName, 128)
	if w.FPort != nil && *w.FPort >= 0 && *w.FPort <= 255 {
		ev.FPort = w.FPort
	}
	if w.FCnt != nil && *w.FCnt >= 0 {
		ev.FCnt = w.FCnt
	}
	for _, rx := range w.RxInfo {
		if rx.RSSI != nil && *rx.RSSI >= -200 && *rx.RSSI <= 50 && (ev.RSSI == nil || *rx.RSSI > *ev.RSSI) {
			ev.RSSI, ev.SNR = rx.RSSI, nil
			if rx.SNR != nil && !math.IsNaN(*rx.SNR) && !math.IsInf(*rx.SNR, 0) && math.Abs(*rx.SNR) < 100 {
				ev.SNR = rx.SNR
			}
		}
	}
	return ev, nil
}

// Model is the catalog model of an uplink when nothing registered says so: a model named in the device or application
// name the installer gave it on the gateway, else the one whose fields a codec's output carries.
func Model(ev Event) string {
	for _, name := range []string{ev.DeviceName, ev.ApplicationName} {
		upper := strings.ToUpper(name)
		for _, p := range domain.DeviceProfiles {
			if p.Radio == "lorawan" && p.ID != domain.LoRaWANGenericProfile && strings.Contains(upper, strings.ToUpper(p.Model)) {
				return p.Model
			}
		}
	}
	has := func(keys ...string) bool {
		for _, k := range keys {
			if _, ok := ev.Object[k]; ok {
				return true
			}
		}
		return false
	}
	switch {
	case has("leakage_status"):
		return "WS303"
	case has("pir", "daylight"):
		return "WS202"
	case has("nh3", "h2s", "nh3_sensor_status", "h2s_sensor_status"):
		return "GS301"
	case has("occupancy", "calibration_status"):
		return "VS330"
	}
	return ""
}

func clip(s string, n int) string {
	s = strings.ToValidUTF8(strings.TrimSpace(s), "")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	for len(s) > n {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}
