package tuyacloud

import (
	"crypto/aes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// mqVectors are produced by tuya-connector-python 0.1.2's own code (testdata/gen_mq_vectors.py): the password and
// consumer URL by TuyaOpenPulsar, the ECB ciphertexts checked by its decryptor; GCM per tuya-pulsar-sdk-go.
type mqVectors struct {
	AccessID    string `json:"access_id"`
	Secret      string `json:"secret"`
	Password    string `json:"password"`
	ConsumerURL string `json:"consumer_url"`
	ECB         map[string]struct {
		Plaintext string `json:"plaintext"`
		Data      string `json:"data"`
	} `json:"ecb"`
	GCM map[string]struct {
		Plaintext string `json:"plaintext"`
		Data      string `json:"data"`
	} `json:"gcm"`
}

func vectors(t testing.TB) mqVectors {
	t.Helper()
	b, e := os.ReadFile("testdata/mq_vectors.json")
	if e != nil {
		t.Fatal(e)
	}
	var v mqVectors
	if e := json.Unmarshal(b, &v); e != nil {
		t.Fatal(e)
	}
	return v
}

func b64(t testing.TB, s string) []byte {
	t.Helper()
	b, e := base64.StdEncoding.DecodeString(s)
	if e != nil {
		t.Fatal(e)
	}
	return b
}

func TestPasswordAndURLMatchConnector(t *testing.T) {
	v := vectors(t)
	if got := Password(v.AccessID, v.Secret); got != v.Password || len(got) != 16 {
		t.Fatalf("password %q, connector %q", got, v.Password)
	}
	got, e := ConsumerURL("us", v.AccessID, ChannelProd)
	// The connector's endpoint constant ends in "/"; Aether's hosts do not and the path adds it.
	if e != nil || got != v.ConsumerURL {
		t.Fatalf("url %q (%v), connector %q", got, e, v.ConsumerURL)
	}
	test, _ := ConsumerURL("sg", v.AccessID, ChannelTest)
	if !strings.HasPrefix(test, "wss://mqe-sg.iotbing.com:8285/ws/v2/consumer/persistent/") || !strings.Contains(test, "/out/event-test/") {
		t.Fatalf("test channel url %q", test)
	}
	for _, c := range []struct{ region, id, channel string }{
		{"mars", v.AccessID, ChannelProd}, {"us", "short", ChannelProd}, {"us", "bad/../id00000", ChannelProd}, {"us", v.AccessID, "other"},
	} {
		if _, e := ConsumerURL(c.region, c.id, c.channel); e == nil {
			t.Fatalf("accepted %+v", c)
		}
	}
	for region := range MQHosts {
		if !strings.HasPrefix(MQHosts[region], "wss://") {
			t.Fatalf("%s is not TLS", region)
		}
	}
}

func TestDecryptMatchesReferences(t *testing.T) {
	v := vectors(t)
	for name, c := range v.ECB {
		for _, em := range []string{"", "aes_ecb"} {
			got, e := Decrypt(b64(t, c.Data), v.Secret, em)
			if e != nil || string(got) != c.Plaintext {
				t.Fatalf("ecb %s em=%q: %q %v", name, em, got, e)
			}
		}
	}
	for name, c := range v.GCM {
		got, e := Decrypt(b64(t, c.Data), v.Secret, "aes_gcm")
		if e != nil || string(got) != c.Plaintext {
			t.Fatalf("gcm %s: %q %v", name, got, e)
		}
		// Without the em property a GCM message fails; it is never misread as ECB.
		if _, e := Decrypt(b64(t, c.Data), v.Secret, ""); e == nil {
			t.Fatalf("gcm %s decrypted as ecb", name)
		}
	}
}

func ecbEncrypt(t *testing.T, key string, plain []byte) []byte {
	t.Helper()
	block, e := aes.NewCipher([]byte(key[8:24]))
	if e != nil {
		t.Fatal(e)
	}
	out := make([]byte, len(plain))
	for i := 0; i < len(plain); i += 16 {
		block.Encrypt(out[i:i+16], plain[i:i+16])
	}
	return out
}

func TestDecryptRejectsWithoutPanicking(t *testing.T) {
	v := vectors(t)
	gcm := b64(t, v.GCM["status4"].Data)
	tampered := append([]byte(nil), gcm...)
	tampered[len(tampered)-1] ^= 1
	bodyFlip := append([]byte(nil), gcm...)
	bodyFlip[20] ^= 1
	block := strings.Repeat("A", 15)
	cases := map[string]struct {
		data   []byte
		secret string
		em     string
	}{
		"wrong secret ecb": {b64(t, v.ECB["status4"].Data), "othersecret0000000000000000000000", ""},
		"wrong secret gcm": {gcm, "othersecret0000000000000000000000", "aes_gcm"},
		"short secret":     {b64(t, v.ECB["status4"].Data), "tooshort", ""},
		"tampered tag":     {tampered, v.Secret, "aes_gcm"},
		"tampered body":    {bodyFlip, v.Secret, "aes_gcm"},
		"short gcm":        {gcm[:27], v.Secret, "aes_gcm"},
		"ecb not blocks":   {make([]byte, 17), v.Secret, ""},
		"empty":            {nil, v.Secret, ""},
		"unknown mode":     {b64(t, v.ECB["status4"].Data), v.Secret, "aes_cbc"},
		"oversize":         {make([]byte, MaxData+16), v.Secret, ""},
		"zero padding":     {ecbEncrypt(t, v.Secret, []byte(block+"\x00")), v.Secret, ""},
		"padding too big":  {ecbEncrypt(t, v.Secret, []byte(block+"\x11")), v.Secret, ""},
		"inconsistent pad": {ecbEncrypt(t, v.Secret, []byte(strings.Repeat("A", 13)+"\x01\x03\x03")), v.Secret, ""},
	}
	for name, c := range cases {
		if out, e := Decrypt(c.data, c.secret, c.em); !errors.Is(e, ErrDecrypt) || out != nil {
			t.Fatalf("%s: %q %v", name, out, e)
		}
	}
	// A valid full padding block decrypts to the text before it.
	if out, e := Decrypt(ecbEncrypt(t, v.Secret, []byte(strings.Repeat("B", 16)+strings.Repeat("\x10", 16))), v.Secret, "aes_ecb"); e != nil || string(out) != strings.Repeat("B", 16) {
		t.Fatalf("full pad block: %q %v", out, e)
	}
}

// frame wraps an envelope the way Pulsar's WebSocket consumer delivers it.
func frame(t testing.TB, protocol any, data string, em string) []byte {
	t.Helper()
	env, _ := json.Marshal(map[string]any{"data": data, "protocol": protocol, "pv": "2.0", "sign": "d41d8cd98f00b204e9800998ecf8427e", "t": 1790000000999})
	f := map[string]any{"messageId": "CAAQAw==", "payload": base64.StdEncoding.EncodeToString(env), "publishTime": "2026-09-30T08:00:00.000Z", "redeliveryCount": 0}
	if em != "" {
		f["properties"] = map[string]string{"em": em}
	}
	b, _ := json.Marshal(f)
	return b
}

// The whole receive path on reference messages: frame, envelope, decryption, business message.
func TestFrameToEvents(t *testing.T) {
	v := vectors(t)
	for _, c := range []struct {
		name, data, em string
		protocol       any
	}{
		{"ecb", v.ECB["status4"].Data, "", 4},
		{"gcm", v.GCM["status4"].Data, "aes_gcm", "4"}, // protocol as a string is accepted too
	} {
		f, e := ParseFrame(frame(t, c.protocol, c.data, c.em))
		if e != nil || f.MessageID != "CAAQAw==" || f.Properties["em"] != c.em {
			t.Fatalf("%s frame: %+v %v", c.name, f, e)
		}
		env, e := ParseEnvelope(f.Payload)
		if e != nil || env.Protocol != 4 || env.PV != "2.0" || env.T != 1790000000999 {
			t.Fatalf("%s envelope: %+v %v", c.name, env, e)
		}
		plain, e := Decrypt(env.Data, v.Secret, f.Properties["em"])
		if e != nil {
			t.Fatalf("%s decrypt: %v", c.name, e)
		}
		events, e := Parse(env.Protocol, plain)
		if e != nil || len(events) != 1 || events[0].DevID != "bf1111111111111111aa01" || len(events[0].Items) != 2 {
			t.Fatalf("%s events: %+v %v", c.name, events, e)
		}
		if it := events[0].Items[1]; it.Code != "cur_power" || it.DPID != 19 || string(it.Value) != "1234" || it.T != 1790000000123 {
			t.Fatalf("%s item: %+v", c.name, it)
		}
	}
}

func TestFrameBounds(t *testing.T) {
	v := vectors(t)
	good := frame(t, 4, v.ECB["status4"].Data, "")
	nullProps := []byte(strings.Replace(string(good), `"messageId"`, `"properties":null,"messageId"`, 1))
	if _, e := ParseFrame(nullProps); e != nil {
		t.Fatalf("null properties: %v", e)
	}
	manyProps := map[string]string{}
	for i := 0; i <= maxProps; i++ {
		manyProps[strings.Repeat("k", i+1)] = "v"
	}
	mp, _ := json.Marshal(map[string]any{"messageId": "m1", "payload": base64.StdEncoding.EncodeToString([]byte("{}")), "properties": manyProps})
	bad := map[string][]byte{
		"not json":       []byte("{"),
		"no message id":  []byte(`{"payload":"e30="}`),
		"bad base64":     []byte(`{"messageId":"m1","payload":"!!!"}`),
		"empty payload":  []byte(`{"messageId":"m1","payload":""}`),
		"props not text": []byte(`{"messageId":"m1","payload":"e30=","properties":{"em":1}}`),
		"many props":     mp,
		"negative redel": []byte(`{"messageId":"m1","payload":"e30=","redeliveryCount":-1}`),
		"oversize":       append([]byte(`{"messageId":"m1","payload":"`), []byte(strings.Repeat("A", MaxFrame))...),
	}
	for name, b := range bad {
		if _, e := ParseFrame(b); !errors.Is(e, ErrFrame) {
			t.Fatalf("%s: %v", name, e)
		}
	}
	// The message id is still returned for a frame whose payload is bad, so the consumer can acknowledge it.
	if f, e := ParseFrame([]byte(`{"messageId":"m9","payload":"!!!"}`)); e == nil || f.MessageID != "m9" {
		t.Fatalf("message id on a bad frame: %+v %v", f, e)
	}
	for name, env := range map[string]string{
		"no data":        `{"protocol":4}`,
		"bad protocol":   `{"data":"QUFBQQ==","protocol":"x"}`,
		"negative proto": `{"data":"QUFBQQ==","protocol":-1}`,
		"bad data":       `{"data":"***","protocol":4}`,
		"long sign":      `{"data":"QUFBQQ==","protocol":4,"sign":"` + strings.Repeat("s", 300) + `"}`,
	} {
		if _, e := ParseEnvelope([]byte(env)); !errors.Is(e, ErrFrame) {
			t.Fatalf("%s: %v", name, e)
		}
	}
}

func TestAckMessages(t *testing.T) {
	if string(Ack(`id"1`)) != `{"messageId":"id\"1"}` {
		t.Fatalf("ack %s", Ack(`id"1`))
	}
	var n map[string]string
	if json.Unmarshal(Nack("CAAQAw=="), &n) != nil || n["type"] != "negativeAcknowledge" || n["messageId"] != "CAAQAw==" {
		t.Fatalf("nack %s", Nack("CAAQAw=="))
	}
}

// FuzzReceive runs arbitrary frames through the whole receive path: nothing may panic, and anything accepted must
// be bounded.
func FuzzReceive(f *testing.F) {
	v := vectors(f)
	f.Add(frame(f, 4, v.ECB["status4"].Data, ""), "")
	f.Add(frame(f, 20, v.ECB["online20"].Data, ""), "")
	f.Add(frame(f, 4, v.GCM["status4"].Data, "aes_gcm"), "aes_gcm")
	f.Add([]byte(`{"messageId":"m","payload":"e30="}`), "")
	f.Fuzz(func(t *testing.T, b []byte, em string) {
		fr, e := ParseFrame(b)
		if e != nil {
			return
		}
		env, e := ParseEnvelope(fr.Payload)
		if e != nil {
			return
		}
		plain, e := Decrypt(env.Data, v.Secret, em)
		if e != nil {
			return
		}
		events, e := Parse(env.Protocol, plain)
		if e != nil {
			return
		}
		for _, ev := range events {
			if len(ev.Items) > 128 {
				t.Fatalf("unbounded items: %d", len(ev.Items))
			}
		}
	})
}
