package tuya

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// Import is one device of a key import from the Tuya cloud. The local key is already sealed: the repository
// never sees it in the clear.
type Import struct {
	TuyaID, Name, Category, ProductID string
	Sub                               bool
	Spec                              []DP
	LocalKeySealed                    string
	KeyFingerprint                    string
	// For Tuya BLE (docs/platform/tuya-ble.md): the address from the device's factory record (Aether's form, or
	// empty), its uuid, and its sec_key sealed like the local key (empty when Tuya returned none). The import stores
	// them; it never decides how the device is reached.
	BLEMAC, BLEUUID string
	SecKeySealed    string
}

// Fingerprint identifies a local key without revealing it: the first 16 hex digits of its SHA-256, which is what
// core.tuya_devices.key_fingerprint holds and what the UI shows.
func Fingerprint(localKey string) string {
	sum := sha256.Sum256([]byte(localKey))
	return hex.EncodeToString(sum[:])[:16]
}

// ValidLocalKey reports whether a string looks like a Tuya local key: 16 printable ASCII characters.
func ValidLocalKey(k string) bool {
	if len(k) != 16 {
		return false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 0x21 || k[i] > 0x7e {
			return false
		}
	}
	return true
}

// refreshCodes are data points many devices only report when asked (UPDATEDPS): live metering values.
var refreshCodes = map[string]bool{"cur_power": true, "cur_voltage": true, "cur_current": true}

// RefreshDPs lists the data points the agent should ask the device to refresh periodically.
func RefreshDPs(spec []DP) []int {
	out := []int{}
	for _, d := range spec {
		if refreshCodes[d.Code] {
			out = append(out, d.ID)
		}
	}
	return out
}

// DPTypes lists each data point's type by id ("1" -> "bool"), which a Tuya BLE agent needs to encode a command:
// over BLE every value carries its type byte.
func DPTypes(spec []DP) map[string]string {
	out := make(map[string]string, len(spec))
	for _, d := range spec {
		switch d.Type {
		case "bool", "value", "enum", "string", "bitmap", "raw":
			out[strconv.Itoa(d.ID)] = d.Type
		}
	}
	return out
}

// lockCategories are the Tuya product categories of locks and safes (door locks, residential and business locks,
// hotel locks, lock-with-camera models, access control, safe boxes). Over BLE they are read-only in Aether: their
// data points carry unlock requests and member keys, and actuating them waits for an explicit decision.
var lockCategories = map[string]bool{"ms": true, "jtmspro": true, "jtmsbh": true, "gyms": true, "hotelms": true, "bxx": true,
	"videolock": true, "photolock": true, "mk": true, "ms_category": true}

// LockCategory reports a Tuya lock or safe category.
func LockCategory(category string) bool { return lockCategories[category] }
