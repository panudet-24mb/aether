package tests

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

// A tag export is streamed, and the server's WriteTimeout (15 s) is one absolute deadline for a response: the stream
// moves the connection's write deadline 30 s ahead as it makes progress. Against a real listener (app.Test has no
// deadlines): a slow client that also pauses 20 s gets the whole archive after well over 15 s; a client that stops
// reading for 45 s is cut off, and its export slot comes back.
func TestTagExportStreamsPastTheWriteTimeout(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	_, auth, p := f.account(t)
	gateway, device := uuid.NewString(), uuid.NewString()
	const tag = "c3000039cc03"
	const rows = 30000
	for _, q := range []string{
		`INSERT INTO core.gateways(id,tenant_id,name,model,token_hash) VALUES($2,$1,'Ward','minew-mg3','x')`,
		`INSERT INTO core.sensor_streams(tenant_id,gateway_id,external_id,name,last_seen) VALUES($1,$2,'` + tag + `','W',now())`,
		`INSERT INTO core.devices(id,tenant_id,gateway_id,name,external_id,profile_id,roaming) VALUES($3,$1,$2,'W','` + tag + `','minew-b10',true)`,
		// High-entropy rows, so compression cannot shrink the archive below what socket buffers absorb.
		`INSERT INTO core.sensor_samples(tenant_id,gateway_id,external_id,event_key,received_at,decoder_id,reading)
     SELECT $1,$2,'` + tag + `','k'||i,now()-make_interval(secs => i),'x',jsonb_build_object('a',md5(i::text),'b',md5((i*7)::text),'c',md5((i*13)::text))
     FROM generate_series(1,` + fmt.Sprint(rows) + `) i`,
		`INSERT INTO core.ble_history(tenant_id,gateway_id,external_id,event_key,received_at,raw,source)
     SELECT $1,$2,'` + tag + `','k'||i,now()-make_interval(secs => i),md5(i::text)||md5((i*3)::text)||md5((i*5)::text)||md5((i*11)::text),'device'
     FROM generate_series(1,` + fmt.Sprint(rows) + `) i`,
	} {
		args := []any{p.TenantID, gateway}
		if strings.Contains(q, "$3") {
			args = append(args, device)
		}
		if _, e := f.admin.ExecContext(ctx, q, args...); e != nil {
			t.Fatalf("%s: %v", q, e)
		}
	}
	t.Cleanup(func() {
		f.admin.Exec(`DELETE FROM core.sensor_samples WHERE tenant_id=$1`, p.TenantID)
		f.admin.Exec(`DELETE FROM core.ble_history WHERE tenant_id=$1`, p.TenantID)
	})

	api := busyAPI(f)
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	go api.Listener(ln, fiber.ListenConfig{DisableStartupMessage: true})
	t.Cleanup(func() { api.Shutdown() })
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, e := (&net.Dialer{}).DialContext(ctx, network, addr)
		if e == nil {
			conn.(*net.TCPConn).SetReadBuffer(16 * 1024) // a small window: the server feels a slow reader at once
		}
		return conn, e
	}, DisableKeepAlives: true}}
	export := func() *http.Response {
		t.Helper()
		var res *http.Response
		for attempt := 0; ; attempt++ {
			r, _ := http.NewRequest("POST", "http://"+ln.Addr().String()+"/api/v1/devices/"+device+"/privacy-export", strings.NewReader(`{}`))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Authorization", "Bearer "+auth.AccessToken)
			res, e = client.Do(r)
			if e == nil {
				break
			}
			if attempt == 20 {
				t.Fatal(e)
			}
			time.Sleep(50 * time.Millisecond) // the listener starts in its own goroutine
		}
		if res.StatusCode != 200 {
			t.Fatalf("export: %d", res.StatusCode)
		}
		return res
	}
	manifest := func(body []byte) (string, error) {
		archive, e := zip.NewReader(bytes.NewReader(body), int64(len(body)))
		if e != nil {
			return "", e
		}
		for _, file := range archive.File {
			if file.Name == "manifest.json" {
				r, _ := file.Open()
				b, _ := io.ReadAll(r)
				return string(b), nil
			}
		}
		return "", errors.New("no manifest.json")
	}

	// Slow, with a 20 s pause: complete, well past 15 s.
	started := time.Now()
	res := export()
	var body bytes.Buffer
	chunk := make([]byte, 4096)
	for time.Since(started) < 5*time.Second {
		n, e := res.Body.Read(chunk)
		body.Write(chunk[:n])
		if e != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(20 * time.Second)
	rest, e := io.ReadAll(res.Body)
	res.Body.Close()
	body.Write(rest)
	elapsed := time.Since(started)
	if e != nil {
		t.Fatalf("slow export cut off after %s (%d bytes): %v", elapsed, body.Len(), e)
	}
	m, e := manifest(body.Bytes())
	if e != nil || !strings.Contains(m, `"complete":true`) || elapsed < 20*time.Second {
		t.Fatalf("slow export: %s after %s, %d bytes: %v", m, elapsed, body.Len(), e)
	}
	t.Logf("slow export: %d bytes in %s, manifest %s", body.Len(), elapsed.Round(time.Millisecond), m)

	// Stalled for 45 s after the first 16 KB: the server gives up about 30 s after the client stopped taking data.
	res = export()
	head := make([]byte, 16*1024)
	io.ReadFull(res.Body, head)
	time.Sleep(45 * time.Second)
	rest, e = io.ReadAll(res.Body)
	res.Body.Close()
	whole := append(head, rest...)
	if _, me := manifest(whole); me == nil {
		t.Fatalf("a client stalled for 45 s still got the whole archive (%d bytes)", len(whole))
	}
	t.Logf("stalled export: cut off, %d bytes, read error %v", len(whole), e)

	// Both slots of the workspace are free again: two exports at once succeed.
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, _ := http.NewRequest("POST", "http://"+ln.Addr().String()+"/api/v1/devices/"+device+"/privacy-export", strings.NewReader(`{}`))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Authorization", "Bearer "+auth.AccessToken)
			res, e := http.DefaultClient.Do(r)
			if e != nil {
				codes <- 0
				return
			}
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			codes <- res.StatusCode
		}()
	}
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != 200 {
			t.Fatalf("after the cut-off stream, an export got %d (slot not released?)", code)
		}
	}
}

