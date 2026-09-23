package tests

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

// The runner supplies a disposable Mosquitto broker, real provisioner and collector.
func mqttJourney(t *testing.T, f *fixture, api *fiber.App, token string, creds map[string]any, packet []map[string]any) func() {
	t.Helper()
	id := creds["gateway_id"].(string)
	wait := func(label string, ready func() bool) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if ready() {
				return
			}
			time.Sleep(250 * time.Millisecond)
		}
		t.Fatalf("timeout: %s", label)
	}
	waitApplied := func(revision float64) {
		wait("credential provisioning", func() bool {
			code, out := get(t, api, "/api/v1/gateways/mqtt-status", token)
			for _, row := range rows(out) {
				if code == 200 && row["gateway_id"] == id && row["applied_revision"] == revision {
					return true
				}
			}
			return false
		})
	}
	connect := func(c map[string]any, password string, allowed bool) mqtt.Client {
		t.Helper()
		client := mqtt.NewClient(mqtt.NewClientOptions().AddBroker(os.Getenv("MQTT_JOURNEY_BROKER")).SetClientID("journey-" + uuid.NewString()).SetUsername(c["username"].(string)).SetPassword(password).SetConnectTimeout(3 * time.Second).SetAutoReconnect(false))
		result := client.Connect()
		if !result.WaitTimeout(5 * time.Second) {
			t.Fatal("MQTT connect timed out")
		}
		if (result.Error() == nil) != allowed {
			t.Fatalf("unexpected MQTT authentication result (allowed=%t)", allowed)
		}
		t.Cleanup(func() { client.Disconnect(100) })
		return client
	}
	publish := func(client mqtt.Client, topic string, data any) {
		t.Helper()
		body, _ := json.Marshal(data)
		result := client.Publish(topic, 1, false, body)
		if !result.WaitTimeout(5*time.Second) || result.Error() != nil {
			t.Fatal("MQTT publish failed")
		}
	}
	waitApplied(1)
	client := connect(creds, creds["password"].(string), true)
	connect(creds, "incorrect-password", false)
	// Subscribe acceptance is not authorization: verify actual delivery is blocked.
	seen := make(chan bool, 1)
	sub := client.Subscribe("/aether/gateways/+/status", 1, func(mqtt.Client, mqtt.Message) {
		select {
		case seen <- true:
		default:
		}
	})
	if !sub.WaitTimeout(5*time.Second) || sub.Error() != nil {
		t.Fatal("subscribe failed")
	}
	publish(client, creds["post_topic"].(string), packet)
	wait("collector persisted MQTT packet", func() bool {
		code, out := get(t, api, "/api/v1/studio/sources", token)
		return code == 200 && values(out, "key")[id+"/"+packet[0]["mac"].(string)]
	})
	select {
	case <-seen:
		t.Fatal("gateway received status data without read permission")
	case <-time.After(time.Second):
	}
	// An existing gateway's topic must reject writes from this gateway.
	code, other, _ := req(t, api, "POST", "/api/v1/gateways", "Bearer "+token, "", "", map[string]any{"name": "ACL target", "model": "minew-mg3"})
	if code != 201 {
		t.Fatal("create ACL target")
	}
	otherID := other["gateway"].(map[string]any)["id"].(string)
	code, _, _ = req(t, api, "POST", "/api/v1/gateways/"+otherID+"/mqtt", "Bearer "+token, "", "", map[string]any{})
	if code != 201 {
		t.Fatal("enroll ACL target")
	}
	publish(client, "/aether/gateways/"+otherID+"/status", packet)
	time.Sleep(time.Second)
	code, out := get(t, api, "/api/v1/studio/sources", token)
	if code != 200 || values(out, "key")[otherID+"/"+packet[0]["mac"].(string)] {
		t.Fatal("cross-gateway write ACL failed")
	}
	t.Log("PASS MQTT login, wrong-password rejection, own publish, denied status read and cross-gateway publish")
	return func() {
		code, rotated, _ := req(t, api, "POST", "/api/v1/gateways/"+id+"/mqtt/rotate", "Bearer "+token, "", "", map[string]any{})
		if code != 201 {
			t.Fatal("rotate credentials")
		}
		waitApplied(2)
		connect(creds, creds["password"].(string), false)
		connect(rotated, rotated["password"].(string), true)
		code, _, _ = req(t, api, "POST", "/api/v1/gateways/"+id+"/revoke", "Bearer "+token, "", "", map[string]any{})
		if code != 204 {
			t.Fatalf("revoke: %d", code)
		}
		// The worker refreshes broker files every three seconds.
		time.Sleep(4 * time.Second)
		connect(rotated, rotated["password"].(string), false)
		t.Log("PASS MQTT rotated credentials and revoked-account connection rejection")
	}
}
