package zigbee2mqtt

import (
	"aether/backend/internal/domain"
	"aether/backend/internal/security"
	"encoding/json"
)

// Commands go to aether/z2m/<gateway id>/<ieee>/set. The device is always addressed by IEEE address, which
// Zigbee2MQTT accepts in place of the friendly name: a rename, or a name containing "/", cannot redirect a
// command. mqtt-commander's broker account may write exactly these topics and nothing else (no bridge/request).

// SetTopic is the topic a command for one device of one gateway is published to.
func SetTopic(gatewayID, ieee string) (string, error) {
	if !security.ValidID(gatewayID) || !ValidIEEE(ieee) {
		return "", domain.ErrInvalid
	}
	return Prefix + gatewayID + "/" + ieee + "/set", nil
}

// SetPayload is the /set body for one property: {"brightness":128}. The value was validated against the device's
// definition when the command was queued and is always explicit (a toggle was resolved to ON or OFF), so a
// duplicate delivery changes nothing.
func SetPayload(property string, value json.RawMessage) ([]byte, error) {
	if !PropertyPattern.MatchString(property) || len(value) == 0 || len(value) > 1024 || !json.Valid(value) {
		return nil, domain.ErrInvalid
	}
	return json.Marshal(map[string]json.RawMessage{property: compact(value)})
}
