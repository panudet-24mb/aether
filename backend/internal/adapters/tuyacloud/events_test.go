package tuyacloud

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// Synthetic business messages, shaped per Tuya "Message Types" (developer.tuya.com/en/docs/iot/message-type,
// read 2026-09-30). None is a capture from a real project; G0 replaces or confirms them.
const devIDMixed = "BF1111111111111111AA01"

func one(t *testing.T, protocol int, msg string) Event {
	t.Helper()
	events, e := Parse(protocol, []byte(msg))
	if e != nil || len(events) != 1 {
		t.Fatalf("protocol %d %s: %+v %v", protocol, msg, events, e)
	}
	if events[0].DevID != "bf1111111111111111aa01" || events[0].Protocol != protocol {
		t.Fatalf("protocol %d: %+v", protocol, events[0])
	}
	return events[0]
}

func TestStatusReport(t *testing.T) {
	ev := one(t, ProtocolStatus, `{"dataId":"AAXI3c1i6xxx0001","devId":"`+devIDMixed+`","productKey":"awgmk9pixxx00001",
		"status":[{"code":"switch_1","value":true,"t":1790000000123,"1":"true"},
		          {"code":"cur_power","value":"1234","t":"1790000000200","19":"1234"},
		          {"code":"relay_status","value":"last"}]}`)
	if ev.Kind != EventStatus || ev.ProductID != "awgmk9pixxx00001" || ev.T != 1790000000200 || len(ev.Items) != 3 {
		t.Fatalf("%+v", ev)
	}
	want := []Item{{"switch_1", 1, json.RawMessage(`true`), 1790000000123}, {"cur_power", 19, json.RawMessage(`"1234"`), 1790000000200},
		{"relay_status", 0, json.RawMessage(`"last"`), 0}}
	for i, w := range want {
		got := ev.Items[i]
		if got.Code != w.Code || got.DPID != w.DPID || string(got.Value) != string(w.Value) || got.T != w.T {
			t.Fatalf("item %d: %+v, want %+v", i, got, w)
		}
	}
}

func TestLegacyDeviceEvents(t *testing.T) {
	for code, kind := range map[string]EventKind{"online": EventOnline, "offline": EventOffline, "delete": EventRemoved, "bindUser": EventBound} {
		ev := one(t, ProtocolDevice, `{"devId":"`+devIDMixed+`","productKey":"awgmk9pixxx00001","bizCode":"`+code+`","bizData":{"time":1790000000},"ts":1790000000456}`)
		if ev.Kind != kind || ev.T != 1790000000456 || ev.Items != nil {
			t.Fatalf("%s: %+v", code, ev)
		}
	}
	// The device id may sit in bizData, and bizData's time stands in for a missing ts.
	ev := one(t, ProtocolDevice, `{"bizCode":"nameUpdate","bizData":{"devId":"`+devIDMixed+`","name":"  Kitchen ไฟ  ","time":1790000000}}`)
	if ev.Kind != EventRenamed || ev.Name != "Kitchen ไฟ" || ev.T != 1790000000 {
		t.Fatalf("rename: %+v", ev)
	}
	for _, code := range []string{"dpNameUpdate", "upgradeStatus", "somethingNew"} {
		events, e := Parse(ProtocolDevice, []byte(`{"devId":"`+devIDMixed+`","bizCode":"`+code+`","bizData":{}}`))
		if e != nil || events == nil || len(events) != 0 {
			t.Fatalf("%s: %+v %v", code, events, e)
		}
	}
}

func TestIoTCoreMessages(t *testing.T) {
	ev := one(t, ProtocolProperty, `{"bizCode":"devicePropertyMessage","bizData":{"devId":"`+devIDMixed+`","productId":"awgmk9pixxx00001",
		"dataId":"x","properties":[{"code":"switch_1","dpId":1,"time":1790000000123,"value":true},{"code":"countdown_1","dpId":"7","time":1790000000124,"value":60}]},
		"ts":1790000000999}`)
	if ev.Kind != EventStatus || ev.T != 1790000000999 || len(ev.Items) != 2 || ev.Items[1].DPID != 7 || ev.Items[1].T != 1790000000124 || string(ev.Items[1].Value) != "60" {
		t.Fatalf("%+v", ev)
	}
	if events, e := Parse(ProtocolProperty, []byte(`{"bizCode":"deviceEventMessage","bizData":{"devId":"`+devIDMixed+`","outputParams":{}}}`)); e != nil || len(events) != 0 {
		t.Fatalf("device event: %+v %v", events, e)
	}
	for code, kind := range map[string]EventKind{"deviceOnline": EventOnline, "deviceOffline": EventOffline, "deviceDelete": EventRemoved, "deviceBindSpace": EventBound} {
		ev := one(t, ProtocolDeviceIoTCore, `{"bizCode":"`+code+`","bizData":{"devId":"`+devIDMixed+`","productId":"awgmk9pixxx00001","time":1790000000},"ts":1790000000456}`)
		if ev.Kind != kind || ev.T != 1790000000456 || ev.ProductID != "awgmk9pixxx00001" {
			t.Fatalf("%s: %+v", code, ev)
		}
	}
	ev = one(t, ProtocolDeviceIoTCore, `{"bizCode":"deviceNameUpdate","bizData":{"devId":"`+devIDMixed+`","name":"Hall"},"ts":1790000000456}`)
	if ev.Kind != EventRenamed || ev.Name != "Hall" {
		t.Fatalf("rename: %+v", ev)
	}
	if events, e := Parse(ProtocolDeviceIoTCore, []byte(`{"bizCode":"deviceDpInfoUpdate","bizData":{}}`)); e != nil || len(events) != 0 {
		t.Fatalf("unused 1001 code: %+v %v", events, e)
	}
}

