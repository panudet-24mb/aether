package tuyable

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"
)

// The vectors in testdata/vectors.json come from the ha_tuya_ble reference code; see testdata/README.md.
type vectors struct {
	CRC16 []struct {
		Data string
		CRC  uint16
	}
	Varint []struct {
		Value uint32
		Bytes string
	}
	Keys []struct {
		LocalKey        string `json:"local_key"`
		SecKey          string `json:"sec_key"`
		PairingLoginKey string `json:"pairing_login_key"`
		LoginKey        string `json:"login_key"`
		Srand           string
		SessionKey      string `json:"session_key"`
		LoginFlag       byte   `json:"login_flag"`
		SessionFlag     byte   `json:"session_flag"`
	}
	Frames []struct {
		Name       string
		Seq        uint32
		ResponseTo uint32 `json:"response_to"`
		Code       Code
		Data       string
		Key        string
		Flag       byte
		Protocol   byte
		IV         string
		Plaintext  string
		Fragments  []string
	}
	PairingRequest struct {
		UUID     string
		Login6   string
		DeviceID string `json:"device_id"`
		Payload  string
	} `json:"pairing_request"`
	DPs []struct {
		LengthSize int `json:"length_size"`
		Encoded    string
		Decoded    []jsonDP
	}
	Receive []struct {
		Code       Code
		Data       string
		LengthSize int `json:"length_size"`
		Ack        *string
		Time       *float64
		Decoded    []jsonDP
	}
	Time struct {
		UnixMS int64 `json:"unix_ms"`
		Time1  string
		Time2  string
	}
	DeviceInfo struct {
		Data            string
		DeviceVersion   string `json:"device_version"`
		ProtocolVersion string `json:"protocol_version"`
		Protocol        byte
		Flags           byte
		Bound           bool
		Srand           string
		HardwareVersion string `json:"hardware_version"`
		AuthKey         string `json:"auth_key"`
	} `json:"device_info"`
	Adverts []struct {
		ServiceData      string `json:"service_data"`
		ManufacturerData string `json:"manufacturer_data"`
		ProductID        string `json:"product_id"`
		UUID             string
		Bound            bool
		Protocol         byte
	}
}

type jsonDP struct {
	ID    byte
	Type  DPType
	Value any
}

