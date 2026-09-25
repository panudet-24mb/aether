package tuyalocal

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"testing"
)

// Every byte of the CRC/HMAC/GCM-protected region is covered: flipping any one must be refused, never
// decoded into different DPs.
func TestTamperedFramesAreRejected(t *testing.T) {
	for _, name := range []string{"33", "34", "35"} {
		t.Run(name, func(t *testing.T) {
			v := loadVectors(t, name)
			s := clientSession(t, v)
			if v.Negotiation != nil {
				s.SetSessionKey(unhex(t, v.Negotiation.SessionKey))
			}
			good := unhex(t, v.StatusPush.Frame)
			if _, _, e := s.Unpack(good); e != nil {
				t.Fatalf("untouched frame: %v", e)
			}
			// Skip the prefix and length fields (tampering those is a framing error, tested separately).
			start := 16
			if v.Version == "3.5" {
				start = 18
			}
			for i := start; i < len(good)-4; i++ {
				bad := bytes.Clone(good)
				bad[i] ^= 0x01
				_, _, e := s.Unpack(bad)
				if !errors.Is(e, ErrChecksum) && !errors.Is(e, ErrAuth) {
					t.Fatalf("byte %d flipped: got %v, want a checksum/auth error", i, e)
				}
			}
			// The header is authenticated too (CRC/HMAC over it on 55AA, GCM additional data on 6699).
			bad := bytes.Clone(good)
			bad[5] ^= 0x01 // sequence number
			if _, _, e := s.Unpack(bad); e == nil {
				t.Fatal("flipped sequence number accepted")
			}
		})
	}
}

func TestNegotiationRejectsWrongKey(t *testing.T) {
	for _, name := range []string{"34", "35"} {
		t.Run(name, func(t *testing.T) {
			v := loadVectors(t, name)
			n := v.Negotiation
			neg := &negotiation{version: map[string]Version{"34": V34, "35": V35}[name], localKey: []byte(v.LocalKey), localNonce: []byte(n.LocalNonce)}
			// A device holding another key proves the wrong HMAC over our nonce.
			wrong := NegotiationResponse([]byte("ffffffffffffffff"), []byte(n.LocalNonce), []byte(n.RemoteNonce))
			if _, e := neg.finishPayload(wrong); !errors.Is(e, ErrKeyRejected) {
				t.Fatalf("wrong HMAC: %v", e)
			}
			if _, e := neg.finishPayload([]byte("short")); !errors.Is(e, ErrKeyRejected) {
				t.Fatalf("short answer: %v", e)
			}
			// …and frames it with that key, which fails before the HMAC is even read.
			other, _ := NewSession(neg.version, v.DeviceID, "ffffffffffffffff", Device)
			frame, e := other.Encode(CmdSessKeyNegResp, wrong)
			if e != nil {
				t.Fatal(e)
			}
			s := clientSession(t, v)
			if _, _, e := s.Unpack(frame); !errors.Is(e, ErrChecksum) && !errors.Is(e, ErrAuth) {
				t.Fatalf("frame under another key: %v", e)
			}
			// The device side refuses a FINISH that does not prove our key.
			if VerifyFinish([]byte(v.LocalKey), []byte(n.RemoteNonce), bytes.Repeat([]byte{1}, 32)) {
				t.Fatal("forged FINISH accepted")
			}
		})
	}
}

// 3.3 has no negotiation: a device with another key yields a payload that does not decrypt.
func TestWrongKeyOn33IsSuspect(t *testing.T) {
	v := loadVectors(t, "33")
	other, _ := NewSession(V33, v.DeviceID, "ffffffffffffffff", Device)
	frame, e := other.Encode(CmdDPQuery, []byte(`{"devId":"x","dps":{"1":true}}`))
	if e != nil {
		t.Fatal(e)
	}
	s := clientSession(t, v)
	f, _, e := s.Unpack(frame) // CRC is keyless, so framing succeeds
	if e != nil {
		t.Fatal(e)
	}
	if p, e := s.Decode(f); e == nil {
		if _, _, e := ParseDPS(p); e == nil {
			t.Fatalf("payload under another key decoded: %s", p)
		}
	}
}

// 3.5 devices answer with their own global counter; nothing may depend on it matching ours.
func TestSequenceMismatchTolerated(t *testing.T) {
	v := loadVectors(t, "35")
	d, _ := NewSession(V35, v.DeviceID, v.LocalKey, Device)
	d.SetSessionKey(unhex(t, v.Negotiation.SessionKey))
	d.SetSeq(987654)
	frame, e := d.Encode(CmdStatus, []byte(v.StatusPush.JSON))
	if e != nil {
		t.Fatal(e)
	}
	s := clientSession(t, v)
	s.SetSessionKey(unhex(t, v.Negotiation.SessionKey))
	f, _, e := s.Unpack(frame)
	if e != nil || f.Seq != 987654 {
		t.Fatalf("seq %d: %v", f.Seq, e)
	}
	sameDPS(t, decodeDevice(t, s, frame), v.StatusPush.DPS)
}

