package tuyalocal

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The vectors were produced once by testdata/gen_vectors.py with tinytuya 1.20.0 and fixed keys, nonces,
// clock and IVs. Client frames must match tinytuya's encoder byte for byte; device frames (built with
// tinytuya's pack_message and decoded by tinytuya when generated) must decode to the same DPs here.

type frameVec struct {
	Seq     uint32 `json:"seq"`
	Cmd     uint32 `json:"cmd"`
	Payload string `json:"payload"`
	Frame   string `json:"frame"`
}

type deviceVec struct {
	Seq   uint32         `json:"seq"`
	Cmd   uint32         `json:"cmd"`
	JSON  string         `json:"json"`
	DPS   map[string]any `json:"dps"`
	Frame string         `json:"frame"`
}

type vectorFile struct {
	Tinytuya    string   `json:"tinytuya"`
	Version     string   `json:"version"`
	DeviceID    string   `json:"device_id"`
	LocalKey    string   `json:"local_key"`
	Now         int64    `json:"now"`
	ClientIV    string   `json:"client_iv"`
	DeviceIV    string   `json:"device_iv"`
	DPQuery     frameVec `json:"dp_query"`
	Control     frameVec `json:"control"`
	Heartbeat   frameVec `json:"heartbeat"`
	Negotiation *struct {
		LocalNonce  string   `json:"local_nonce"`
		RemoteNonce string   `json:"remote_nonce"`
		Start       frameVec `json:"start"`
		Resp        frameVec `json:"resp"`
		Finish      frameVec `json:"finish"`
		SessionKey  string   `json:"session_key"`
	} `json:"negotiation"`
	StatusPush *deviceVec `json:"status_push"`
	QueryReply *deviceVec `json:"query_reply"`
	Broadcast  struct {
		JSON  map[string]any `json:"json"`
		Frame string         `json:"frame"`
	} `json:"broadcast"`
}

func readVectors(name string) (vectorFile, error) {
	var v vectorFile
	raw, e := os.ReadFile(filepath.Join("testdata", "vectors_"+name+".json"))
	if e != nil {
		return v, e
	}
	if e := json.Unmarshal(raw, &v); e != nil {
		return v, e
	}
	if v.Tinytuya != "1.20.0" {
		return v, fmt.Errorf("vectors from tinytuya %s, expected 1.20.0", v.Tinytuya)
	}
	return v, nil
}

func loadVectors(t *testing.T, name string) vectorFile {
	t.Helper()
	v, e := readVectors(name)
	if e != nil {
		t.Fatal(e)
	}
	return v
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, e := hex.DecodeString(s)
	if e != nil {
		t.Fatal(e)
	}
	return b
}

// clientSession is a client session with the vector's pinned clock and IV.
func clientSession(t *testing.T, v vectorFile) *Session {
	t.Helper()
	ver, e := ParseVersion(v.Version)
	if e != nil {
		t.Fatal(e)
	}
	s, e := NewSession(ver, v.DeviceID, v.LocalKey, Client)
	if e != nil {
		t.Fatal(e)
	}
	s.Now = func() time.Time { return time.Unix(v.Now, 0) }
	s.IV = func() []byte { return []byte(v.ClientIV) }
	return s
}

func checkClientFrame(t *testing.T, s *Session, name string, want frameVec, cmd uint32, payload []byte) {
	t.Helper()
	if cmd != want.Cmd {
		t.Fatalf("%s: command %#x, tinytuya %#x", name, cmd, want.Cmd)
	}
	if string(payload) != want.Payload {
		t.Fatalf("%s: payload\n got %s\nwant %s", name, payload, want.Payload)
	}
	s.SetSeq(want.Seq)
	got, e := s.Encode(cmd, payload)
	if e != nil {
		t.Fatal(e)
	}
	if hex.EncodeToString(got) != want.Frame {
		t.Fatalf("%s: frame\n got %x\nwant %s", name, got, want.Frame)
	}
}

// sameDPS compares DP maps through JSON, since ours hold json.Number and the vectors float64.
func sameDPS(t *testing.T, got, want map[string]any) {
	t.Helper()
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	var gm, wm map[string]any
	_ = json.Unmarshal(g, &gm)
	_ = json.Unmarshal(w, &wm)
	gj, _ := json.Marshal(gm)
	wj, _ := json.Marshal(wm)
	if !bytes.Equal(gj, wj) {
		t.Fatalf("dps %s, want %s", gj, wj)
	}
}

func decodeDevice(t *testing.T, s *Session, frame []byte) map[string]any {
	t.Helper()
	f, n, e := s.Unpack(frame)
	if e != nil || n != len(frame) {
		t.Fatalf("unpack: %v (consumed %d of %d)", e, n, len(frame))
	}
	p, e := s.Decode(f)
	if e != nil {
		t.Fatalf("decode: %v", e)
	}
	dps, ok, e := ParseDPS(p)
	if e != nil || !ok {
		t.Fatalf("dps: %v %v (%s)", ok, e, p)
	}
	return dps
}

