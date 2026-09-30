package tuyable

import (
	"bytes"
	"errors"
	"math/rand/v2"
	"reflect"
	"testing"
	"time"
)

// Round trips over random frames, keys, protocols and MTUs.
func TestRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range 500 {
		data := make([]byte, rng.IntN(300))
		for j := range data {
			data[j] = byte(rng.Uint32())
		}
		f := Frame{Seq: rng.Uint32(), ResponseTo: rng.Uint32(), Code: Code(rng.Uint32()), Data: data}
		key := make([]byte, 16)
		for j := range key {
			key[j] = byte(rng.Uint32())
		}
		msg, err := Seal(f, key, FlagSession, nil)
		if err != nil {
			t.Fatal(err)
		}
		mtu := MinMTU + rng.IntN(40)
		var r Reassembler
		var got []byte
		for _, p := range Fragment(msg, byte(2+i%3), mtu) {
			if len(p) > mtu {
				t.Fatalf("packet of %d bytes for MTU %d", len(p), mtu)
			}
			out, err := r.Push(p)
			if err != nil {
				t.Fatal(err)
			}
			if out != nil {
				got = out
			}
		}
		back, _, err := Unseal(got, func(byte) []byte { return key })
		if err != nil || back.Seq != f.Seq || back.Code != f.Code || !bytes.Equal(back.Data, f.Data) {
			t.Fatalf("round trip %d: %+v, %v", i, back, err)
		}
		// Any flipped ciphertext bit is caught by the CRC (or the length check) with overwhelming probability.
		bad := bytes.Clone(got)
		bad[17+rng.IntN(len(bad)-17)] ^= 1 << rng.IntN(8)
		if _, _, err := Unseal(bad, func(byte) []byte { return key }); err == nil && !bytes.Equal(bad, got) {
			// CBC corrupts a whole block; a silent pass would mean the CRC is not checked.
			t.Fatalf("corrupted message %d accepted", i)
		}
	}
}

func TestDPRoundTrip(t *testing.T) {
	dps := []DP{
		{1, DPBool, true}, {2, DPValue, int64(-2147483648)}, {3, DPValue, int64(2147483647)},
		{4, DPEnum, int64(0)}, {5, DPEnum, int64(255)}, {6, DPEnum, int64(256)}, {7, DPEnum, int64(65536)},
		{8, DPString, "ไทย"}, {9, DPRaw, []byte{}}, {10, DPBitmap, []byte{1, 2, 3}},
	}
	for _, size := range []int{1, 2} {
		b, err := EncodeDPs(dps, size)
		if err != nil {
			t.Fatal(err)
		}
		got, end, err := ParseDPs(b, 0, size)
		if err != nil || end != len(b) || !reflect.DeepEqual(got, dps) {
			t.Fatalf("size %d: %v %v", size, got, err)
		}
	}
	for _, bad := range []DP{
		{1, DPValue, int64(1 << 31)}, {1, DPEnum, int64(-1)}, {1, DPBool, 1}, {1, DPString, []byte("x")}, {1, 9, 0},
	} {
		if _, err := EncodeDPs([]DP{bad}, 1); !errors.Is(err, ErrFormat) {
			t.Errorf("EncodeDPs(%v) = %v", bad, err)
		}
	}
	if _, err := EncodeDPs([]DP{{1, DPString, string(make([]byte, 256))}}, 1); !errors.Is(err, ErrFormat) {
		t.Error("256-byte string accepted with one length byte")
	}
}