func TestFrameReaderResyncsAndBounds(t *testing.T) {
	v := loadVectors(t, "33")
	a, b := unhex(t, v.StatusPush.Frame), unhex(t, v.QueryReply.Frame)
	// Garbage, a lone prefix with an absurd length, then two real frames back to back.
	junk := []byte{0x13, 0x37, 0x00, 0x00, 0x55, 0xAA, 0, 0, 0, 1, 0, 0, 0, 8, 0x7f, 0xff, 0xff, 0xff}
	stream := append(append(append(junk, a...), b...), 0x00, 0x00)
	fr := NewFrameReader(bytes.NewReader(stream))
	got1, e := fr.Next()
	if e != nil || !bytes.Equal(got1, a) {
		t.Fatalf("first frame: %v", e)
	}
	got2, e := fr.Next()
	if e != nil || !bytes.Equal(got2, b) {
		t.Fatalf("second frame: %v", e)
	}
	if _, e := fr.Next(); !errors.Is(e, io.EOF) && !errors.Is(e, io.ErrUnexpectedEOF) {
		t.Fatalf("trailing bytes: %v", e)
	}
	if _, e := FrameLength(append([]byte{0, 0, 0x55, 0xAA}, make([]byte, 12)...)); e != nil {
		t.Fatalf("zero-length header: %v", e)
	}
	huge := []byte{0, 0, 0x55, 0xAA, 0, 0, 0, 1, 0, 0, 0, 8, 0, 1, 0, 0}
	if _, e := FrameLength(huge); !errors.Is(e, ErrTooLarge) {
		t.Fatalf("oversized frame: %v", e)
	}
}

func TestPaddingIsVerified(t *testing.T) {
	key := []byte("0123456789abcdef")
	ct, _ := ecbEncrypt(key, []byte("hello"), true)
	if p, e := ecbDecrypt(key, ct, true); e != nil || string(p) != "hello" {
		t.Fatalf("round trip %q %v", p, e)
	}
	if _, e := ecbDecrypt([]byte("ffffffffffffffff"), ct, true); !errors.Is(e, ErrPadding) {
		t.Fatalf("wrong key padding: %v", e)
	}
	if _, e := ecbDecrypt(key, ct[:5], true); !errors.Is(e, ErrPadding) {
		t.Fatalf("partial block: %v", e)
	}
}

func FuzzUnpack(f *testing.F) {
	for _, name := range []string{"33", "34", "35"} {
		raw := mustVectors(name)
		f.Add(raw.push, raw.key)
		f.Add(raw.bc, []byte(nil))
	}
	f.Add([]byte{0, 0, 0x55, 0xAA}, []byte(nil))
	f.Add([]byte{0, 0, 0x66, 0x99, 0, 0}, []byte("0123456789abcdef"))
	f.Fuzz(func(t *testing.T, data, key []byte) {
		if len(key) != 0 && len(key) != 16 {
			key = nil
		}
		for _, m := range []RetcodeMode{NoRetcode, WithRetcode, DetectRetcode} {
			fr, n, e := Unpack(data, key, m)
			if e == nil && (n <= 0 || n > len(data) || len(fr.Payload) > n) {
				t.Fatalf("consumed %d of %d", n, len(data))
			}
		}
		_, _ = DecodeBroadcast(data)
		r := NewFrameReader(bytes.NewReader(data))
		for i := 0; i < 8; i++ {
			if _, e := r.Next(); e != nil {
				break
			}
		}
		if s, e := NewSession(V33, "x", "0123456789abcdef", Client); e == nil {
			if fr, _, e := s.Unpack(data); e == nil {
				_, _ = s.Decode(fr)
			}
		}
	})
}

type fuzzSeed struct{ push, bc, key []byte }

func mustVectors(name string) fuzzSeed {
	v, e := readVectors(name)
	if e != nil {
		panic(e)
	}
	dec := func(s string) []byte { b, _ := hex.DecodeString(s); return b }
	var key []byte
	if v.Negotiation != nil {
		key = dec(v.Negotiation.SessionKey)
	}
	return fuzzSeed{push: dec(v.StatusPush.Frame), bc: dec(v.Broadcast.Frame), key: key}
}