// Erasing a tag's history while ingest for the same tag runs: the erasure takes the in-scope gateway rows first, as
// ingest does, so the two serialise and neither dies in a deadlock.
func TestTagErasureUnderLiveIngest(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	_, _, owner := f.account(t)
	g, _, e := f.service.CreateGateway(ctx, owner, "Ward", "minew-mg3")
	if e != nil {
		t.Fatal(e)
	}
	d, e := f.service.CreateDevice(ctx, owner, g.ID, "Wearer", "c30000393fe5", "generic-environment@1")
	if e != nil {
		t.Fatal(e)
	}
	if _, e := f.admin.Exec(`UPDATE core.devices SET roaming=true WHERE id=$1`, d.ID); e != nil {
		t.Fatal(e)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	failures := []error{}
	captured := 0
	for w := 0; w < 3; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				// A new temperature each time, so every packet is stored (no redelivery dedupe).
				packet := fmt.Sprintf(`[{"mac":"c30000393fe5","rawData":"0201060303E1FF1016E1FFA10164%04X394AE53F390000C3"}]`, (w*10000+i)%0x7fff)
				if _, e := f.repo.CapturePacket(ctx, owner.TenantID, g.ID, []byte(packet)); e != nil {
					mu.Lock()
					failures = append(failures, fmt.Errorf("ingest: %w", e))
					mu.Unlock()
					continue
				}
				mu.Lock()
				captured++
				mu.Unlock()
			}
		}(w)
	}
	erasures := 0
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		// Every erasure then has the stream row to rewrite and a presence row to delete: the two rows ingest takes in
		// the opposite order (stream in saveSamples, presence in the zone decision).
		if _, e := f.admin.Exec(`UPDATE core.sensor_streams SET name='Wearer' WHERE gateway_id=$1`, g.ID); e != nil {
			t.Fatal(e)
		}
		if _, e := f.repo.EraseIdentityHistory(ctx, owner, d.ID, nil, false); e != nil {
			mu.Lock()
			failures = append(failures, fmt.Errorf("erase: %w", e))
			mu.Unlock()
		}
		erasures++
		time.Sleep(10 * time.Millisecond)
	}
	close(stop)
	wg.Wait()
	for _, failure := range failures {
		t.Errorf("%v (SQLSTATE %s)", failure, sqlState(errors.Unwrap(failure)))
	}
	if erasures < 10 || captured < 50 {
		t.Fatalf("too little concurrency to prove anything: %d erasures, %d packets", erasures, captured)
	}
	t.Logf("%d erasures during %d captured packets, %d failures", erasures, captured, len(failures))
}