func TestVectorsClientFrames(t *testing.T) {
	for _, name := range []string{"33", "34", "35"} {
		t.Run(name, func(t *testing.T) {
			v := loadVectors(t, name)
			s := clientSession(t, v)
			if n := v.Negotiation; n != nil {
				// START carries the raw local nonce, framed with the real key.
				checkClientFrame(t, s, "start", frameVec{Seq: n.Start.Seq, Cmd: CmdSessKeyNegStart, Payload: n.LocalNonce, Frame: n.Start.Frame},
					CmdSessKeyNegStart, []byte(n.LocalNonce))
				f, _, e := s.Unpack(unhex(t, n.Resp.Frame))
				if e != nil || f.Cmd != CmdSessKeyNegResp {
					t.Fatalf("resp: %v %#x", e, f.Cmd)
				}
				resp, e := s.Decode(f)
				if e != nil {
					t.Fatal(e)
				}
				neg := &negotiation{version: s.Version, localKey: []byte(v.LocalKey), localNonce: []byte(n.LocalNonce)}
				finish, e := neg.finishPayload(resp)
				if e != nil {
					t.Fatal(e)
				}
				if string(neg.remoteNonce) != n.RemoteNonce {
					t.Fatalf("remote nonce %q", neg.remoteNonce)
				}
				s.SetSeq(n.Finish.Seq)
				got, e := s.Encode(CmdSessKeyNegFinish, finish)
				if e != nil || hex.EncodeToString(got) != n.Finish.Frame {
					t.Fatalf("finish frame\n got %x\nwant %s (%v)", got, n.Finish.Frame, e)
				}
				key, e := neg.sessionKey()
				if e != nil || hex.EncodeToString(key) != n.SessionKey {
					t.Fatalf("session key %x, want %s (%v)", key, n.SessionKey, e)
				}
				s.SetSessionKey(key)
			}
			cmd, p := s.DPQuery()
			checkClientFrame(t, s, "dp_query", v.DPQuery, cmd, p)
			cmd, p = s.Control(map[string]any{"1": true, "2": 50})
			checkClientFrame(t, s, "control", v.Control, cmd, p)
			cmd, p = s.Heartbeat()
			checkClientFrame(t, s, "heartbeat", v.Heartbeat, cmd, p)

			sameDPS(t, decodeDevice(t, s, unhex(t, v.StatusPush.Frame)), v.StatusPush.DPS)
			sameDPS(t, decodeDevice(t, s, unhex(t, v.QueryReply.Frame)), v.QueryReply.DPS)
		})
	}
}

// The simulator's device-side encoder must produce the same frames tinytuya decodes.
func TestVectorsDeviceFrames(t *testing.T) {
	for _, name := range []string{"33", "34", "35"} {
		t.Run(name, func(t *testing.T) {
			v := loadVectors(t, name)
			ver, _ := ParseVersion(v.Version)
			d, e := NewSession(ver, v.DeviceID, v.LocalKey, Device)
			if e != nil {
				t.Fatal(e)
			}
			d.IV = func() []byte { return []byte(v.DeviceIV) }
			if n := v.Negotiation; n != nil {
				resp := NegotiationResponse([]byte(v.LocalKey), []byte(n.LocalNonce), []byte(n.RemoteNonce))
				d.SetSeq(n.Resp.Seq)
				got, e := d.Encode(CmdSessKeyNegResp, resp)
				if e != nil || hex.EncodeToString(got) != n.Resp.Frame {
					t.Fatalf("resp frame\n got %x\nwant %s (%v)", got, n.Resp.Frame, e)
				}
				d.SetSessionKey(unhex(t, n.SessionKey))
			}
			d.SetSeq(v.StatusPush.Seq)
			got, e := d.Encode(CmdStatus, []byte(v.StatusPush.JSON))
			if e != nil || hex.EncodeToString(got) != v.StatusPush.Frame {
				t.Fatalf("push frame\n got %x\nwant %s (%v)", got, v.StatusPush.Frame, e)
			}
			queryCmd := CmdDPQuery
			if ver >= V34 {
				queryCmd = CmdDPQueryNew
			}
			d.SetSeq(v.QueryReply.Seq)
			got, e = d.Encode(queryCmd, []byte(v.QueryReply.JSON))
			if e != nil || hex.EncodeToString(got) != v.QueryReply.Frame {
				t.Fatalf("query reply frame\n got %x\nwant %s (%v)", got, v.QueryReply.Frame, e)
			}
		})
	}
}

func TestVectors31(t *testing.T) {
	v := loadVectors(t, "31")
	s := clientSession(t, v)
	cmd, p := s.DPQuery()
	checkClientFrame(t, s, "dp_query", v.DPQuery, cmd, p)
	cmd, p = s.Control(map[string]any{"1": true, "2": 50})
	checkClientFrame(t, s, "control", v.Control, cmd, p)
}

func TestVectorsBroadcast(t *testing.T) {
	for _, name := range []string{"31", "33", "34", "35"} {
		t.Run(name, func(t *testing.T) {
			v := loadVectors(t, name)
			b, e := DecodeBroadcast(unhex(t, v.Broadcast.Frame))
			if e != nil {
				t.Fatal(e)
			}
			if b.GwID != v.DeviceID || b.Version != v.Version || b.IP != v.Broadcast.JSON["ip"] || b.ProductKey != "keyabcdefghijklm" {
				t.Fatalf("broadcast %+v", b)
			}
			if b.Encrypted != (name != "31") {
				t.Fatalf("encrypted flag %v", b.Encrypted)
			}
		})
	}
}