func TestUnknownProtocol(t *testing.T) {
	for _, p := range []int{0, 1, 23, 999, 1002} {
		if _, e := Parse(p, []byte(`{"devId":"`+devIDMixed+`"}`)); !errors.Is(e, ErrUnknownProtocol) {
			t.Fatalf("protocol %d: %v", p, e)
		}
	}
}

func TestEventBounds(t *testing.T) {
	status := func(item string) string {
		return `{"devId":"` + devIDMixed + `","status":[` + item + `]}`
	}
	var many []string
	for i := 0; i <= 128; i++ {
		many = append(many, fmt.Sprintf(`{"code":"c%d","value":%d}`, i, i))
	}
	bad := map[string]struct {
		protocol int
		msg      string
	}{
		"not json":          {ProtocolStatus, `{`},
		"no status":         {ProtocolStatus, `{"devId":"` + devIDMixed + `","status":[]}`},
		"129 items":         {ProtocolStatus, status(strings.Join(many, ","))},
		"value too big":     {ProtocolStatus, status(`{"code":"x","value":"` + strings.Repeat("a", MaxValue) + `"}`)},
		"missing value":     {ProtocolStatus, status(`{"code":"x"}`)},
		"bad code":          {ProtocolStatus, status(`{"code":"a b","value":1}`)},
		"code not text":     {ProtocolStatus, status(`{"code":1,"value":1}`)},
		"negative time":     {ProtocolStatus, status(`{"code":"x","value":1,"t":-5}`)},
		"short dev id":      {ProtocolStatus, `{"devId":"bf11","status":[{"code":"x","value":1}]}`},
		"dev id symbols":    {ProtocolStatus, `{"devId":"bf1111111111111111aa0/","status":[{"code":"x","value":1}]}`},
		"nul":               {ProtocolStatus, status(`{"code":"x","value":"a\u0000"}`) + "\x00"},
		"invalid utf8":      {ProtocolStatus, status(`{"code":"x","value":"` + "\xff" + `"}`)},
		"oversize":          {ProtocolStatus, strings.Repeat(" ", MaxData+1)},
		"no dev id":         {ProtocolDevice, `{"bizCode":"online","bizData":{}}`},
		"name control":      {ProtocolDevice, `{"devId":"` + devIDMixed + `","bizCode":"nameUpdate","bizData":{"name":"a\u0007b"}}`},
		"name empty":        {ProtocolDevice, `{"devId":"` + devIDMixed + `","bizCode":"nameUpdate","bizData":{"name":"  "}}`},
		"name long":         {ProtocolDeviceIoTCore, `{"bizCode":"deviceNameUpdate","bizData":{"devId":"` + devIDMixed + `","name":"` + strings.Repeat("ไ", 129) + `"}}`},
		"bizData not obj":   {ProtocolDeviceIoTCore, `{"bizCode":"deviceOnline","bizData":"x"}`},
		"long bizCode":      {ProtocolDevice, `{"bizCode":"` + strings.Repeat("b", 65) + `"}`},
		"no properties":     {ProtocolProperty, `{"bizCode":"devicePropertyMessage","bizData":{"devId":"` + devIDMixed + `","properties":[]}}`},
		"property bad json": {ProtocolProperty, `{"bizCode":"devicePropertyMessage","bizData":{"devId":"` + devIDMixed + `","properties":[{"code":"x"}]}}`},
	}
	for name, c := range bad {
		if events, e := Parse(c.protocol, []byte(c.msg)); !errors.Is(e, ErrFrame) || events != nil {
			t.Fatalf("%s: %+v %v", name, events, e)
		}
	}
	// A data point id outside 1..255 or a foreign product id is dropped, not trusted; the item itself stays.
	ev := one(t, ProtocolStatus, `{"devId":"`+devIDMixed+`","productKey":"../etc","status":[{"code":"x","value":1,"300":"1"}]}`)
	if ev.Items[0].DPID != 0 || ev.ProductID != "" {
		t.Fatalf("%+v", ev)
	}
	// 128 items is the limit, not over it.
	if _, e := Parse(ProtocolStatus, []byte(status(strings.Join(many[:128], ",")))); e != nil {
		t.Fatalf("128 items: %v", e)
	}
}
