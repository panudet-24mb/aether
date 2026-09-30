// Package tuyable speaks the Tuya BLE point-to-point protocol (versions 2, 3 and 4, plus the "v2 sec_key" key
// derivation) over an abstract GATT link. It holds no radio code: a Link carries the 20-byte writes and
// notifications, so the same session runs over BlueZ, an ESPHome Bluetooth proxy or the tuyablesim simulator.
//
// The byte-level logic is ported from ha_tuya_ble (MIT; see LICENSE.ha_tuya_ble) and checked against vectors
// generated from that code (testdata/vectors.json). Tuya BLE mesh and SIG mesh devices use a different protocol
// and are not supported.
//
// Wire format, from the outside in:
//
//	GATT write / notify (≤ 20 bytes each)   varint(packet#) [varint(total length) byte(protocol<<4) if packet# == 0] chunk
//	message (the chunks joined)             security_flag(1) ‖ IV(16) ‖ AES-128-CBC(key, IV, plaintext)
//	plaintext                               seq(4) ‖ response_to(4) ‖ code(2) ‖ length(2) ‖ data ‖ CRC16(2) ‖ zero padding to 16
//
// All integers are big-endian. The CRC is CRC-16/MODBUS (init 0xFFFF, reflected polynomial 0xA001) over
// everything before it. The security flag selects the key: 4 (or 14 for sec_key devices) is the login key used
// for the device-info exchange, 5 (or 15) is the session key used afterwards, and 1 is the auth key the device
// hands out in its device-info answer. The login and session keys are 16-byte MD5 digests (AES-128); the auth key
// is 32 bytes, so frames sealed with it are AES-256-CBC.
//
// There is no MAC: the CRC only catches accidental corruption and wrong keys. Within one connection an attacker in
// radio range could replay or tamper with frames undetected; each connection derives a new session key from the
// device's random, which stops replay across connections.
package tuyable

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"fmt"
)

// Code is a Tuya BLE function code (const.py TuyaBLECode). Codes below 0x8000 are sent by the host (the device
// answers with the same code and response_to set); codes from 0x8000 are initiated by the device.
type Code uint16

const (
	CodeDeviceInfo   Code = 0x0000
	CodePair         Code = 0x0001
	CodeDPs          Code = 0x0002
	CodeDeviceStatus Code = 0x0003
	CodeUnbind       Code = 0x0005
	CodeDeviceReset  Code = 0x0006
	CodeDPsV4        Code = 0x0027

	CodeReceiveDP         Code = 0x8001
	CodeReceiveTimeDP     Code = 0x8003
	CodeReceiveSignDP     Code = 0x8004
	CodeReceiveSignTimeDP Code = 0x8005
	CodeReceiveDPV4       Code = 0x8006
	CodeReceiveTimeDPV4   Code = 0x8007
	CodeTime1Request      Code = 0x8011
	CodeTime2Request      Code = 0x8012
)

var codeNames = map[Code]string{
	CodeDeviceInfo: "DEVICE_INFO", CodePair: "PAIR", CodeDPs: "DPS", CodeDeviceStatus: "DEVICE_STATUS",
	CodeUnbind: "UNBIND", CodeDeviceReset: "DEVICE_RESET", CodeDPsV4: "DPS_V4",
	CodeReceiveDP: "RECEIVE_DP", CodeReceiveTimeDP: "RECEIVE_TIME_DP", CodeReceiveSignDP: "RECEIVE_SIGN_DP",
	CodeReceiveSignTimeDP: "RECEIVE_SIGN_TIME_DP", CodeReceiveDPV4: "RECEIVE_DP_V4",
	CodeReceiveTimeDPV4: "RECEIVE_TIME_DP_V4", CodeTime1Request: "TIME1_REQ", CodeTime2Request: "TIME2_REQ",
}

func (c Code) String() string {
	if n, ok := codeNames[c]; ok {
		return n
	}
	return fmt.Sprintf("0x%04x", uint16(c))
}

// Security flags (the first byte of a message).
const (
	FlagAuth      byte = 1
	FlagLogin     byte = 4
	FlagSession   byte = 5
	FlagLoginV2   byte = 14
	FlagSessionV2 byte = 15
)

