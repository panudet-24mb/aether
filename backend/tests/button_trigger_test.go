package tests

import (
	"aether/backend/internal/adapters/postgres"
	"context"
	"testing"
)

// Real frames from the physical B10 (c300007b573c) captured on the production MG3 on 2026-09-23: at rest it
// sends accelerometer/info/TLM only; a press starts its iBeacon slot. A registered B10 must raise exactly
// one SOS per burst, with no teaching.
func TestB10PressFromTriggerSlot(t *testing.T) {
	f := setup(t)
	_, _, a := f.account(t)
	ctx := context.Background()
	g, _, e := f.service.CreateGateway(ctx, a, "SOS", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	idle := []string{
		`[{"mac":"c300007b573c","rawData":"0201060303E1FF1216E1FFA103140059FF7EFF393C577B0000C3"}]`,
		`[{"mac":"c300007b573c","rawData":"0201060303E1FF0F16E1FFA108143C577B0000C3423130"}]`,
		`[{"mac":"c300007b573c","rawData":"0201060303AAFE1116AAFE20000EB21400000C0BC806CF3720"}]`,
	}
	press := `[{"mac":"c300007b573c","rawData":"0201061AFF4C000215FDA50693A4E24FB1AFCFC6EB0764782500000000C5"},{"mac":"c300007b573c","rawData":"0201060303E1FF1216E1FFA103140051FF80FF393C577B0000C3"}]`
	press2 := `[{"mac":"c300007b573c","rawData":"0201061AFF4C000215FDA50693A4E24FB1AFCFC6EB0764782500000000C5"},{"mac":"c300007b573c","rawData":"0201060303E1FF1216E1FFA103140054FF76FF3E3C577B0000C3"}]`
	capture := func(p string) {
		if _, e := f.repo.CapturePacket(ctx, a.TenantID, g.ID, []byte(p)); e != nil {
			t.Fatal(e)
		}
	}
	count := func(q string) int {
		var n int
		if e := f.admin.QueryRowContext(ctx, q, g.ID).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n
	}
	buttons := func() int {
		return count(`SELECT count(*) FROM core.device_events WHERE gateway_id=$1 AND external_id='c300007b573c' AND event_type='button'`)
	}
	capture(idle[0]) // creates the stream so the device can be registered
	if _, e = f.service.CreateDevice(ctx, a, g.ID, "SOS B10", "C300007B573C", "minew-b10-pending@1"); e != nil {
		t.Fatal(e)
	}
	for _, p := range idle {
		capture(p)
	}
	if n := buttons(); n != 0 {
		t.Fatalf("idle B10 raised %d presses", n)
	}
	capture(press)
	capture(press2) // same burst
	if n := buttons(); n != 1 {
		t.Fatalf("one press burst must raise exactly one button event: %d", n)
	}
	if n := count(`SELECT count(*) FROM core.alerts WHERE gateway_id=$1 AND external_id='c300007b573c' AND event_type='button' AND severity='critical'`); n != 1 {
		t.Fatalf("SOS alert not opened: %d", n)
	}
	// A burst that ended long ago: the next iBeacon is a new press.
	if _, e = f.admin.ExecContext(ctx, `UPDATE core.stream_state SET trigger_at=now()-interval '5 minutes' WHERE gateway_id=$1 AND external_id='c300007b573c'`, g.ID); e != nil {
		t.Fatal(e)
	}
	capture(`[{"mac":"c300007b573c","rawData":"0201061AFF4C000215FDA50693A4E24FB1AFCFC6EB0764782500000000C5"},{"mac":"c300007b573c","rawData":"0201060303E1FF0F16E1FFA108143C577B0000C3423130"}]`)
	if n := buttons(); n != 2 {
		t.Fatalf("press after the burst ended: %d", n)
	}
	sosAlerts := func() int {
		return count(`SELECT count(*) FROM core.alerts WHERE gateway_id=$1 AND external_id='c300007b573c' AND event_type='button'`)
	}
	// The first SOS is still open (unacknowledged): the second press does not open another one.
	if n := sosAlerts(); n != 1 {
		t.Fatalf("second press while the first SOS is unacknowledged: %d alerts", n)
	}
	// Once somebody acknowledged it, the next press rings again.
	if _, e = f.admin.ExecContext(ctx, `UPDATE core.alerts SET status='acknowledged',opened_at=now()-interval '5 minutes' WHERE gateway_id=$1`, g.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = f.admin.ExecContext(ctx, `UPDATE core.stream_state SET trigger_at=now()-interval '5 minutes' WHERE gateway_id=$1 AND external_id='c300007b573c'`, g.ID); e != nil {
		t.Fatal(e)
	}
	capture(press)
	if n := sosAlerts(); n != 2 {
		t.Fatalf("press after acknowledgement must open a new SOS: %d alerts", n)
	}
}

// Shadow mode silences everything except SOS: a press still opens its critical alert.
func TestSOSBypassesShadowMode(t *testing.T) {
	f := setup(t)
	f.repo.Configure(postgres.Options{AlertsShadow: true, DiscoveryLimit: 100})
	_, _, a := f.account(t)
	ctx := context.Background()
	g, _, e := f.service.CreateGateway(ctx, a, "Shadow SOS", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.repo.CapturePacket(ctx, a.TenantID, g.ID, []byte(`[{"mac":"c300007b573c","rawData":"0201060303E1FF0F16E1FFA108143C577B0000C3423130"}]`)); e != nil {
		t.Fatal(e)
	}
	if _, e = f.service.CreateDevice(ctx, a, g.ID, "SOS B10", "C300007B573C", "minew-b10-pending@1"); e != nil {
		t.Fatal(e)
	}
	if _, e = f.repo.CapturePacket(ctx, a.TenantID, g.ID, []byte(`[{"mac":"c300007b573c","rawData":"0201061AFF4C000215FDA50693A4E24FB1AFCFC6EB0764782500000000C5"}]`)); e != nil {
		t.Fatal(e)
	}
	var sos, other int
	if e = f.admin.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE event_type='button'), count(*) FILTER (WHERE event_type<>'button') FROM core.alerts WHERE gateway_id=$1`, g.ID).Scan(&sos, &other); e != nil {
		t.Fatal(e)
	}
	if sos != 1 || other != 0 {
		t.Fatalf("shadow mode: sos alerts %d, other alerts %d", sos, other)
	}
}
