package studio

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// Readings shaped like a UG65's codec output after Aether's pipeline: every value in metrics, states in values.
func milesightInput(metrics map[string]any, values map[string]any, history []any, events []any) []byte {
	input := map[string]any{
		"now": "2026-10-29T03:00:30Z", "source": "device",
		"data":    map[string]any{"kind": "environment", "model": "GS301", "received_at": "2026-10-29T02:55:00Z", "metrics": metrics, "values": values},
		"history": history, "events": events, "alerts": []any{},
	}
	b, _ := json.Marshal(input)
	return b
}

func TestMilesightWidgetsAreValid(t *testing.T) {
	seen := 0
	for _, item := range Official() {
		if item.Brand != "Milesight" {
			continue
		}
		seen++
		if e := Validate(item); e != nil {
			t.Fatalf("%s invalid: %v", item.ID, e)
		}
		var d Definition
		if e := json.Unmarshal(item.Definition, &d); e != nil || d.DecodeCode != "" || len(d.Kinds) == 0 {
			t.Fatalf("%s must consume canonical readings for a kind: %v", item.ID, e)
		}
	}
	if seen != 4 {
		t.Fatalf("expected 4 Milesight widgets, saw %d", seen)
	}
}

// Every Milesight widget renders real-shaped input inside the sandbox, degrades on empty input and escapes names.
func TestMilesightWidgetsRender(t *testing.T) {
	// The QuickJS runtime ships in the backend image; plain developer machines may not have it.
	if exec.Command("python3", "-c", "import quickjs").Run() != nil {
		t.Skip("python quickjs module unavailable; run inside the backend image")
	}
	hist := func(key string, vals ...float64) []any {
		out := []any{}
		for i, v := range vals {
			out = append(out, map[string]any{"received_at": "2026-10-29T0" + string(rune('0'+i)) + ":00:00Z", "metrics": map[string]any{key: v}})
		}
		return out
	}
	events := []any{
		map[string]any{"event_type": "occupied", "device_name": "<b>Stall</b>", "occurred_at": "2026-10-29T02:51:00Z"},
		map[string]any{"event_type": "leak", "device_name": "<b>Leak</b>", "occurred_at": "2026-10-28T10:00:00Z"},
	}
	cases := map[string][]struct {
		input []byte
		want  string
	}{
		"official-milesight-gs301-v1": {
			{milesightInput(map[string]any{"nh3": 0.4, "h2s": 0.003, "temperature": 29.1, "humidity": 78, "battery": 69}, nil, hist("nh3", 0.2, 0.3, 0.4), nil), "สะอาด"},
			{milesightInput(map[string]any{"nh3": 6.2, "h2s": 0.01}, nil, nil, nil), "กลิ่นแรง"},
			{milesightInput(map[string]any{"temperature": 29.1}, map[string]any{"nh3_sensor_status": "polarizing"}, nil, nil), "กำลังอุ่นเครื่อง"},
			{milesightInput(map[string]any{"temperature": 29.1}, map[string]any{"h2s_sensor_status": "device error"}, nil, nil), "เซนเซอร์ผิดพลาด"},
		},
		"official-milesight-ws202-v1": {
			{milesightInput(map[string]any{"motion": 1, "daylight": 0, "battery": 100}, nil, hist("motion", 0, 1, 1), events), "มีการเคลื่อนไหว"},
			{milesightInput(map[string]any{"motion": 0, "daylight": 1}, nil, nil, nil), "สว่าง"},
		},
		"official-milesight-vs330-v1": {
			{milesightInput(map[string]any{"motion": 1, "distance": 1234, "battery": 98}, map[string]any{"calibration_status": "success"}, hist("motion", 0, 1), events), "มีคนมาแล้ว"},
			{milesightInput(map[string]any{"motion": 0}, nil, nil, nil), "ว่าง"},
		},
		"official-milesight-ws303-v1": {
			{milesightInput(map[string]any{"leak": 1, "battery": 90}, nil, nil, events), "พบน้ำรั่ว"},
			{milesightInput(map[string]any{"leak": 0}, nil, nil, nil), "แห้ง · ปกติ"},
		},
	}
	for _, item := range Official() {
		tests, ok := cases[item.ID]
		if !ok {
			continue
		}
		delete(cases, item.ID)
		var d Definition
		_ = json.Unmarshal(item.Definition, &d)
		for _, c := range tests {
			out, e := Run(context.Background(), d.Code, "render", c.input)
			if e != nil {
				t.Fatalf("%s render failed: %v", item.ID, e)
			}
			var fields map[string]any
			_ = json.Unmarshal(out, &fields)
			markup := SafeHTML(fields["html"].(string))
			if !strings.Contains(markup, c.want) {
				t.Fatalf("%s: %q not in %s", item.ID, c.want, markup)
			}
			if strings.Contains(markup, "<b>Stall</b>") || strings.Contains(markup, "<b>Leak</b>") {
				t.Fatalf("%s did not escape device names", item.ID)
			}
		}
		if _, e := Run(context.Background(), d.Code, "render", []byte(`{"data":{},"history":[],"events":[],"alerts":[],"now":"2026-10-29T03:00:30Z"}`)); e != nil {
			t.Fatalf("%s failed on empty input: %v", item.ID, e)
		}
	}
	if len(cases) != 0 {
		t.Fatalf("widgets missing from Official(): %v", cases)
	}
}
