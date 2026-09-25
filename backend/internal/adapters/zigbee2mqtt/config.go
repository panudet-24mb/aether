package zigbee2mqtt

import (
	"strconv"
	"strings"
)

// Server is the MQTT URL Zigbee2MQTT connects to: mqtts:// for the TLS listener (every production deployment),
// mqtt:// only for a development broker without TLS.
func Server(tls bool, host string, port int) string {
	scheme := "mqtt://"
	if tls {
		scheme = "mqtts://"
	}
	return scheme + host + ":" + strconv.Itoa(port)
}

// ConfigSnippet is the part of Zigbee2MQTT's configuration.yaml the operator pastes on the site host. The password
// is the one just issued (shown once). Home Assistant discovery stays off because its topics are outside the
// gateway's ACL; availability must be on, because it is how Aether tells an idle wall switch from a dead one; the
// bridge/health heartbeat (Zigbee2MQTT 2.x) is how it tells a quiet site from a dead one (postgres.Z2MSilentAfter).
func ConfigSnippet(tls bool, host string, port int, gatewayID, password string) string {
	user := "gw-" + gatewayID
	lines := []string{
		"# Aether · requires Zigbee2MQTT 2.x or newer",
		"mqtt:",
		"  server: " + Server(tls, host, port),
		"  base_topic: " + BaseTopic(gatewayID),
		"  user: " + user,
		"  password: '" + strings.ReplaceAll(password, "'", "''") + "'",
		"  client_id: " + user,
	}
	if tls {
		lines = append(lines, "  ca: /app/data/aether-ca.crt   # copy the broker CA (ca.crt) here", "  reject_unauthorized: true")
	}
	lines = append(lines,
		"  keepalive: 60",
		"  version: 4",
		"  include_device_information: true",
		"homeassistant:",
		"  enabled: false",
		"availability:",
		"  enabled: true",
		"health:",
		"  interval: 10",
	)
	return strings.Join(lines, "\n") + "\n"
}
