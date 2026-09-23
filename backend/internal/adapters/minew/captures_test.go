package minew

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Golden captures: real advertisements recorded from a tag on the bench by infra/capture-golden.py.
// A file without an "expect" block must still decode without panicking; the test then prints a
// ready-to-paste expect block, which the operator confirms by eye against what the device shows and
// commits. Only then may the profile be marked Verified in backend/internal/domain/catalog.go.
type captureRow struct {
	MAC  string `json:"mac"`
	Raw  string `json:"rawData"`
	RSSI *int   `json:"rssi"`
}

type captureBeacon struct {
	Type      string `json:"type,omitempty"`
	UUID      string `json:"uuid,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Instance  string `json:"instance,omitempty"`
	Major     *int   `json:"major,omitempty"`
	Minor     *int   `json:"minor,omitempty"`
}

// Ranges are inclusive [min,max] pairs, so a capture of a live sensor does not need an exact value.
type captureExpect struct {
	Kind        string               `json:"kind,omitempty"`
	Model       string               `json:"model,omitempty"`
	Frames      []string             `json:"frames,omitempty"`
	Unknown     []string             `json:"unknown,omitempty"`
	Battery     []float64            `json:"battery,omitempty"`
	Temperature []float64            `json:"temperature,omitempty"`
	Humidity    []float64            `json:"humidity,omitempty"`
	Metrics     map[string][]float64 `json:"metrics,omitempty"`
	Beacon      *captureBeacon       `json:"beacon,omitempty"`
}

type captureFile struct {
	Label        string         `json:"label"`
	Synthetic    bool           `json:"synthetic"`
	Anonymized   bool           `json:"anonymized"`
	CapturedAt   string         `json:"captured_at"`
	GatewayModel string         `json:"gateway_model"`
	Note         string         `json:"note"`
	MAC          string         `json:"mac"`
	Rows         []captureRow   `json:"rows"`
	Expect       *captureExpect `json:"expect"`
}

func TestRealCaptures(t *testing.T) {
	files, e := filepath.Glob("testdata/real/*.json")
	if e != nil {
		t.Fatal(e)
	}
	if len(files) == 0 {
		t.Fatal("no capture files: at least the synthetic example must ship")
	}
	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			blob, e := os.ReadFile(path)
			if e != nil {
				t.Fatal(e)
			}
			var f captureFile
			if e := json.Unmarshal(blob, &f); e != nil {
				t.Fatalf("%s is not a capture file: %v", path, e)
			}
			if len(f.Rows) == 0 {
				t.Fatal("capture has no rows")
			}
			merged := Reading{}
			decoded := 0
			for i, row := range f.Rows {
				mac := row.MAC
				if mac == "" {
					mac = f.MAC
				}
				r, ok := DecodeFramesFor(row.Raw, mac)
				if hasUnknownPrefix(r.Unknown, "ffe1:a1:") && hasUnknownSuffix(r.Unknown, ":mac-mismatch") && !declared(f.Expect, ":mac-mismatch") {
					// The frame's own MAC disagrees with the row's address. In an anonymised file that
					// means the rewrite was not applied consistently and the golden is worthless.
					t.Fatalf("row %d: MAC in the frame does not match %s (%v)", i, mac, r.Unknown)
				}
				if !ok {
					continue
				}
				decoded++
				merged.Merge(r)
			}
			if decoded == 0 {
				t.Fatalf("no row decoded; Aether understands nothing this tag sent. Unknown descriptors are in the live view, keep the file and do not guess")
			}
			if f.Expect == nil {
				t.Logf("ไม่มีบล็อก expect ในไฟล์นี้ ตรวจค่าด้วยตาแล้วคัดลอกบล็อกนี้ลงไป:\n%s", suggestExpect(merged))
				return
			}
			checkExpect(t, *f.Expect, merged)
		})
	}
}

func hasUnknownPrefix(list []string, p string) bool {
	for _, u := range list {
		if strings.HasPrefix(u, p) {
			return true
		}
	}
	return false
}

func hasUnknownSuffix(list []string, s string) bool {
	for _, u := range list {
		if strings.HasSuffix(u, s) {
			return true
		}
	}
	return false
}

func declared(e *captureExpect, suffix string) bool {
	return e != nil && hasUnknownSuffix(e.Unknown, suffix)
}

func checkExpect(t *testing.T, want captureExpect, got Reading) {
	t.Helper()
	if want.Kind != "" && got.Kind != want.Kind {
		t.Errorf("kind=%q want %q", got.Kind, want.Kind)
	}
	if want.Model != "" && got.Model != want.Model {
		t.Errorf("model=%q want %q", got.Model, want.Model)
	}
	if len(want.Frames) > 0 {
		a, b := append([]string(nil), got.Frames...), append([]string(nil), want.Frames...)
		sort.Strings(a)
		sort.Strings(b)
		if strings.Join(a, ",") != strings.Join(b, ",") {
			t.Errorf("frames=%v want %v", a, b)
		}
	}
	a, b := append([]string(nil), got.Unknown...), append([]string(nil), want.Unknown...)
	sort.Strings(a)
	sort.Strings(b)
	if strings.Join(a, ",") != strings.Join(b, ",") {
		t.Errorf("unknown=%v want %v", a, b)
	}
	inRange(t, "battery", float64(got.Battery), want.Battery)
	inRange(t, "temperature", got.Temperature, want.Temperature)
	inRange(t, "humidity", got.Humidity, want.Humidity)
	for name, bounds := range want.Metrics {
		v, ok := got.Metrics[name]
		if !ok {
			t.Errorf("metric %s missing (have %v)", name, got.Metrics)
			continue
		}
		inRange(t, name, v, bounds)
	}
	if want.Beacon != nil {
		if got.Beacon == nil {
			t.Fatalf("beacon missing, want %+v", *want.Beacon)
		}
		for _, p := range []struct{ name, got, want string }{
			{"type", got.Beacon.Type, want.Beacon.Type},
			{"uuid", got.Beacon.UUID, want.Beacon.UUID},
			{"namespace", got.Beacon.Namespace, want.Beacon.Namespace},
			{"instance", got.Beacon.Instance, want.Beacon.Instance},
		} {
			if p.want != "" && p.got != p.want {
				t.Errorf("beacon %s=%q want %q", p.name, p.got, p.want)
			}
		}
		if want.Beacon.Major != nil && got.Beacon.Major != *want.Beacon.Major {
			t.Errorf("beacon major=%d want %d", got.Beacon.Major, *want.Beacon.Major)
		}
		if want.Beacon.Minor != nil && got.Beacon.Minor != *want.Beacon.Minor {
			t.Errorf("beacon minor=%d want %d", got.Beacon.Minor, *want.Beacon.Minor)
		}
	}
}

func inRange(t *testing.T, name string, v float64, bounds []float64) {
	t.Helper()
	if len(bounds) != 2 {
		return
	}
	if v < bounds[0] || v > bounds[1] {
		t.Errorf("%s=%v outside %v", name, v, bounds)
	}
}

// suggestExpect renders what was actually decoded as an expect block, with a little slack on every
// numeric value so a second capture of the same scenario does not fail on noise.
func suggestExpect(r Reading) string {
	e := captureExpect{Kind: r.Kind, Model: r.Model, Frames: append([]string(nil), r.Frames...), Unknown: append([]string(nil), r.Unknown...)}
	sort.Strings(e.Frames)
	if r.Battery > 0 {
		e.Battery = []float64{math.Max(0, float64(r.Battery)-10), math.Min(100, float64(r.Battery)+5)}
	}
	if hasFrame(r.Frames, FrameTH) || hasFrame(r.Frames, FrameTemp) || hasFrame(r.Frames, FrameTHTemp) {
		e.Temperature = slack(r.Temperature, 3)
	}
	if hasFrame(r.Frames, FrameTH) {
		e.Humidity = slack(r.Humidity, 10)
	}
	if len(r.Metrics) > 0 {
		e.Metrics = map[string][]float64{}
		for k, v := range r.Metrics {
			e.Metrics[k] = slack(v, math.Max(0.05, math.Abs(v)*0.25))
		}
	}
	if r.Beacon != nil {
		major, minor := r.Beacon.Major, r.Beacon.Minor
		e.Beacon = &captureBeacon{Type: r.Beacon.Type, UUID: r.Beacon.UUID, Namespace: r.Beacon.Namespace, Instance: r.Beacon.Instance, Major: &major, Minor: &minor}
	}
	b, _ := json.MarshalIndent(map[string]captureExpect{"expect": e}, "", "  ")
	return fmt.Sprintf("%s", b)
}

func slack(v, d float64) []float64 {
	return []float64{math.Round((v-d)*1000) / 1000, math.Round((v+d)*1000) / 1000}
}
