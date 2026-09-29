package tuya

import (
	"encoding/json"
	"strings"
	"testing"
)

func propertyOf(t *testing.T, tr Translation, code string) string {
	t.Helper()
	for name, ref := range tr.DPMap {
		if ref.Code == code {
			return name
		}
	}
	t.Fatalf("no property for %s", code)
	return ""
}

// Every translated data point carries its code, and CodeIndex agrees with the specification.
func TestTranslateKeepsCodes(t *testing.T) {
	for _, name := range []string{"kg_3gang.json", "cz_metering.json", "cl_curtain.json", "wk_thermostat.json", "wsdcg_sleeper.json"} {
		tr, dps, _ := spec(t, name)
		index := CodeIndex(dps)
		if len(index) != len(dps) {
			t.Fatalf("%s: index %d of %d", name, len(index), len(dps))
		}
		for prop, ref := range tr.DPMap {
			if !ValidCode(ref.Code) || index[ref.Code] != ref.DP {
				t.Fatalf("%s %s: %+v, index %d", name, prop, ref, index[ref.Code])
			}
		}
	}
	if len(CodeIndex(nil)) != 0 {
		t.Fatal("empty spec")
	}
	for _, bad := range []string{"", "a b", "switch-1", "x\x00", string(make([]byte, 65))} {
		if ValidCode(bad) {
			t.Fatalf("code %q accepted", bad)
		}
	}
}

func TestCloudWire(t *testing.T) {
	tr, _, _ := spec(t, "kg_3gang.json")
	countdown := propertyOf(t, tr, "countdown_1")
	relay := propertyOf(t, tr, "relay_status")
	for _, c := range []struct{ property, value, cloud, lan string }{
		{"switch_1", `"ON"`, `{"switch_1":true}`, `{"dps":{"1":true}}`},
		{"switch_3", `"OFF"`, `{"switch_3":false}`, `{"dps":{"3":false}}`},
		{countdown, `3600`, `{"countdown_1":3600}`, `{"dps":{"7":3600}}`},
		{relay, `"last"`, `{"relay_status":"last"}`, `{"dps":{"14":"last"}}`},
	} {
		got, e := CloudWire(tr.DPMap, c.property, json.RawMessage(c.value))
		if e != nil || string(got) != c.cloud {
			t.Fatalf("cloud %s %s: %s %v", c.property, c.value, got, e)
		}
		lan, e := Wire(tr.DPMap, c.property, json.RawMessage(c.value))
		if e != nil || string(lan) != c.lan {
			t.Fatalf("lan %s %s: %s %v", c.property, c.value, lan, e)
		}
	}
	// The value checks are Wire's.
	for _, c := range []struct{ property, value, reason string }{
		{"switch_1", `true`, "value"},
		{countdown, `86401`, "value"},
		{countdown, `1.5`, "value"},
		{relay, `"sometimes"`, "value"},
		{"nothing", `1`, "unknown_property"},
	} {
		if _, e := CloudWire(tr.DPMap, c.property, json.RawMessage(c.value)); reason(e) != c.reason {
			t.Fatalf("%s %s: %v", c.property, c.value, e)
		}
	}
	metering, _, _ := spec(t, "cz_metering.json")
	if _, e := CloudWire(metering.DPMap, propertyOf(t, metering, "cur_power"), json.RawMessage(`1`)); reason(e) != "not_settable" {
		t.Fatalf("measurement settable: %v", e)
	}
	// A dp map stored before cloud mode has no codes: the LAN path still works, the cloud path refuses.
	legacy := map[string]Ref{}
	for name, ref := range tr.DPMap {
		ref.Code = ""
		legacy[name] = ref
	}
	if _, e := CloudWire(legacy, "switch_1", json.RawMessage(`"ON"`)); reason(e) != "unknown_code" {
		t.Fatalf("codeless ref: %v", e)
	}
	if w, e := Wire(legacy, "switch_1", json.RawMessage(`"ON"`)); e != nil || string(w) != `{"dps":{"1":true}}` {
		t.Fatalf("legacy lan: %s %v", w, e)
	}
	// The stored dp map round-trips the code and omits it when absent.
	b, _ := json.Marshal(tr.DPMap["switch_1"])
	var back Ref
	if json.Unmarshal(b, &back) != nil || back.Code != "switch_1" {
		t.Fatalf("round trip %s", b)
	}
	if b, _ := json.Marshal(legacy["switch_1"]); strings.Contains(string(b), "code") {
		t.Fatalf("empty code serialised: %s", b)
	}
}
