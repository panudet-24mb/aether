package tests

import (
	"aether/backend/internal/adapters/postgres"
	"context"
	"testing"
)

// The physical S1 (reports "PLUS") broadcasts accelerometer and iBeacon slots every second next to its
// temperature frame. Thinning used to measure SAMPLE_MIN_INTERVAL_SEC from ANY stored sample, so those
// slots kept the timer fresh and no temperature was ever stored in production.
func TestThinningKeepsTemperatureNextToChattyFrames(t *testing.T) {
	f := setup(t)
	f.repo.Configure(postgres.Options{SampleMinIntervalSec: 30, DiscoveryLimit: 100})
	_, _, a := f.account(t)
	ctx := context.Background()
	g, _, e := f.service.CreateGateway(ctx, a, "Thinning", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	// Frames captured from c30000393fe5 on the production MG3 (2026-09-23).
	accel := `[{"mac":"c30000393fe5","rawData":"0201060303e1ff1216e1ffa103640007ff0bffbee53f390000c3"}]`
	temp1 := `[{"mac":"c30000393fe5","rawData":"0201060303E1FF1016E1FFA1016419F8394AE53F390000C3"}]`
	temp2 := `[{"mac":"c30000393fe5","rawData":"0201060303e1ff1016e1ffa1016419b8402ee53f390000c3"}]`
	envSamples := func() int {
		var n int
		if e := f.admin.QueryRowContext(ctx, `SELECT count(*) FROM core.sensor_samples WHERE gateway_id=$1 AND external_id='c30000393fe5' AND reading->'frames' ? 'minew-ffe1-a101@1'`, g.ID).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n
	}
	for _, p := range []string{accel, temp1} {
		if _, e = f.repo.CapturePacket(ctx, a.TenantID, g.ID, []byte(p)); e != nil {
			t.Fatal(e)
		}
	}
	if n := envSamples(); n != 1 {
		t.Fatalf("temperature after an accelerometer sample must be stored: %d", n)
	}
	var temp float64
	if e = f.admin.QueryRowContext(ctx, `SELECT (reading->>'temperature')::float FROM core.sensor_samples WHERE gateway_id=$1 AND external_id='c30000393fe5' AND reading->'frames' ? 'minew-ffe1-a101@1'`, g.ID).Scan(&temp); e != nil || temp < 25.9 || temp > 26.0 {
		t.Fatalf("decoded temperature %v (%v)", temp, e)
	}
	// A second temperature within the interval is still thinned.
	if _, e = f.repo.CapturePacket(ctx, a.TenantID, g.ID, []byte(temp2)); e != nil {
		t.Fatal(e)
	}
	if n := envSamples(); n != 1 {
		t.Fatalf("second temperature inside the interval should be thinned: %d", n)
	}
	// The accelerometer slot is state and is always stored.
	var motion int
	if e = f.admin.QueryRowContext(ctx, `SELECT count(*) FROM core.sensor_samples WHERE gateway_id=$1 AND external_id='c30000393fe5' AND reading->'frames' ? 'minew-ffe1-a103@1'`, g.ID).Scan(&motion); e != nil || motion != 1 {
		t.Fatalf("accelerometer sample: %d (%v)", motion, e)
	}
}