// dp converts a reference DP (raw and bitmap as hex strings, numbers as floats) into the package's form.
func (j jsonDP) dp(t *testing.T) DP {
	d := DP{ID: j.ID, Type: j.Type}
	switch j.Type {
	case DPRaw, DPBitmap:
		d.Value = unhex(t, j.Value.(string))
	case DPValue, DPEnum:
		d.Value = int64(j.Value.(float64))
	default:
		d.Value = j.Value
	}
	return d
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func load(t *testing.T) vectors {
	t.Helper()
	b, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestVectorsCRCAndVarint(t *testing.T) {
	v := load(t)
	for _, c := range v.CRC16 {
		if got := CRC16(unhex(t, c.Data)); got != c.CRC {
			t.Errorf("CRC16(%s) = %#04x, want %#04x", c.Data, got, c.CRC)
		}
	}
	for _, c := range v.Varint {
		enc := AppendVarint(nil, c.Value)
		if hex.EncodeToString(enc) != c.Bytes {
			t.Errorf("varint(%d) = %x, want %s", c.Value, enc, c.Bytes)
		}
		got, pos, err := Varint(enc, 0)
		if err != nil || got != c.Value || pos != len(enc) {
			t.Errorf("Varint(%x) = %d, %d, %v", enc, got, pos, err)
		}
	}
}

func TestVectorsKeys(t *testing.T) {
	v := load(t)
	for _, c := range v.Keys {
		k, err := NewKeys(c.LocalKey, c.SecKey)
		if err != nil {
			t.Fatal(err)
		}
		srand := unhex(t, c.Srand)
		for name, pair := range map[string][2]string{
			"pairing": {hex.EncodeToString(k.PairingKey()), c.PairingLoginKey},
			"login":   {hex.EncodeToString(k.LoginKey()), c.LoginKey},
			"session": {hex.EncodeToString(k.SessionKey(srand)), c.SessionKey},
		} {
			if pair[0] != pair[1] {
				t.Errorf("%s key for %q/%q = %s, want %s", name, c.LocalKey, c.SecKey, pair[0], pair[1])
			}
		}
		if k.LoginFlag() != c.LoginFlag || k.SessionFlag() != c.SessionFlag {
			t.Errorf("flags for %q/%q = %d/%d", c.LocalKey, c.SecKey, k.LoginFlag(), k.SessionFlag())
		}
	}
}

func TestVectorsFrames(t *testing.T) {
	v := load(t)
	for _, c := range v.Frames {
		t.Run(c.Name, func(t *testing.T) {
			f := Frame{Seq: c.Seq, ResponseTo: c.ResponseTo, Code: c.Code, Data: unhex(t, c.Data)}
			raw, err := Plaintext(f)
			if err != nil || hex.EncodeToString(raw) != c.Plaintext {
				t.Fatalf("plaintext %x, %v; want %s", raw, err, c.Plaintext)
			}
			key := unhex(t, c.Key)
			msg, err := Seal(f, key, c.Flag, unhex(t, c.IV))
			if err != nil {
				t.Fatal(err)
			}
			frags := Fragment(msg, c.Protocol, GATTMTU)
			if len(frags) != len(c.Fragments) {
				t.Fatalf("%d fragments, want %d", len(frags), len(c.Fragments))
			}
			var r Reassembler
			var joined []byte
			for i, p := range frags {
				if hex.EncodeToString(p) != c.Fragments[i] {
					t.Fatalf("fragment %d = %x, want %s", i, p, c.Fragments[i])
				}
				// Reassemble the reference's fragments, not ours.
				out, err := r.Push(unhex(t, c.Fragments[i]))
				if err != nil {
					t.Fatal(err)
				}
				if out != nil {
					joined = out
				}
			}
			got, flag, err := Unseal(joined, func(b byte) []byte {
				if b == c.Flag {
					return key
				}
				return nil
			})
			if err != nil || flag != c.Flag || !reflect.DeepEqual(got, Frame{Seq: f.Seq, ResponseTo: f.ResponseTo, Code: f.Code, Data: nonNil(f.Data)}) {
				t.Fatalf("Unseal = %+v, %d, %v", got, flag, err)
			}
		})
	}
}

func nonNil(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}

func TestVectorsPairRequest(t *testing.T) {
	v := load(t).PairingRequest
	k := Keys{LocalKey: string(unhex(t, v.Login6)) + "0000000000"}
	got, err := PairRequest(v.UUID, k, v.DeviceID)
	if err != nil || hex.EncodeToString(got) != v.Payload {
		t.Fatalf("PairRequest = %x, %v; want %s", got, err, v.Payload)
	}
}

func TestVectorsDPs(t *testing.T) {
	v := load(t)
	for _, c := range v.DPs {
		var want []DP
		for _, j := range c.Decoded {
			want = append(want, j.dp(t))
		}
		enc, err := EncodeDPs(want, c.LengthSize)
		if err != nil || hex.EncodeToString(enc) != c.Encoded {
			t.Errorf("EncodeDPs(%v, %d) = %x, %v; want %s", want, c.LengthSize, enc, err, c.Encoded)
		}
		got, end, err := ParseDPs(unhex(t, c.Encoded), 0, c.LengthSize)
		if err != nil || end != len(c.Encoded)/2 || !reflect.DeepEqual(got, want) {
			t.Errorf("ParseDPs(%s) = %v, %d, %v; want %v", c.Encoded, got, end, err, want)
		}
	}
}

func TestVectorsReceive(t *testing.T) {
	v := load(t)
	now := time.Unix(1, 0)
	for _, c := range v.Receive {
		rep, ack, err := parseReport(Frame{Seq: 7, Code: c.Code, Data: unhex(t, c.Data)}, now)
		if err != nil {
			t.Fatalf("%s: %v", c.Code, err)
		}
		var want []DP
		for _, j := range c.Decoded {
			want = append(want, j.dp(t))
		}
		if !reflect.DeepEqual(rep.DPs, want) {
			t.Errorf("%s DPs = %v, want %v", c.Code, rep.DPs, want)
		}
		switch {
		case c.Ack == nil && ack != nil:
			t.Errorf("%s acked with %x, want no ack", c.Code, ack)
		case c.Ack != nil && (ack == nil || hex.EncodeToString(ack) != *c.Ack):
			t.Errorf("%s ack = %x, want %s", c.Code, ack, *c.Ack)
		}
		if c.Time != nil {
			if !rep.Timestamped || rep.Time.UnixMilli() != int64(*c.Time*1000+0.5) {
				t.Errorf("%s time = %v, want %v", c.Code, rep.Time, *c.Time)
			}
		} else if rep.Timestamped || !rep.Time.Equal(now) {
			t.Errorf("%s time = %v, want receive time", c.Code, rep.Time)
		}
	}
}

func TestVectorsTime(t *testing.T) {
	v := load(t).Time
	now := time.UnixMilli(v.UnixMS).In(time.FixedZone("ICT", 7*3600))
	if got := hex.EncodeToString(Time1Answer(now)); got != v.Time1 {
		t.Errorf("TIME1 = %s, want %s", got, v.Time1)
	}
	if got := hex.EncodeToString(Time2Answer(now)); got != v.Time2 {
		t.Errorf("TIME2 = %s, want %s", got, v.Time2)
	}
}

func TestVectorsDeviceInfo(t *testing.T) {
	v := load(t).DeviceInfo
	info, srand, auth, err := ParseDeviceInfo(unhex(t, v.Data))
	if err != nil {
		t.Fatal(err)
	}
	want := DeviceInfo{DeviceVersion: v.DeviceVersion, ProtocolVersion: v.ProtocolVersion, HardwareVersion: v.HardwareVersion,
		Protocol: v.Protocol, Flags: v.Flags, Bound: v.Bound}
	if info != want || hex.EncodeToString(srand) != v.Srand || hex.EncodeToString(auth) != v.AuthKey {
		t.Fatalf("ParseDeviceInfo = %+v %x %x", info, srand, auth)
	}
	if _, _, _, err := ParseDeviceInfo(unhex(t, v.Data)[:45]); err == nil {
		t.Fatal("short device info accepted")
	}
}

func TestVectorsAdverts(t *testing.T) {
	for _, c := range load(t).Adverts {
		sd, md := unhex(t, c.ServiceData), unhex(t, c.ManufacturerData)
		a, err := ParseAdvert(sd, md)
		want := Advert{ProductID: c.ProductID, UUID: c.UUID, Bound: c.Bound, Protocol: c.Protocol}
		if err != nil || a != want {
			t.Fatalf("ParseAdvert = %+v, %v; want %+v", a, err, want)
		}
		gotSD, gotMD := EncodeAdvert(want)
		if !bytes.Equal(gotSD, sd) || !bytes.Equal(gotMD, md) {
			t.Fatalf("EncodeAdvert = %x %x", gotSD, gotMD)
		}
		// Without the product id the uuid stays unknown.
		if a, err := ParseAdvert(nil, md); err != nil || a.UUID != "" || a.Protocol != c.Protocol {
			t.Fatalf("no service data: %+v, %v", a, err)
		}
		// With the wrong product id the uuid does not decrypt to text.
		if _, err := ParseAdvert(append([]byte{0}, "wrongpid"...), md); err == nil {
			t.Fatal("wrong product id accepted")
		}
	}
}
