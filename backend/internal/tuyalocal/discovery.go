package tuyalocal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
)

// Discovery ports (const.py): 6666 carries 3.1 plaintext broadcasts, 6667 encrypted 3.3+ broadcasts and
// 7000 the 3.5 app-solicited ones.
const (
	UDPPort    = 6666
	UDPPortEnc = 6667
	UDPPortApp = 7000
)

// Broadcast is what a device announces on the LAN.
type Broadcast struct {
	GwID       string
	IP         string
	Version    string
	ProductKey string
	// Encrypted is true when the packet was encrypted (anything but 3.1 plaintext).
	Encrypted bool
	// Raw is the decoded JSON object.
	Raw map[string]any
}

// ErrNotBroadcast is returned for packets that are not Tuya discovery broadcasts.
var ErrNotBroadcast = errors.New("tuyalocal: not a Tuya discovery broadcast")

// DecodeBroadcast decodes one UDP discovery packet (udp_helper.decrypt_udp): a 55AA frame whose payload is
// plaintext JSON (3.1) or AES-ECB with the fixed discovery key (3.3/3.4), a 6699 frame sealed with AES-GCM
// under that key (3.5), or a bare ECB blob.
func DecodeBroadcast(pkt []byte) (Broadcast, error) {
	var plain []byte
	encrypted := true
	switch {
	case bytes.HasPrefix(pkt, prefix55AA):
		f, _, e := Unpack(pkt, nil, WithRetcode)
		if e != nil {
			return Broadcast{}, fmt.Errorf("%w: %v", ErrNotBroadcast, e)
		}
		if len(f.Payload) > 1 && f.Payload[0] == '{' && f.Payload[len(f.Payload)-1] == '}' {
			plain, encrypted = f.Payload, false
		} else if plain, e = ecbDecrypt(udpKey, f.Payload, true); e != nil {
			return Broadcast{}, fmt.Errorf("%w: %v", ErrNotBroadcast, e)
		}
	case bytes.HasPrefix(pkt, prefix6699):
		f, _, e := Unpack(pkt, udpKey, DetectRetcode)
		if e != nil {
			return Broadcast{}, fmt.Errorf("%w: %v", ErrNotBroadcast, e)
		}
		plain = bytes.TrimRight(f.Payload, "\x00")
	default:
		var e error
		if plain, e = ecbDecrypt(udpKey, pkt, true); e != nil {
			return Broadcast{}, fmt.Errorf("%w: %v", ErrNotBroadcast, e)
		}
	}
	var raw map[string]any
	if e := json.Unmarshal(plain, &raw); e != nil {
		return Broadcast{}, fmt.Errorf("%w: %v", ErrNotBroadcast, e)
	}
	b := Broadcast{Encrypted: encrypted, Raw: raw}
	b.GwID, _ = raw["gwId"].(string)
	b.IP, _ = raw["ip"].(string)
	b.ProductKey, _ = raw["productKey"].(string)
	switch v := raw["version"].(type) {
	case string:
		b.Version = v
	case float64:
		b.Version = strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.1f", v), "0"), ".")
	}
	if b.GwID == "" {
		return Broadcast{}, fmt.Errorf("%w: no gwId", ErrNotBroadcast)
	}
	return b, nil
}

// EncodeBroadcast builds a discovery packet the way a device of version v sends it (for the simulator and
// tests). 3.1 is plaintext JSON in a 55AA frame, 3.3/3.4 ECB in a 55AA frame, 3.5 GCM in a 6699 frame.
func EncodeBroadcast(v Version, body map[string]any, iv []byte) ([]byte, error) {
	js, e := json.Marshal(body)
	if e != nil {
		return nil, e
	}
	zero := uint32(0)
	switch {
	case v == V31:
		return Pack55AA(0, CmdUDPNew, &zero, js, nil), nil
	case v == V35:
		if iv == nil {
			iv = randomIV()
		}
		return Pack6699(0, CmdUDPNew, &zero, js, udpKey, iv)
	default:
		ct, e := ecbEncrypt(udpKey, js, true)
		if e != nil {
			return nil, e
		}
		return Pack55AA(0, CmdUDPNew, &zero, ct, nil), nil
	}
}

// Listen receives discovery broadcasts on the given UDP addresses (for example ":6666", ":6667", ":7000")
// until ctx ends, calling onBroadcast for every packet that decodes. Packets that are not Tuya broadcasts
// are ignored. 3.5 devices may only announce themselves after a REQ_DEVINFO solicitation to port 7000
// (command_types.py REQ_DEVINFO); this listener does not send one.
func Listen(ctx context.Context, addrs []string, onBroadcast func(Broadcast, net.Addr)) error {
	var conns []net.PacketConn
	for _, a := range addrs {
		pc, e := (&net.ListenConfig{}).ListenPacket(ctx, "udp4", a)
		if e != nil {
			for _, c := range conns {
				c.Close()
			}
			return e
		}
		conns = append(conns, pc)
	}
	var wg sync.WaitGroup
	for _, pc := range conns {
		wg.Add(1)
		go func(pc net.PacketConn) {
			defer wg.Done()
			buf := make([]byte, MaxFrameLength)
			for {
				n, from, e := pc.ReadFrom(buf)
				if e != nil {
					return
				}
				if b, e := DecodeBroadcast(buf[:n]); e == nil {
					onBroadcast(b, from)
				}
			}
		}(pc)
	}
	<-ctx.Done()
	for _, pc := range conns {
		pc.Close()
	}
	wg.Wait()
	return ctx.Err()
}
