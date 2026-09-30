package tuyacloudlink

import (
	"aether/backend/internal/adapters/postgres"
	"aether/backend/internal/adapters/tuya"
	"aether/backend/internal/adapters/tuyacloud"
	"aether/backend/internal/domain"
	"context"
	"errors"
	"strings"
	"time"
)

// syncLoop serves the link's sync requests (a new link, a member's "sync", an unknown or re-bound device), at most
// once per SyncEvery, and never while the trial guard is on: a sync costs one call for the list and one per device.
// After a sync the registered devices' current values are read once, so a card shows state before the first report.
func (l *link) syncLoop() {
	var last time.Time
	for {
		if !last.IsZero() && l.w.now().Sub(last) < l.t.SyncEvery {
			// Too soon: look again once the interval is over (or when poked).
			if !l.sleep(l.t.SyncEvery - l.w.now().Sub(last)) {
				return
			}
		}
		requested, registered, e := l.w.Store.CloudSyncWanted(l.ctx, l.tenant, l.gateway)
		if e == nil && requested != nil && !l.usage.guarded() {
			last = l.w.now()
			if e := l.sync(registered); e != nil {
				if l.ctx.Err() != nil {
					return
				}
				l.warn("tuya cloud: device sync failed", "error", e.Error())
			} else if e := l.w.Store.CompleteCloudSync(l.ctx, l.tenant, l.gateway, *requested); e != nil && l.ctx.Err() == nil {
				l.warn("tuya cloud: sync not recorded", "error", e.Error())
			}
		}
		select {
		case <-l.ctx.Done():
			return
		case <-l.pokes:
		}
	}
}

func (l *link) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-l.ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// sync lists the project's devices and each device's data-point model, and stores them (no key: cloud mode needs
// none). A device whose model cannot be read is stored without one only if it was never stored.
func (l *link) sync(registered []string) error {
	l.mu.Lock()
	sess := l.sess
	l.mu.Unlock()
	var devices []tuyacloud.Device
	if e := l.api(sess, func(c *tuyacloud.Client) error {
		var e error
		devices, e = c.Devices(l.ctx)
		return e
	}); e != nil {
		return e
	}
	imports := []postgres.CloudImport{}
	for _, d := range devices {
		id := strings.ToLower(strings.TrimSpace(d.ID))
		var raw []byte
		var format string
		if e := l.api(sess, func(c *tuyacloud.Client) error {
			var e error
			raw, format, e = c.Model(l.ctx, d.ID)
			return e
		}); e != nil {
			if errors.Is(e, tuyacloud.ErrRateLimited) || errors.Is(e, tuyacloud.ErrAuth) || l.ctx.Err() != nil {
				return e
			}
			continue
		}
		var spec []tuya.DP
		category := d.Category
		var e error
		if format == "model" {
			spec, e = tuya.ParseModel(raw)
		} else {
			var cat string
			spec, cat, e = tuya.ParseSpecifications(raw)
			if category == "" {
				category = cat
			}
		}
		if e != nil {
			continue
		}
		imports = append(imports, postgres.CloudImport{TuyaID: id, Name: d.Name, Category: category, ProductID: d.ProductID,
			ParentID: strings.ToLower(d.GatewayID), NodeID: d.NodeID, Sub: d.Sub, Spec: spec})
	}
	if _, e := l.w.Store.SaveCloudDevices(l.ctx, l.tenant, l.gateway, imports); e != nil {
		return e
	}
	l.log("tuya cloud: devices synced", "listed", len(devices), "stored", len(imports))
	return l.snapshot(sess, registered)
}

// snapshot reads the current properties of each registered device once and stores them like a status report
// (protocol 0, one report time per property as Tuya gives it, so a newer message is never overwritten).
func (l *link) snapshot(sess *session, registered []string) error {
	for i, id := range registered {
		if i == tuyacloud.MaxDevices || l.usage.guarded() {
			break
		}
		var props []tuyacloud.Property
		if e := l.api(sess, func(c *tuyacloud.Client) error {
			var e error
			props, e = c.Properties(l.ctx, id)
			return e
		}); e != nil {
			if errors.Is(e, tuyacloud.ErrRateLimited) || errors.Is(e, tuyacloud.ErrAuth) || l.ctx.Err() != nil {
				return e
			}
			continue
		}
		ev := tuyacloud.Event{Kind: tuyacloud.EventStatus, DevID: id}
		for _, p := range props {
			if len(p.Value) == 0 {
				continue
			}
			ev.Items = append(ev.Items, tuyacloud.Item{Code: p.Code, DPID: p.DPID, Value: p.Value, T: p.Time})
		}
		if len(ev.Items) == 0 {
			continue
		}
		if _, e := l.w.Store.CaptureTuyaCloud(l.ctx, l.tenant, l.gateway, 0, []tuyacloud.Event{ev}, false); e != nil && !errors.Is(e, domain.ErrInvalid) {
			return e
		}
	}
	return nil
}

// api makes one OpenAPI call and turns a project-wide refusal into the link's state: credentials refused, API
// service not subscribed, quota reached. A call for one device failing is not the link's fault.
func (l *link) api(sess *session, fn func(*tuyacloud.Client) error) error {
	if sess == nil {
		return tuyacloud.ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(l.ctx, 2*tuyacloud.CallTimeout)
	defer cancel()
	e := sess.call(ctx, fn)
	var api *tuyacloud.APIError
	errors.As(e, &api)
	code := int64(0)
	if api != nil {
		code = int64(api.Code)
	}
	switch {
	case errors.Is(e, tuyacloud.ErrAuth):
		l.setState(domain.CloudLinkAuthFailed, "api_auth", code)
	case errors.Is(e, tuyacloud.ErrNotSubscribed):
		l.setState(domain.CloudLinkNotSubscribed, "api_not_subscribed", code)
	case errors.Is(e, tuyacloud.ErrRateLimited):
		l.setState(domain.CloudLinkQuota, "api_quota", code)
	}
	return e
}