const (
	headerSize = 12
	ivSize     = 16
	// MaxData bounds the data field of one frame. Real frames are well under 1 KiB; the bound keeps a hostile or
	// broken peer from making us allocate from a length it chose.
	MaxData = 2048
)

// Frame is one decrypted Tuya BLE message.
type Frame struct {
	Seq        uint32
	ResponseTo uint32
	Code       Code
	Data       []byte
}

// CRC16 is CRC-16/MODBUS as the protocol uses it (tuya_ble.py _calc_crc16).
func CRC16(data []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, b := range data {
		crc ^= uint16(b)
		for range 8 {
			if crc&1 != 0 {
				crc = crc>>1 ^ 0xA001
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}

// Plaintext lays a frame out before encryption: header, data, CRC and zero padding to the AES block size.
func Plaintext(f Frame) ([]byte, error) {
	if len(f.Data) > MaxData {
		return nil, fmt.Errorf("%w: %d data bytes", ErrFormat, len(f.Data))
	}
	raw := make([]byte, headerSize, headerSize+len(f.Data)+2+aes.BlockSize)
	binary.BigEndian.PutUint32(raw[0:], f.Seq)
	binary.BigEndian.PutUint32(raw[4:], f.ResponseTo)
	binary.BigEndian.PutUint16(raw[8:], uint16(f.Code))
	binary.BigEndian.PutUint16(raw[10:], uint16(len(f.Data)))
	raw = append(raw, f.Data...)
	raw = binary.BigEndian.AppendUint16(raw, CRC16(raw))
	for len(raw)%aes.BlockSize != 0 {
		raw = append(raw, 0)
	}
	return raw, nil
}

// Seal encrypts a frame into a message: flag ‖ IV ‖ ciphertext. A nil iv draws a random one, which is what the
// protocol expects; tests pass a fixed IV to compare with the reference vectors.
func Seal(f Frame, key []byte, flag byte, iv []byte) ([]byte, error) {
	raw, err := Plaintext(f)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("tuyable: key: %w", err)
	}
	if iv == nil {
		iv = make([]byte, ivSize)
		if _, err := rand.Read(iv); err != nil {
			return nil, err
		}
	} else if len(iv) != ivSize {
		return nil, fmt.Errorf("%w: IV must be %d bytes", ErrFormat, ivSize)
	}
	out := make([]byte, 1+ivSize+len(raw))
	out[0] = flag
	copy(out[1:], iv)
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out[1+ivSize:], raw)
	return out, nil
}

// Unseal decrypts a message with the key keyFor returns for its security flag (nil means the flag is not
// acceptable) and checks the header and CRC.
//
// ha_tuya_ble skips the CRC check when the plaintext ends exactly at the data; every frame a device builds
// carries its CRC, so Unseal always requires it: with a wrong key that check is what fails.
func Unseal(msg []byte, keyFor func(flag byte) []byte) (Frame, byte, error) {
	if len(msg) < 1+ivSize+aes.BlockSize || (len(msg)-1-ivSize)%aes.BlockSize != 0 {
		return Frame{}, 0, fmt.Errorf("%w: message of %d bytes", ErrFormat, len(msg))
	}
	flag := msg[0]
	key := keyFor(flag)
	if key == nil {
		return Frame{}, flag, fmt.Errorf("%w: unexpected security flag %d", ErrFormat, flag)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return Frame{}, flag, fmt.Errorf("tuyable: key: %w", err)
	}
	raw := make([]byte, len(msg)-1-ivSize)
	cipher.NewCBCDecrypter(block, msg[1:1+ivSize]).CryptBlocks(raw, msg[1+ivSize:])
	n := int(binary.BigEndian.Uint16(raw[10:]))
	end := headerSize + n
	if end+2 > len(raw) {
		return Frame{}, flag, fmt.Errorf("%w: data length %d beyond the message", ErrCRC, n)
	}
	if CRC16(raw[:end]) != binary.BigEndian.Uint16(raw[end:]) {
		return Frame{}, flag, ErrCRC
	}
	return Frame{
		Seq:        binary.BigEndian.Uint32(raw[0:]),
		ResponseTo: binary.BigEndian.Uint32(raw[4:]),
		Code:       Code(binary.BigEndian.Uint16(raw[8:])),
		Data:       bytes.Clone(raw[headerSize:end]),
	}, flag, nil
}
