package tuyable

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"fmt"
	"strings"
)

// GATT identifiers. Legacy devices expose service 0x1910 (advertised as 0xA201) with notify 0x2B10 and write
// 0x2B11; newer TuyaOS devices expose service 0xFD50 with their own 128-bit characteristics (const.py).
const (
	ServiceA201    uint16 = 0xA201
	ServiceFD50    uint16 = 0xFD50
	ManufacturerID uint16 = 0x07D0

	CharNotify     = "00002b10-0000-1000-8000-00805f9b34fb"
	CharWrite      = "00002b11-0000-1000-8000-00805f9b34fb"
	CharNotifyFD50 = "00000002-0000-1001-8001-00805f9b07d0"
	CharWriteFD50  = "00000001-0000-1001-8001-00805f9b07d0"
)

// Advert is what a Tuya BLE advertisement says about a device without connecting to it.
//
// Service data (on 0xA201 or 0xFD50): type(1) ‖ id. Type 0 means the id is the product id (8 or 16 ASCII bytes).
// Manufacturer data (company 0x07D0):  flags(1) ‖ protocol(1) ‖ 4 bytes ‖ AES-CBC(key = IV = MD5(product id), uuid)
// where bit 7 of flags is "bound". Advertisements do not carry DP values; those need a connection.
type Advert struct {
	ProductID string
	Bound     bool
	Protocol  byte
	// UUID is the device uuid, empty when the service data did not carry a product id to decrypt it with.
	UUID string
}

// ParseAdvert decodes the service data and manufacturer data of one advertisement (tuya_ble.py
// _decode_advertisement_data). serviceData may be nil.
func ParseAdvert(serviceData, manufacturerData []byte) (Advert, error) {
	var a Advert
	if len(serviceData) > 1 && serviceData[0] == 0 {
		a.ProductID = string(serviceData[1:])
		if !printable(a.ProductID) {
			return Advert{}, fmt.Errorf("%w: product id is not printable", ErrFormat)
		}
	}
	if len(manufacturerData) <= 6 {
		return Advert{}, fmt.Errorf("%w: manufacturer data of %d bytes", ErrFormat, len(manufacturerData))
	}
	a.Bound = manufacturerData[0]&0x80 != 0
	a.Protocol = manufacturerData[1]
	enc := manufacturerData[6:]
	if a.ProductID == "" {
		return a, nil
	}
	if len(enc)%aes.BlockSize != 0 {
		return Advert{}, fmt.Errorf("%w: encrypted uuid of %d bytes", ErrFormat, len(enc))
	}
	key := md5.Sum([]byte(a.ProductID))
	block, _ := aes.NewCipher(key[:])
	out := make([]byte, len(enc))
	cipher.NewCBCDecrypter(block, key[:]).CryptBlocks(out, enc)
	a.UUID = strings.TrimRight(string(out), "\x00")
	if !printable(a.UUID) {
		return Advert{}, fmt.Errorf("%w: uuid did not decrypt to text", ErrFormat)
	}
	return a, nil
}

// EncodeAdvert builds the service and manufacturer data for an advertisement. The simulator uses it; a uuid that
// is not a multiple of 16 bytes is zero-padded.
func EncodeAdvert(a Advert) (serviceData, manufacturerData []byte) {
	serviceData = append([]byte{0}, a.ProductID...)
	flags := byte(0)
	if a.Bound {
		flags = 0x80
	}
	manufacturerData = []byte{flags, a.Protocol, 0, 0, 0, 0}
	uuid := []byte(a.UUID)
	for len(uuid)%aes.BlockSize != 0 {
		uuid = append(uuid, 0)
	}
	key := md5.Sum([]byte(a.ProductID))
	block, _ := aes.NewCipher(key[:])
	enc := make([]byte, len(uuid))
	cipher.NewCBCEncrypter(block, key[:]).CryptBlocks(enc, uuid)
	return serviceData, append(manufacturerData, enc...)
}

func printable(s string) bool {
	for i := range len(s) {
		if s[i] < 0x20 || s[i] > 0x7E {
			return false
		}
	}
	return true
}
