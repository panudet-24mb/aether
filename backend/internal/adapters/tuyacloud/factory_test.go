package tuyacloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

// Factory records are read FactoryBatch ids per call, only for well-formed ids; a failing batch fails the call.
func TestFactoryInfosBatches(t *testing.T) {
	var batches [][]string
	fail := false
	f, c, done := newFake(t, func(path string, q url.Values) (int, string) {
		if code, body, ok := okToken(path); ok {
			return code, body
		}
		if path != "/v1.0/iot-03/devices/factory-infos" {
			return 404, `{}`
		}
		if fail {
			return 200, `{"success":false,"code":1106,"msg":"permission deny"}`
		}
		ids := strings.Split(q.Get("device_ids"), ",")
		batches = append(batches, ids)
		list := []map[string]string{}
		for _, id := range ids {
			list = append(list, map[string]string{"id": id, "uuid": "u" + id[len(id)-4:], "mac": "DC234D00" + id[len(id)-4:], "sn": "sn"})
		}
		b, _ := json.Marshal(map[string]any{"success": true, "result": list})
		return 200, string(b)
	})
	defer done()
	ctx := context.Background()
	if e := c.Authenticate(ctx); e != nil {
		t.Fatal(e)
	}
	ids := []string{"bad id!", "short"}
	for i := 0; i < 25; i++ {
		ids = append(ids, fmt.Sprintf("bf000000000000%06d", i))
	}
	out, e := c.FactoryInfos(ctx, ids)
	if e != nil {
		t.Fatal(e)
	}
	if len(batches) != 2 || len(batches[0]) != FactoryBatch || len(batches[1]) != 5 || len(out) != 25 {
		t.Fatalf("batches %d (%d, %d), %d records", len(batches), len(batches[0]), len(batches[1]), len(out))
	}
	for _, b := range batches {
		for _, id := range b {
			if !deviceIDPattern.MatchString(id) {
				t.Fatalf("malformed id sent: %q", id)
			}
		}
	}
	if out[0].MAC != "DC234D000000" || out[0].UUID != "u0000" {
		t.Fatalf("record: %+v", out[0])
	}
	fail = true
	if _, e := c.FactoryInfos(ctx, ids); !errors.Is(e, ErrPermission) {
		t.Fatalf("refused batch: %v", e)
	}
	if f.badSigns != 0 {
		t.Fatalf("%d requests failed signature verification", f.badSigns)
	}
}

// The listing's uuid and sec_key are read; the secKey spelling some listings use lands in SecKey too.
func TestDevicesCarryBLEIdentity(t *testing.T) {
	_, c, done := newFake(t, func(path string, q url.Values) (int, string) {
		if code, body, ok := okToken(path); ok {
			return code, body
		}
		if path == "/v1.0/iot-01/associated-users/devices" {
			return 200, `{"success":true,"result":{"has_more":false,"devices":[
  {"id":"bf0000000000000bt01","name":"a","local_key":"FAKEKEY-bt01-000","uuid":"uuidbt01","sec_key":"SECKEY-bt01-0000"},
  {"id":"bf0000000000000bt02","name":"b","local_key":"FAKEKEY-bt02-000","uuid":"uuidbt02","secKey":"SECKEY-bt02-0000"}]}}`
		}
		return 404, `{}`
	})
	defer done()
	ctx := context.Background()
	if e := c.Authenticate(ctx); e != nil {
		t.Fatal(e)
	}
	devices, e := c.Devices(ctx)
	if e != nil || len(devices) != 2 {
		t.Fatalf("%v %v", devices, e)
	}
	if devices[0].UUID != "uuidbt01" || devices[0].SecKey != "SECKEY-bt01-0000" || devices[1].SecKey != "SECKEY-bt02-0000" || devices[1].SecKeyCamel != "" {
		t.Fatalf("devices: %+v", devices)
	}
}
