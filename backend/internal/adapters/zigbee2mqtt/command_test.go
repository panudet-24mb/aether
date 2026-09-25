package zigbee2mqtt

import "testing"

func TestSetTopicAndPayload(t *testing.T) {
	topic, e := SetTopic("22222222-2222-4222-8222-222222222222", "0xa4c1380000000002")
	if e != nil || topic != "aether/z2m/22222222-2222-4222-8222-222222222222/0xa4c1380000000002/set" {
		t.Fatalf("topic: %q %v", topic, e)
	}
	for _, bad := range [][2]string{{"not-a-uuid", "0xa4c1380000000002"}, {"22222222-2222-4222-8222-222222222222", "ห้อง/ไฟ"}, {"22222222-2222-4222-8222-222222222222", "0xA4C1380000000002"}, {"22222222-2222-4222-8222-222222222222", "+"}} {
		if _, e := SetTopic(bad[0], bad[1]); e == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
	b, e := SetPayload("state_left", []byte(`"ON"`))
	if e != nil || string(b) != `{"state_left":"ON"}` {
		t.Fatalf("payload: %s %v", b, e)
	}
	if b, e := SetPayload("color", []byte(`{ "x": 0.3, "y": 0.4 }`)); e != nil || string(b) != `{"color":{"x":0.3,"y":0.4}}` {
		t.Fatalf("composite: %s %v", b, e)
	}
	for _, bad := range []struct {
		property string
		value    string
	}{{"state_l1\"}", `"ON"`}, {"", `"ON"`}, {"brightness", ``}, {"brightness", `{`}, {"bright ness", `1`}} {
		if _, e := SetPayload(bad.property, []byte(bad.value)); e == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
}
