package tuya

import (
	"crypto/sha256"
	"encoding/hex"
)

// Import is one device of a key import from the Tuya cloud. The local key is already sealed: the repository
// never sees it in the clear.
type Import struct {
	TuyaID, Name, Category, ProductID string
	Sub                               bool
	Spec                              []DP
	LocalKeySealed                    string
	KeyFingerprint                    string
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
