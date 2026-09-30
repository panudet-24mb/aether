// Package bletest is a fake Bluetooth radio for tests of the Tuya BLE transport: it "hears" tuyablesim devices
// and connects to them, with knobs for failures the host adapter would cause. The agent never imports it.
package bletest

import (
	"context"
	"errors"
	"sync"
	"time"

	"aether/backend/internal/edge/ble"
	"aether/backend/internal/tuyable"
	"aether/backend/internal/tuyable/tuyablesim"
)

// Radio advertises every added device on each Every tick while Scan runs.
type Radio struct {
	Every time.Duration

	mu       sync.Mutex
	devices  map[string]*entry
	scanErr  error
	open     int
	maxOpen  int
	connects map[string]int
}

type entry struct {
	dev        *tuyablesim.Device
	fd50       bool
	rssi       int
	silent     bool  // not advertising
	connectErr error // what Connect returns instead of a link
}

// New makes an empty radio.
func New() *Radio {
	return &Radio{Every: 10 * time.Millisecond, devices: map[string]*entry{}, connects: map[string]int{}}
}

// Add puts a simulated device in range at mac.
func (r *Radio) Add(mac string, d *tuyablesim.Device, rssi int) {
	r.mu.Lock()
	r.devices[mac] = &entry{dev: d, rssi: rssi}
	r.mu.Unlock()
}

// Silence stops (or resumes) a device's advertisements.
func (r *Radio) Silence(mac string, on bool) {
	r.mu.Lock()
	if e := r.devices[mac]; e != nil {
		e.silent = on
	}
	r.mu.Unlock()
}

// RefuseConnect makes Connect to mac fail with err (nil restores it).
func (r *Radio) RefuseConnect(mac string, err error) {
	r.mu.Lock()
	if e := r.devices[mac]; e != nil {
		e.connectErr = err
	}
	r.mu.Unlock()
}

// FailScan makes Scan fail with err at once (nil restores it).
func (r *Radio) FailScan(err error) {
	r.mu.Lock()
	r.scanErr = err
	r.mu.Unlock()
}

// MaxOpen is the most links that were open at the same time; Connects counts attempts per address.
func (r *Radio) MaxOpen() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.maxOpen
}

func (r *Radio) Connects(mac string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.connects[mac]
}

func (r *Radio) Adapter() string { return "hci-test" }

func (r *Radio) Scan(ctx context.Context, on func(ble.Advertisement)) error {
	r.mu.Lock()
	e := r.scanErr
	r.mu.Unlock()
	if e != nil {
		return e
	}
	t := time.NewTicker(r.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		r.mu.Lock()
		var ads []ble.Advertisement
		for mac, x := range r.devices {
			if x.silent {
				continue
			}
			sd, md := x.dev.Advert()
			ads = append(ads, ble.Advertisement{MAC: mac, RSSI: x.rssi, ServiceData: sd, FD50: x.fd50, ManufacturerData: md})
		}
		r.mu.Unlock()
		for _, a := range ads {
			on(a)
		}
	}
}

func (r *Radio) Connect(ctx context.Context, mac string, _ bool) (tuyable.Link, error) {
	r.mu.Lock()
	r.connects[mac]++
	x := r.devices[mac]
	r.mu.Unlock()
	if x == nil {
		return nil, errors.New("bletest: no device at that address")
	}
	if x.connectErr != nil {
		return nil, x.connectErr
	}
	link, e := x.dev.Connect()
	if e != nil {
		return nil, e
	}
	r.mu.Lock()
	r.open++
	if r.open > r.maxOpen {
		r.maxOpen = r.open
	}
	r.mu.Unlock()
	return &counted{Link: link, r: r}, nil
}

// counted keeps the count of open links.
type counted struct {
	tuyable.Link
	r    *Radio
	once sync.Once
}

func (c *counted) Close() error {
	c.once.Do(func() {
		c.r.mu.Lock()
		c.r.open--
		c.r.mu.Unlock()
	})
	return c.Link.Close()
}
