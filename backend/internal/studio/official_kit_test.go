package studio

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// Every official kit widget must validate and render real-shaped input inside the sandbox.
func TestKitWidgetsRender(t *testing.T) {
	// The QuickJS runtime ships in the backend image; plain developer machines may not have it.
	if exec.Command("python3", "-c", "import quickjs").Run() != nil {
		t.Skip("python quickjs module unavailable; run inside the backend image")
	}
	input := map[string]any{
		"now":    "2026-09-20T03:00:30Z",
		"source": "simulated",
		"data": map[string]any{"kind": "motion", "model": "E8S", "battery": 91, "rssi": -71, "received_at": "2026-09-20T03:00:25Z",
			"metrics": map[string]any{"accel_g": 1.02, "vibration": 1, "tamper": 1},
			"beacon":  map[string]any{"type": "eddystone_uid", "namespace": "ae7e5100000000000001", "instance": "00000001f915", "voltage": 3.9}},
		"history": []any{
			map[string]any{"metrics": map[string]any{"accel_g": 0.99}, "beacon": map[string]any{"instance": "000000000007"}},
			map[string]any{"metrics": map[string]any{"accel_g": 1.2}, "beacon": map[string]any{"instance": "000000000007"}},
			map[string]any{"metrics": map[string]any{"accel_g": 1.02}, "beacon": map[string]any{"instance": "00000001f915"}},
		},
		"events": []any{map[string]any{"event_type": "tamper", "device_name": "<b>MBT01</b>", "occurred_at": "2026-09-20T02:58:00Z"}, map[string]any{"event_type": "button", "device_name": "B10", "occurred_at": "2026-09-20T02:59:00Z"}},
		"alerts": []any{map[string]any{"title": "MBT01 · ป้ายถูกถอด", "severity": "critical", "opened_at": "2026-09-20T02:58:00Z"}},
	}
	payload, _ := json.Marshal(input)
	want := map[string]string{"official-motion-v1": "กำลังเคลื่อนไหว", "official-tamper-v1": "ป้ายถูกถอด", "official-button-v1": "กำลังกดปุ่ม", "official-beacon-v1": "00f915", "official-events-v1": "ป้ายถูกถอด", "official-alerts-v1": "OPEN ALERTS · 1"}
	seen := 0
	for _, item := range Official() {
		expect, ok := want[item.ID]
		if !ok {
			continue
		}
		seen++
		if e := Validate(item); e != nil {
			t.Fatalf("%s invalid: %v", item.ID, e)
		}
		var d Definition
		if e := json.Unmarshal(item.Definition, &d); e != nil || d.DecodeCode != "" {
			t.Fatalf("%s must consume canonical readings: %v", item.ID, e)
		}
		out, e := Run(context.Background(), d.Code, "render", payload)
		if e != nil {
			t.Fatalf("%s render failed: %v", item.ID, e)
		}
		var fields map[string]any
		_ = json.Unmarshal(out, &fields)
		markup := SafeHTML(fields["html"].(string))
		if !strings.Contains(markup, expect) {
			t.Fatalf("%s: %q not in %s", item.ID, expect, markup)
		}
		if strings.Contains(markup, "<b>MBT01</b>") {
			t.Fatalf("%s did not escape device names", item.ID)
		}
		// Empty input must degrade to a message, never throw.
		if _, e := Run(context.Background(), d.Code, "render", []byte(`{"data":{},"history":[],"events":[],"alerts":[],"now":"2026-09-20T03:00:30Z"}`)); e != nil {
			t.Fatalf("%s failed on empty input: %v", item.ID, e)
		}
	}
	if seen != len(want) {
		t.Fatalf("expected %d kit widgets, saw %d", len(want), seen)
	}
}
