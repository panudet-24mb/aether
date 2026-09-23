package minew

import (
	"aether/backend/internal/domain"
	"encoding/json"
	"testing"
	"time"
)

// Independent vector: the example MG3 packet published in reelyactive/barnowl-minew README
// (device fee150bada55, acceleration frame, battery 74%). Decoded by hand, not by their code.
func TestAccelerationVectorFromPublishedExample(t *testing.T) {
	r, ok := DecodeFrames("0201060303e1ff1216e1ffa1034affe7004500fa55daba50e1fe")
	if !ok || r.Kind != KindMotion || r.Battery != 74 {
		t.Fatalf("accel frame: %+v %v", r, ok)
	}
	if r.Metrics["accel_x"] != -25.0/256 || r.Metrics["accel_y"] != 69.0/256 || r.Metrics["accel_z"] != 250.0/256 {
		t.Fatalf("accel values: %+v", r.Metrics)
	}
	if len(r.Frames) != 1 || r.Frames[0] != FrameAccel {
		t.Fatalf("frames: %v", r.Frames)
	}
}

func TestBeaconFrames(t *testing.T) {
	ib, ok := DecodeFrames("1aff4c000215e2c56db5dffb48d2b060d0f5a71096e000010002c5")
	if !ok || ib.Kind != KindBeacon || ib.Beacon == nil || ib.Beacon.Type != "ibeacon" || ib.Beacon.UUID != "e2c56db5-dffb-48d2-b060-d0f5a71096e0" || ib.Beacon.Major != 1 || ib.Beacon.Minor != 2 || ib.Beacon.TxPower != -59 {
		t.Fatalf("ibeacon: %+v %v", ib, ok)
	}
	uid, ok := DecodeFrames("1516aafe00e700112233445566778899000000000001")
	if !ok || uid.Beacon == nil || uid.Beacon.Type != "eddystone_uid" || uid.Beacon.Namespace != "00112233445566778899" || uid.Beacon.Instance != "000000000001" || uid.Beacon.TxPower != -25 {
		t.Fatalf("eddystone uid: %+v %v", uid, ok)
	}
	tlm, ok := DecodeFrames("1116aafe20000bb81a800000001000000064")
	if !ok || tlm.Beacon == nil || tlm.Beacon.Type != "eddystone_tlm" || tlm.Beacon.Voltage != 3 || tlm.Beacon.AdvCount != 16 || tlm.Beacon.Uptime != 10 || tlm.Metrics["tlm_temperature"] != 26.5 {
		t.Fatalf("eddystone tlm: %+v %v", tlm, ok)
	}
	// UID + TLM in one advertisement keep the identity and gain telemetry.
	both, ok := DecodeFrames("1516aafe00e7001122334455667788990000000000011116aafe20000bb81a800000001000000064")
	if !ok || both.Beacon.Type != "eddystone_uid" || both.Beacon.Instance != "000000000001" || both.Beacon.Voltage != 3 || len(both.Frames) != 2 {
		t.Fatalf("uid+tlm: %+v %v", both, ok)
	}
}

func TestEventFramesAndInfo(t *testing.T) {
	tamper, ok := DecodeFrames("0d16e1ffa1205a01aabbccddeeff")
	if !ok || tamper.Kind != KindTamper || tamper.Metrics["tamper"] != 1 || tamper.Battery != 90 {
		t.Fatalf("tamper: %+v %v", tamper, ok)
	}
	info, ok := DecodeFrames("0e16e1ffa10864aabbccddeeff5331")
	if !ok || info.Kind != KindInfo || info.Model != "S1" || info.Battery != 100 {
		t.Fatalf("info: %+v %v", info, ok)
	}
	vib, ok := DecodeFrames("1116e1ffa1185500000010 01aabbccddeeff")
	if ok || vib.Kind != "" {
		t.Fatal("hex with whitespace must be rejected")
	}
	vib, ok = DecodeFrames("1116e1ffa118550000001001aabbccddeeff")
	if !ok || vib.Kind != KindMotion || vib.Metrics["vibration"] != 1 || vib.Metrics["vibration_ts"] != 16 {
		t.Fatalf("vibration: %+v %v", vib, ok)
	}
	for _, bad := range []string{"0d16e1ffa120ff01aabbccddeeff", "0c16e1ffa1205a01aabbccddee", "19ff4c000215e2c56db5dffb48d2b060d0f5a71096e000010002", "1416aafe00e7001122334455667788990000000000", "0e16e1ffa10864aabbccddeeff53ff"} {
		if _, ok := DecodeFrames(bad); ok {
			t.Fatalf("invalid frame accepted: %s", bad)
		}
	}
}

func TestProjectionMergesSlotsPerUplink(t *testing.T) {
	now := time.Now().UTC()
	// One tag reports an iBeacon slot and an acceleration slot in the same uplink, plus an info frame.
	body := json.RawMessage(`[{"type":"Gateway","mac":"111111111111"},
	 {"mac":"F00000000005","rawData":"1aff4c000215e2c56db5dffb48d2b060d0f5a71096e000010005c5","rssi":-50},
	 {"mac":"F00000000005","rawData":"1216e1ffa1034affe7004500fa55daba50e1fe","rssi":-51},
	 {"mac":"F00000000005","rawData":"0f16e1ffa10864aabbccddeeff433130","rssi":-51},
	 {"mac":"AABBCCDDEEFF","rawData":"1016e1ffa101641c8a2f51000000000000","rssi":-62}]`)
	v := Project(domain.Gateway{}, []domain.Packet{{ReceivedAt: now, Payload: body}})
	if len(v.Sensors) != 2 || v.NearbyDevices != 2 {
		t.Fatalf("sensors: %+v", v.Sensors)
	}
	var tag, env *Sensor
	for i := range v.Sensors {
		if v.Sensors[i].ID == "f00000000005" {
			tag = &v.Sensors[i]
		} else {
			env = &v.Sensors[i]
		}
	}
	if tag == nil || env == nil {
		t.Fatal("missing sensors")
	}
	if tag.Kind != KindMotion || tag.Model != "C10" || tag.Name != "Minew C10" || tag.Latest.Beacon == nil || tag.Latest.Beacon.Minor != 5 || tag.Latest.Metrics["accel_z"] == 0 || len(tag.Latest.Frames) != 3 || *tag.Latest.RSSI != -51 {
		t.Fatalf("merged tag: %+v", tag.Latest)
	}
	if env.Kind != KindEnvironment || env.Latest.Temperature != 28.5390625 || env.Name != "Minew temperature & humidity" {
		t.Fatalf("environment: %+v", env)
	}
}
