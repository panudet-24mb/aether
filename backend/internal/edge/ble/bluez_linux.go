//go:build linux

package ble

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"tinygo.org/x/bluetooth"

	"aether/backend/internal/tuyable"
)

// BlueZ is the host's Bluetooth through bluetoothd over the system D-Bus (tinygo.org/x/bluetooth). The container
// needs no capability for it: only the host's /run/dbus mounted read-only, and a D-Bus policy that lets the agent's
// user (uid 10001) talk to org.bluez (docs/platform/aether-edge-install.md). It never touches raw HCI.
type BlueZ struct {
	id string

	mu      sync.Mutex
	adapter *bluetooth.Adapter
}

// NewSystemRadio is the radio of the host's adapter id (hci0).
func NewSystemRadio(id string) Radio {
	if id == "" {
		id = "hci0"
	}
	return &BlueZ{id: id}
}

var (
	uuidA201 = bluetooth.New16BitUUID(tuyable.ServiceA201)
	uuidFD50 = bluetooth.New16BitUUID(tuyable.ServiceFD50)
	// The legacy GATT service is 0x1910 (it advertises 0xA201).
	uuidService1910 = bluetooth.New16BitUUID(0x1910)
)

func (b *BlueZ) Adapter() string { return b.id }

// enable connects to bluetoothd once; a failed attempt is retried on the next call.
func (b *BlueZ) enable() (*bluetooth.Adapter, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.adapter != nil {
		return b.adapter, nil
	}
	a := bluetooth.NewAdapter(b.id)
	if e := a.Enable(); e != nil {
		return nil, classify(e)
	}
	b.adapter = a
	return a, nil
}

// classify maps BlueZ and D-Bus failures to the states the heartbeat reports.
func classify(e error) error {
	var de dbus.Error
	msg := strings.ToLower(e.Error())
	switch {
	case errors.As(e, &de) && de.Name == "org.freedesktop.DBus.Error.AccessDenied",
		strings.Contains(msg, "accessdenied"), strings.Contains(msg, "system_bus_socket"), strings.Contains(msg, "permission denied"):
		return fmt.Errorf("%w: %v", ErrNoPermission, e)
	case strings.Contains(msg, "does not exist"), strings.Contains(msg, "not powered"), strings.Contains(msg, "rfkill"),
		strings.Contains(msg, "org.bluez.error.notready"):
		return fmt.Errorf("%w: %v", ErrNoAdapter, e)
	}
	return e
}

// Scan passes on every advertisement that carries Tuya service or manufacturer data. BlueZ reports a device again
// whenever one of its properties changes (RSSI, data), which is how repeated advertisements arrive over D-Bus.
func (b *BlueZ) Scan(ctx context.Context, on func(Advertisement)) error {
	a, e := b.enable()
	if e != nil {
		return e
	}
	done := make(chan error, 1)
	go func() {
		done <- a.Scan(func(_ *bluetooth.Adapter, r bluetooth.ScanResult) {
			adv := Advertisement{MAC: r.Address.MAC.String(), RSSI: int(r.RSSI)}
			for _, s := range r.AdvertisementPayload.ServiceData() {
				switch s.UUID {
				case uuidA201:
					adv.ServiceData = s.Data
				case uuidFD50:
					adv.ServiceData, adv.FD50 = s.Data, true
				}
			}
			for _, m := range r.AdvertisementPayload.ManufacturerData() {
				if m.CompanyID == tuyable.ManufacturerID {
					adv.ManufacturerData = m.Data
				}
			}
			if adv.ServiceData != nil || adv.ManufacturerData != nil {
				on(adv)
			}
		})
	}()
	select {
	case <-ctx.Done():
		// StopScan can fail (the scan already ended on its own); never let shutdown wait on it for long.
		_ = a.StopScan()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		return nil
	case e := <-done:
		if e == nil {
			return errors.New("ble: scan ended")
		}
		return classify(e)
	}
}

// connectCap bounds how long a connect BlueZ was asked to abandon may still hold its slot.
const connectCap = 15 * time.Second

// Connect opens the device's Tuya GATT service within ctx. BlueZ's own connect call has no deadline of ours: when
// ctx ends first, BlueZ is told to abandon the attempt (Device1.Disconnect cancels a connection in progress), and
// Connect returns only once BlueZ answered or connectCap passed, so the manager keeps the connection slot until the
// adapter is really free.
func (b *BlueZ) Connect(ctx context.Context, mac string, fd50 bool) (tuyable.Link, error) {
	a, e := b.enable()
	if e != nil {
		return nil, e
	}
	m, e := bluetooth.ParseMAC(strings.ToUpper(mac))
	if e != nil {
		return nil, fmt.Errorf("ble: bad address %q", mac)
	}
	type result struct {
		dev bluetooth.Device
		err error
	}
	got := make(chan result, 1)
	go func() {
		d, e := a.Connect(bluetooth.Address{MACAddress: bluetooth.MACAddress{MAC: m}}, bluetooth.ConnectionParams{})
		got <- result{d, e}
	}()
	var dev bluetooth.Device
	select {
	case r := <-got:
		if r.err != nil {
			return nil, r.err
		}
		dev = r.dev
	case <-ctx.Done():
		b.abandon(m)
		select {
		case r := <-got:
			if r.err == nil {
				_ = r.dev.Disconnect()
			}
		case <-time.After(connectCap):
			go func() {
				if r := <-got; r.err == nil {
					_ = r.dev.Disconnect()
				}
			}()
		}
		return nil, ctx.Err()
	}
	link, e := openLink(ctx, dev, fd50)
	if e != nil {
		_ = dev.Disconnect()
		return nil, e
	}
	return link, nil
}

