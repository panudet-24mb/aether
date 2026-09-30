package tuyable

import (
	"crypto/md5"
	"fmt"
	"log/slog"
	"slices"
)

// Keys are the secrets a Tuya BLE device shares with its owner's cloud account: the 16-character local_key and,
// on newer ("protocol v2" in ha_tuya_ble's terms, FD50 service) devices, a 16-character sec_key. Both come from
// the one-time Tuya IoT import; the device's auth_key is not needed up front (it arrives in the handshake).
//
// Key derivation (security.py TuyaBLESecurityMaterial):
//
//	material    = local_key[:6]                (legacy)
//	            = local_key ‖ sec_key          (sec_key devices)
//	login key   = MD5(material)                 used with flag 4 (legacy) or 14 (sec_key)
//	session key = MD5(material ‖ srand)         used with flag 5 or 15; srand is 6 bytes from the device-info answer
//
// The pair request always carries local_key[:6], whichever derivation is in use.
type Keys struct {
	LocalKey string
	SecKey   string
}

// NewKeys validates the keys: ASCII, a local_key of at least 6 bytes, and a sec_key of exactly 16 bytes when set.
func NewKeys(localKey, secKey string) (Keys, error) {
	for _, s := range []string{localKey, secKey} {
		for i := range len(s) {
			if s[i] >= 0x80 {
				return Keys{}, fmt.Errorf("%w: keys must be ASCII", ErrFormat)
			}
		}
	}
	if len(localKey) < 6 {
		return Keys{}, fmt.Errorf("%w: local_key must have at least 6 bytes", ErrFormat)
	}
	if secKey != "" && len(secKey) != 16 {
		return Keys{}, fmt.Errorf("%w: sec_key must have exactly 16 bytes", ErrFormat)
	}
	return Keys{LocalKey: localKey, SecKey: secKey}, nil
}

// String, GoString and LogValue never show the keys, so Keys (and a Config holding them) can be printed or logged.
func (k Keys) String() string {
	sec := "none"
	if k.SecKey != "" {
		sec = "set"
	}
	return fmt.Sprintf("tuyable.Keys{local_key:<%d bytes redacted> sec_key:%s}", len(k.LocalKey), sec)
}

func (k Keys) GoString() string { return k.String() }

func (k Keys) LogValue() slog.Value {
	return slog.GroupValue(slog.Int("local_key_bytes", len(k.LocalKey)), slog.Bool("sec_key", k.SecKey != ""))
}

// V2 reports whether the sec_key derivation is in use.
func (k Keys) V2() bool { return k.SecKey != "" }

// PairingKey is the six bytes of local_key the pair request carries.
func (k Keys) PairingKey() []byte { return []byte(k.LocalKey[:6]) }

func (k Keys) material() []byte {
	if k.V2() {
		return []byte(k.LocalKey + k.SecKey)
	}
	return k.PairingKey()
}

// LoginKey encrypts the device-info exchange.
func (k Keys) LoginKey() []byte {
	h := md5.Sum(k.material())
	return h[:]
}

// SessionKey encrypts everything after the device-info exchange. srand is the 6-byte device random.
func (k Keys) SessionKey(srand []byte) []byte {
	h := md5.Sum(append(k.material(), srand...))
	return h[:]
}

// LoginFlag and SessionFlag are the security flags that go with the two keys.
func (k Keys) LoginFlag() byte {
	if k.V2() {
		return FlagLoginV2
	}
	return FlagLogin
}

func (k Keys) SessionFlag() byte {
	if k.V2() {
		return FlagSessionV2
	}
	return FlagSession
}

// Firmware quirks keyed by Tuya product id, from ha_tuya_ble (tuya_ble.py). They are part of the protocol as
// deployed, not of any spec.
var (
	// fd50DeviceInfoProducts send their device-info request with payload 00 f3 and protocol 2 in the first
	// fragment when they are reached through the FD50 service.
	fd50DeviceInfoProducts = []string{"jntxv3q4", "9hdajpiw", "2hmqh0ty", "qcrilcpr"}
	// legacyKeyProducts use the legacy derivation even when the cloud returns a sec_key.
	legacyKeyProducts = []string{"qcrilcpr", "mknd4lci"}
)

// UsesLegacyKeys reports whether a product ignores its sec_key.
func UsesLegacyKeys(productID string) bool { return slices.Contains(legacyKeyProducts, productID) }

// NeedsFD50DeviceInfo reports whether the device-info request takes the FD50 framing for this product.
func NeedsFD50DeviceInfo(productID string, fd50Service bool) bool {
	return fd50Service && slices.Contains(fd50DeviceInfoProducts, productID)
}

// PairRequest builds the pair payload: uuid ‖ local_key[:6] ‖ device id, zero-padded to 44 bytes.
func PairRequest(uuid string, k Keys, deviceID string) ([]byte, error) {
	b := append(append([]byte(uuid), k.PairingKey()...), deviceID...)
	if len(b) > 44 {
		return nil, fmt.Errorf("%w: uuid and device id too long for the pair request", ErrFormat)
	}
	return append(b, make([]byte, 44-len(b))...), nil
}
