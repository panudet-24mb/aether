package tuyacloud

// Tuya Message Service: the real-time side of Tuya Cloud mode. Tuya pushes device status and online/offline events
// for a cloud project into an Apache Pulsar topic; Aether consumes it through Pulsar's WebSocket consumer API, the
// transport Tuya's own Python connector uses. This file holds only the pure parts (URLs, credentials, frame and
// envelope parsing, decryption, ack messages); the connection loop lives in the worker.
//
// Sources, checked 2026-09-30:
//   - tuya-connector-python tuya_connector/openpulsar.py and tuya_enums.py (MIT, github HEAD 82487205, identical to
//     PyPI 0.1.2): endpoint, consumer path and query, headers username/password, password derivation, AES-ECB with
//     key secret[8:24], ack {"messageId": ...}.
//   - tuya-pulsar-sdk-go auth.go, pkg/tyutils/aes.go and example/main.go (MIT, HEAD 2506ea44): the same password
//     ("auth1" data), the "em" message property selecting aes_gcm (12-byte nonce, then ciphertext and tag) or
//     AES-ECB, and the binary endpoints of the other data centers.
//   - Apache Pulsar WebSocket API: frame fields and negative acknowledge.
//
// Every Tuya SDK disables TLS certificate verification; Aether does not. The mqe hosts present publicly trusted
// certificates.

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"unicode/utf8"
)

// MQHosts maps each data center to its Message Service WebSocket endpoint (port 8285). "us", "eu", "in" and "cn"
// are the endpoints tuya-connector-python names. "us-e", "eu-w" and "sg" are derived from the Go SDK's binary
// endpoints (pulsar+ssl://mqe-ueaz.tuyaus.com:7285 and friends): TLS answers on 8285 with a valid certificate,
// but the WebSocket path there is unverified until it is tried against a real project (MQVerified).
var MQHosts = map[string]string{
	"us":   "wss://mqe.tuyaus.com:8285",
	"eu":   "wss://mqe.tuyaeu.com:8285",
	"in":   "wss://mqe.tuyain.com:8285",
	"cn":   "wss://mqe.tuyacn.com:8285",
	"us-e": "wss://mqe-ueaz.tuyaus.com:8285",
	"eu-w": "wss://mqe-weaz.tuyaeu.com:8285",
	"sg":   "wss://mqe-sg.iotbing.com:8285",
}

// MQVerified reports which MQHosts entries are confirmed by a Tuya SDK; the UI can warn for the others.
var MQVerified = map[string]bool{"us": true, "eu": true, "in": true, "cn": true}

// Message Service topics: "event" carries every device of the project; "event-test" only the devices added as
// test devices in the Tuya console (tuya_enums.py TuyaCloudPulsarTopic).
const (
	ChannelProd = "event"
	ChannelTest = "event-test"
)

// Bounds on what the consumer accepts. A Tuya message is a few hundred bytes; these leave room for large status
// reports and stop anything else before it is decoded.
const (
	MaxFrame   = 256 << 10
	MaxPayload = 192 << 10
	MaxData    = 128 << 10
	maxProps   = 32
	maxPropLen = 256
	maxMsgID   = 256
)

var (
	// ErrFrame is a frame, envelope or event that is malformed or out of bounds: acknowledge it and drop it.
	ErrFrame = errors.New("tuya mq: malformed message")
	// ErrDecrypt is a message that does not decrypt with the project's secret: acknowledge it and drop it.
	ErrDecrypt = errors.New("tuya mq: message does not decrypt")
)

// ConsumerURL is the WebSocket URL of the project's subscription: the topic <access_id>/out/<channel>, the
// subscription <access_id>-sub, Failover, 3 s ack timeout (openpulsar.py __get_topic_url).
func ConsumerURL(region, accessID, channel string) (string, error) {
	host, ok := MQHosts[region]
	if !ok || !credentialPattern.MatchString(accessID) || (channel != ChannelProd && channel != ChannelTest) {
		return "", errors.New("tuya mq: invalid region, access id or channel")
	}
	id := url.PathEscape(accessID)
	return host + "/ws/v2/consumer/persistent/" + id + "/out/" + channel + "/" + id + "-sub?ackTimeoutMillis=3000&subscriptionType=Failover", nil
}