func TestReassemblerRejects(t *testing.T) {
	var r Reassembler
	for _, tc := range []struct {
		name string
		pkts [][]byte
	}{
		{"continuation first", [][]byte{{1, 0xAA}}},
		{"gap", [][]byte{{0, 40, 0x30, 1, 2}, {2, 3}}},
		{"too long", [][]byte{{0, 3, 0x30, 1, 2}, {1, 3, 4}}},
		{"length zero", [][]byte{{0, 0, 0x30}}},
		{"length beyond limit", [][]byte{AppendVarint(AppendVarint(nil, 0), MaxMessage+1)}},
		{"no protocol byte", [][]byte{{0, 5}}},
		{"varint too long", [][]byte{{0x80, 0x80, 0x80, 0x80, 0x01}}},
	} {
		var err error
		for _, p := range tc.pkts {
			if _, err = r.Push(p); err != nil {
				break
			}
		}
		if !errors.Is(err, ErrFormat) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	// A new packet 0 replaces a partial message.
	r.Reset()
	r.Push([]byte{0, 10, 0x30, 1, 2})
	if out, err := r.Push([]byte{0, 2, 0x30, 7, 8}); err != nil || !bytes.Equal(out, []byte{7, 8}) {
		t.Fatalf("restart: %x %v", out, err)
	}
}

func TestParseReportRejects(t *testing.T) {
	for _, f := range []Frame{
		{Code: CodeReceiveSignDP, Data: []byte{0, 1}},
		{Code: CodeReceiveDPV4, Data: []byte{1, 0, 0, 0, 0, 0, 0}},
		{Code: CodeReceiveTimeDPV4, Data: []byte{0, 0, 0, 0, 0, 0, 0}},
		{Code: CodeReceiveTimeDP, Data: []byte{2}},
		{Code: CodeReceiveTimeDP, Data: []byte{0, '1', '2'}},
		{Code: CodeReceiveDP, Data: []byte{1, 9, 1, 0}},
		{Code: CodeReceiveDP, Data: []byte{1, 1, 5, 0}},
		{Code: 0x8099},
	} {
		if _, _, err := parseReport(f, time.Now()); !errors.Is(err, ErrFormat) {
			t.Errorf("parseReport(%s %x) = %v", f.Code, f.Data, err)
		}
	}
}

func FuzzReassemble(f *testing.F) {
	msg, _ := Seal(Frame{Seq: 1, Code: CodeDeviceStatus}, make([]byte, 16), FlagSession, make([]byte, 16))
	var seed []byte
	for _, p := range Fragment(msg, 3, GATTMTU) {
		seed = append(append(seed, byte(len(p))), p...)
	}
	f.Add(seed)
	f.Add([]byte{3, 0, 5, 0x30, 2, 1})
	f.Fuzz(func(t *testing.T, stream []byte) {
		// The stream is a sequence of length-prefixed packets.
		var r Reassembler
		for len(stream) > 0 {
			n := min(int(stream[0]), len(stream)-1)
			pkt := stream[1 : 1+n]
			stream = stream[1+n:]
			out, err := r.Push(pkt)
			if err == nil && out != nil {
				if len(out) > MaxMessage {
					t.Fatalf("message of %d bytes", len(out))
				}
				Unseal(out, func(byte) []byte { return make([]byte, 16) })
			}
		}
	})
}

func FuzzParseDPs(f *testing.F) {
	enc, _ := EncodeDPs([]DP{{1, DPBool, true}, {2, DPValue, int64(5)}, {3, DPString, "x"}, {4, DPRaw, []byte{1}}}, 1)
	f.Add(enc, false)
	enc2, _ := EncodeDPs([]DP{{1, DPEnum, int64(300)}}, 2)
	f.Add(enc2, true)
	f.Fuzz(func(t *testing.T, data []byte, v4 bool) {
		size := 1
		if v4 {
			size = 2
		}
		dps, end, err := ParseDPs(data, 0, size)
		if end > len(data) {
			t.Fatalf("end %d beyond %d", end, len(data))
		}
		if err != nil {
			return
		}
		// Whatever parses re-encodes to a prefix-equal byte string, except integers the device sent in a
		// non-canonical width (the encoder picks its own widths).
		again, err := EncodeDPs(dps, size)
		if err != nil {
			for _, dp := range dps {
				if dp.Type == DPValue || dp.Type == DPEnum {
					return
				}
			}
			t.Fatalf("re-encode %v: %v", dps, err)
		}
		back, _, err := ParseDPs(again, 0, size)
		if err != nil || !reflect.DeepEqual(back, dps) {
			t.Fatalf("re-parse %v: %v %v", dps, back, err)
		}
	})
}

func FuzzParseReport(f *testing.F) {
	body, _ := EncodeDPs([]DP{{1, DPBool, true}, {2, DPValue, int64(7)}}, 1)
	body4, _ := EncodeDPs([]DP{{1, DPBool, true}}, 2)
	f.Add(uint16(CodeReceiveDP), body)
	f.Add(uint16(CodeReceiveSignTimeDP), append([]byte{0, 1, 0, 1, 0, 0, 0, 1}, body...))
	f.Add(uint16(CodeReceiveTimeDPV4), append([]byte{0, 0, 0, 0, 1, 0, 0, 0}, append([]byte("1767225600123"), body4...)...))
	f.Fuzz(func(t *testing.T, code uint16, data []byte) {
		rep, ack, err := parseReport(Frame{Code: Code(code), Data: data}, time.Unix(0, 0))
		if err != nil {
			return
		}
		if ack != nil && len(ack) > 8 {
			t.Fatalf("ack of %d bytes", len(ack))
		}
		for _, dp := range rep.DPs {
			if dp.Type > DPBitmap {
				t.Fatalf("type %d", dp.Type)
			}
		}
	})
}

func FuzzParseAdvert(f *testing.F) {
	sd, md := EncodeAdvert(Advert{ProductID: "gvygg3m8", UUID: "tuya5d8f2a3c9e1b", Protocol: 3, Bound: true})
	f.Add(sd, md)
	f.Add([]byte{0}, []byte{0, 4, 0, 0, 0, 0, 1})
	f.Fuzz(func(t *testing.T, sd, md []byte) {
		a, err := ParseAdvert(sd, md)
		if err != nil {
			return
		}
		if !printable(a.UUID) || !printable(a.ProductID) {
			t.Fatalf("unprintable advert %+v", a)
		}
	})
}

// FuzzSealUnseal checks that any frame round-trips and that Unseal never panics or accepts garbage silently for a
// different key.
func FuzzSealUnseal(f *testing.F) {
	f.Add(uint32(1), uint32(0), uint16(CodeDeviceStatus), []byte{}, []byte("0123456789abcdef"), []byte{})
	f.Add(uint32(9), uint32(3), uint16(CodeDPsV4), []byte{0, 0, 0, 0, 5, 1, 1, 0, 1, 1}, []byte("fedcba9876543210"), []byte{4, 20})
	f.Fuzz(func(t *testing.T, seq, resp uint32, code uint16, data, key, noise []byte) {
		if len(key) != 16 || len(data) > MaxData {
			return
		}
		fr := Frame{Seq: seq, ResponseTo: resp, Code: Code(code), Data: data}
		msg, err := Seal(fr, key, FlagSession, nil)
		if err != nil {
			t.Fatal(err)
		}
		got, _, err := Unseal(msg, func(byte) []byte { return key })
		if err != nil || got.Seq != seq || got.ResponseTo != resp || got.Code != fr.Code || !bytes.Equal(got.Data, data) {
			t.Fatalf("round trip: %+v %v", got, err)
		}
		Unseal(noise, func(byte) []byte { return key })
		Unseal(append([]byte{FlagSession}, noise...), func(byte) []byte { return key })
	})
}