// abandon asks BlueZ to stop connecting to a device (Device1.Disconnect on its object also cancels a connection
// in progress).
func (b *BlueZ) abandon(m bluetooth.MAC) {
	bus, e := dbus.SystemBus()
	if e != nil {
		return
	}
	path := dbus.ObjectPath("/org/bluez/" + b.id + "/dev_" + strings.ReplaceAll(m.String(), ":", "_"))
	call := bus.Object("org.bluez", path).Go("org.bluez.Device1.Disconnect", 0, nil)
	select {
	case <-call.Done:
	case <-time.After(5 * time.Second):
	}
}

func openLink(ctx context.Context, dev bluetooth.Device, fd50 bool) (*bluezLink, error) {
	service, notifyUUID, writeUUID := uuidService1910, tuyable.CharNotify, tuyable.CharWrite
	if fd50 {
		service, notifyUUID, writeUUID = uuidFD50, tuyable.CharNotifyFD50, tuyable.CharWriteFD50
	}
	nu, _ := bluetooth.ParseUUID(notifyUUID)
	wu, _ := bluetooth.ParseUUID(writeUUID)
	type found struct {
		notify, write bluetooth.DeviceCharacteristic
		err           error
	}
	got := make(chan found, 1)
	go func() {
		services, e := dev.DiscoverServices([]bluetooth.UUID{service})
		if e != nil || len(services) != 1 {
			got <- found{err: fmt.Errorf("ble: Tuya service not found: %v", e)}
			return
		}
		chars, e := services[0].DiscoverCharacteristics([]bluetooth.UUID{nu, wu})
		var f found
		n := 0
		for _, c := range chars {
			switch c.UUID() {
			case nu:
				f.notify, n = c, n+1
			case wu:
				f.write, n = c, n+1
			}
		}
		if e != nil || n != 2 {
			got <- found{err: fmt.Errorf("ble: Tuya characteristics not found: %v", e)}
			return
		}
		got <- f
	}()
	var f found
	select {
	case f = <-got:
		if f.err != nil {
			return nil, f.err
		}
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	l := &bluezLink{dev: dev, write: f.write, notify: f.notify, ch: make(chan []byte, 64), quit: make(chan struct{})}
	if e := l.notify.EnableNotifications(l.onNotify); e != nil {
		return nil, fmt.Errorf("ble: notifications refused: %w", e)
	}
	go l.watch()
	return l, nil
}

// bluezLink is one GATT connection. Notifications are delivered in order on a buffered channel; if the session
// cannot keep up the link is closed rather than a notification dropped (tuyable.Link's contract).
type bluezLink struct {
	dev           bluetooth.Device
	write, notify bluetooth.DeviceCharacteristic

	mu     sync.Mutex
	ch     chan []byte
	closed bool
	reason error
	quit   chan struct{}
	once   sync.Once
}

func (l *bluezLink) onNotify(buf []byte) {
	b := append([]byte(nil), buf...)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	select {
	case l.ch <- b:
	default:
		l.shut(errors.New("ble: notifications overflowed"))
	}
}

// shut closes the notification channel; called with l.mu held.
func (l *bluezLink) shut(reason error) {
	if l.closed {
		return
	}
	l.closed, l.reason = true, reason
	close(l.ch)
}

// watch notices the device going away: BlueZ has no per-connection callback here, so the Connected property is
// polled.
func (l *bluezLink) watch() {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-l.quit:
			return
		case <-t.C:
			if ok, e := l.dev.Connected(); e == nil && !ok {
				l.mu.Lock()
				l.shut(errors.New("ble: device disconnected"))
				l.mu.Unlock()
				return
			}
		}
	}
}

func (l *bluezLink) Write(ctx context.Context, packet []byte) error {
	done := make(chan error, 1)
	go func() {
		_, e := l.write.WriteWithoutResponse(packet)
		done <- e
	}()
	select {
	case e := <-done:
		return e
	case <-ctx.Done():
		return ctx.Err()
	case <-l.quit:
		return errors.New("ble: link closed")
	}
}

func (l *bluezLink) Notifications() <-chan []byte { return l.ch }

// Reason says why the notifications ended, when the device (not the host) ended them.
func (l *bluezLink) Reason() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reason
}

func (l *bluezLink) Close() error {
	l.once.Do(func() {
		close(l.quit)
		_ = l.notify.EnableNotifications(nil)
		l.mu.Lock()
		l.shut(nil)
		l.mu.Unlock()
		_ = l.dev.Disconnect()
	})
	return nil
}