// Password is the Message Service password sent in the "password" header next to "username: <access_id>":
// md5hex(access_id + md5hex(secret))[8:24] (openpulsar.py __gen_pwd; the Go SDK's auth1 data is the same).
func Password(accessID, secret string) string {
	return md5Hex(accessID + md5Hex(secret))[8:24]
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// Frame is one message as Pulsar's WebSocket consumer delivers it.
type Frame struct {
	MessageID       string
	Payload         []byte // the envelope JSON, base64-decoded
	Properties      map[string]string
	PublishTime     string
	RedeliveryCount int
}

// ParseFrame reads one WebSocket frame. Anything oversize or malformed is ErrFrame; the caller acknowledges it
// by message id when one could be read (MessageID is filled in whenever it is valid).
func ParseFrame(b []byte) (Frame, error) {
	var f Frame
	if len(b) > MaxFrame {
		return f, ErrFrame
	}
	var raw struct {
		MessageID       string          `json:"messageId"`
		Payload         string          `json:"payload"`
		Properties      json.RawMessage `json:"properties"`
		PublishTime     string          `json:"publishTime"`
		RedeliveryCount int             `json:"redeliveryCount"`
	}
	if json.Unmarshal(b, &raw) != nil {
		return f, ErrFrame
	}
	if raw.MessageID == "" || len(raw.MessageID) > maxMsgID || !utf8.ValidString(raw.MessageID) {
		return f, ErrFrame
	}
	f.MessageID = raw.MessageID
	if len(raw.Payload) > base64.StdEncoding.EncodedLen(MaxPayload) {
		return f, ErrFrame
	}
	payload, e := base64.StdEncoding.DecodeString(raw.Payload)
	if e != nil || len(payload) == 0 || len(payload) > MaxPayload {
		return f, ErrFrame
	}
	f.Payload = payload
	f.Properties = map[string]string{}
	if len(raw.Properties) > 0 && !bytes.Equal(bytes.TrimSpace(raw.Properties), []byte("null")) {
		props := map[string]string{}
		if json.Unmarshal(raw.Properties, &props) != nil || len(props) > maxProps {
			return f, ErrFrame
		}
		for k, v := range props {
			if len(k) > maxPropLen || len(v) > maxPropLen {
				return f, ErrFrame
			}
			f.Properties[k] = v
		}
	}
	if len(raw.PublishTime) > 64 || raw.RedeliveryCount < 0 {
		return f, ErrFrame
	}
	f.PublishTime, f.RedeliveryCount = raw.PublishTime, raw.RedeliveryCount
	return f, nil
}

// Envelope is the message body Tuya publishes: the encrypted business data and its protocol number.
type Envelope struct {
	Data     []byte // ciphertext, base64-decoded
	Protocol int
	PV       string
	Sign     string
	T        int64
}

// ParseEnvelope reads {data, protocol, pv, sign, t} (Tuya "Message Types"). The sign field is kept but not checked:
// its algorithm is not documented well enough to verify (GCM messages are authenticated by their tag anyway).
func ParseEnvelope(payload []byte) (Envelope, error) {
	var env Envelope
	var raw struct {
		Data     string          `json:"data"`
		Protocol json.RawMessage `json:"protocol"`
		PV       json.RawMessage `json:"pv"`
		Sign     string          `json:"sign"`
		T        json.RawMessage `json:"t"`
	}
	if len(payload) > MaxPayload || json.Unmarshal(payload, &raw) != nil {
		return env, ErrFrame
	}
	protocol, ok := intField(raw.Protocol)
	if !ok || protocol < 0 || protocol > 1<<20 {
		return env, ErrFrame
	}
	if len(raw.Data) > base64.StdEncoding.EncodedLen(MaxData) {
		return env, ErrFrame
	}
	data, e := base64.StdEncoding.DecodeString(raw.Data)
	if e != nil || len(data) == 0 || len(data) > MaxData {
		return env, ErrFrame
	}
	env.Data, env.Protocol = data, int(protocol)
	env.PV = textField(raw.PV)
	if len(env.PV) > 16 || len(raw.Sign) > 256 {
		return env, ErrFrame
	}
	env.Sign = raw.Sign
	if t, ok := intField(raw.T); ok {
		env.T = t
	}
	return env, nil
}

// intField reads a JSON number or a numeric string (Tuya is not consistent between protocols).
func intField(raw json.RawMessage) (int64, bool) {
	var n json.Number
	if len(raw) == 0 {
		return 0, false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		n = json.Number(s)
	} else if json.Unmarshal(raw, &n) != nil {
		return 0, false
	}
	v, e := strconv.ParseInt(string(n), 10, 64)
	return v, e == nil
}

func textField(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return string(n)
	}
	return ""
}

// Decrypt opens a message's data with the project's secret. The key is secret[8:24]. The "em" message property
// selects the mode: "aes_gcm" is a 12-byte nonce, then the ciphertext and its 16-byte tag; anything else (absent,
// "aes_ecb") is AES-128-ECB with PKCS#7 padding, which is checked strictly (the Go SDK's unpadding panics on bad
// input; this never panics). Whether Pulsar's WebSocket proxy passes "em" through is unverified; without it a GCM
// message fails as ErrDecrypt rather than being misread.
func Decrypt(data []byte, secret, em string) ([]byte, error) {
	if len(secret) < 24 || len(data) == 0 || len(data) > MaxData {
		return nil, ErrDecrypt
	}
	block, e := aes.NewCipher([]byte(secret[8:24]))
	if e != nil {
		return nil, ErrDecrypt
	}
	switch em {
	case "aes_gcm":
		gcm, e := cipher.NewGCMWithNonceSize(block, 12)
		if e != nil || len(data) < 12+gcm.Overhead() {
			return nil, ErrDecrypt
		}
		out, e := gcm.Open(nil, data[:12], data[12:], nil)
		if e != nil {
			return nil, ErrDecrypt
		}
		return out, nil
	case "", "aes_ecb":
		size := block.BlockSize()
		if len(data)%size != 0 {
			return nil, ErrDecrypt
		}
		out := make([]byte, len(data))
		for i := 0; i < len(data); i += size {
			block.Decrypt(out[i:i+size], data[i:i+size])
		}
		n := int(out[len(out)-1])
		if n < 1 || n > size {
			return nil, ErrDecrypt
		}
		for _, b := range out[len(out)-n:] {
			if int(b) != n {
				return nil, ErrDecrypt
			}
		}
		return out[:len(out)-n], nil
	}
	return nil, ErrDecrypt
}

// Ack is the WebSocket message acknowledging one message (openpulsar.py __send_ack).
func Ack(messageID string) []byte {
	b, _ := json.Marshal(map[string]string{"messageId": messageID})
	return b
}

// Nack asks Pulsar to redeliver one message later (Pulsar WebSocket API negativeAcknowledge): used when Aether
// could not store it for a transient reason, never for a malformed message.
func Nack(messageID string) []byte {
	b, _ := json.Marshal(map[string]string{"type": "negativeAcknowledge", "messageId": messageID})
	return b
}
