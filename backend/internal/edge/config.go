// Package edge is the Aether Edge agent: it runs on a site host (a Raspberry Pi next to the Tuya Wi-Fi devices),
// talks to each registered device over the LAN with its local key (internal/tuyalocal) and connects OUT to the
// Aether broker with its gateway's own account. It moves raw Tuya data points only; what they mean is known to the
// server (docs/platform/aether-edge.md).
//
// The package must stay free of server code (database, HTTP framework, domain/security packages): it is compiled
// into a small image that runs on customer hardware. TestAgentDependencies enforces that.
package edge

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Version is the agent's version, set at build time with -ldflags "-X aether/backend/internal/edge.Version=…".
var Version = "dev"

var (
	uuidPattern   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	tokenPattern  = regexp.MustCompile(`^[A-Za-z0-9_-]{16,256}$`)
	devicePattern = regexp.MustCompile(`^[a-z0-9]{16,32}$`)
)

// Config is the agent's environment. Secrets (HTTPToken, MQTTPassword) are never logged; String redacts them.
type Config struct {
	API          string // Aether's public origin, https://…
	GatewayID    string
	HTTPToken    string
	MQTTURL      string // mqtts://host:8883 (mqtt:// only for a development broker)
	MQTTUsername string
	MQTTPassword string
	MQTTCAFile   string
	WebCAFile    string // optional extra root for the HTTPS configuration pull (AETHER_TLS=internal)
	LogLevel     string
	HealthFile   string // written on every heartbeat; `aether-edge health` checks its age
}

// LoadConfig reads and validates the agent's environment through getenv.
func LoadConfig(getenv func(string) string) (Config, error) {
	c := Config{
		API:          strings.TrimRight(strings.TrimSpace(getenv("AETHER_API")), "/"),
		GatewayID:    strings.ToLower(strings.TrimSpace(getenv("GATEWAY_ID"))),
		HTTPToken:    strings.TrimSpace(getenv("HTTP_TOKEN")),
		MQTTURL:      strings.TrimSpace(getenv("MQTT_URL")),
		MQTTUsername: strings.TrimSpace(getenv("MQTT_USERNAME")),
		MQTTPassword: getenv("MQTT_PASSWORD"),
		MQTTCAFile:   strings.TrimSpace(getenv("MQTT_CA_FILE")),
		WebCAFile:    strings.TrimSpace(getenv("WEB_CA_FILE")),
		LogLevel:     strings.ToLower(strings.TrimSpace(getenv("LOG_LEVEL"))),
		HealthFile:   strings.TrimSpace(getenv("HEALTH_FILE")),
	}
	if c.HealthFile == "" {
		c.HealthFile = DefaultHealthFile
	}
	if c.MQTTUsername == "" {
		c.MQTTUsername = "gw-" + c.GatewayID
	}
	var problems []string
	api, e := url.Parse(c.API)
	if e != nil || (api.Scheme != "https" && api.Scheme != "http") || api.Host == "" || api.Path != "" || api.RawQuery != "" {
		problems = append(problems, "AETHER_API must be an origin such as https://aether.example.com")
	}
	if !uuidPattern.MatchString(c.GatewayID) {
		problems = append(problems, "GATEWAY_ID must be the gateway's UUID")
	}
	if !tokenPattern.MatchString(c.HTTPToken) {
		problems = append(problems, "HTTP_TOKEN is missing or malformed")
	}
	if _, e := brokerURL(c.MQTTURL); e != nil {
		problems = append(problems, "MQTT_URL must be mqtts://host:port (or mqtt:// for a development broker)")
	}
	if c.MQTTUsername != "gw-"+c.GatewayID {
		problems = append(problems, "MQTT_USERNAME must be gw-<GATEWAY_ID>")
	}
	if c.MQTTPassword == "" {
		problems = append(problems, "MQTT_PASSWORD is missing")
	}
	if strings.HasPrefix(c.MQTTURL, "mqtts://") && c.MQTTCAFile == "" {
		problems = append(problems, "MQTT_CA_FILE is required for mqtts://")
	}
	switch c.LogLevel {
	case "", "info", "debug", "warn", "error":
	default:
		problems = append(problems, "LOG_LEVEL must be debug, info, warn or error")
	}
	if len(problems) > 0 {
		return Config{}, errors.New(strings.Join(problems, "; "))
	}
	return c, nil
}

// String describes the configuration without its secrets.
func (c Config) String() string {
	return fmt.Sprintf("api=%s gateway=%s mqtt=%s user=%s ca=%q web_ca=%q", c.API, c.GatewayID, c.MQTTURL, c.MQTTUsername, c.MQTTCAFile, c.WebCAFile)
}

// brokerURL turns mqtts://host:port into paho's ssl://host:port (mqtt:// into tcp://).
func brokerURL(s string) (string, error) {
	u, e := url.Parse(s)
	if e != nil || u.Host == "" || u.Port() == "" || u.Path != "" || u.User != nil {
		return "", errors.New("bad broker URL")
	}
	switch u.Scheme {
	case "mqtts":
		return "ssl://" + u.Host, nil
	case "mqtt":
		return "tcp://" + u.Host, nil
	}
	return "", errors.New("bad broker scheme")
}

// Topics of one gateway's tree (adapters/edge on the server reads them).
func topicStatus(gw string) string     { return "aether/edge/" + gw + "/status" }
func topicHealth(gw string) string     { return "aether/edge/" + gw + "/health" }
func topicDiscovery(gw string) string  { return "aether/edge/" + gw + "/discovery" }
func topicState(gw, dev string) string { return "aether/edge/" + gw + "/" + dev + "/state" }
func topicAvailability(gw, dev string) string {
	return "aether/edge/" + gw + "/" + dev + "/availability"
}
func topicSetFilter(gw string) string { return "aether/edge/" + gw + "/+/set" }

// ValidDevice reports a Tuya device id: 16 to 32 lower-case letters and digits.
func ValidDevice(s string) bool { return devicePattern.MatchString(s) }
