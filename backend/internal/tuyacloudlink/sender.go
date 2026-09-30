package tuyacloudlink

import (
	"aether/backend/internal/adapters/tuyacloud"
	"aether/backend/internal/commander"
	"aether/backend/internal/domain"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"
)

// Failure reasons stored on a cloud command that could not be delivered.
const (
	ReasonLinkUnavailable = "link_unavailable"
	ReasonQuotaNear       = "quota_near"
	ReasonOffline         = "device_offline"
	ReasonQuota           = "quota"
	ReasonAuth            = "auth_failed"
	ReasonUnreachable     = "cloud_unreachable"
	ReasonRejected        = "rejected"
	ReasonInvalid         = "invalid command payload"
)

// tuyaDeviceOffline is Tuya's error code for a command to a device that is not connected to the cloud.
const tuyaDeviceOffline = 2001

// Dispatcher is the command dispatcher of the tuya-cloud worker: it claims only cloud commands, at CommandRate per
// link, and delivers each through the link's OpenAPI session. Timeouts and automation requests are left to
// mqtt-commander, which runs them for every transport.
func (w *Worker) Dispatcher() *commander.Dispatcher {
	return &commander.Dispatcher{Store: cloudStore{w.Store, w}, Now: time.Now, Transports: []string{"cloud"}, Rate: CommandRate, Burst: CommandBurst,
		NoHousekeeping: true, Deliver: w.deliver}
}

// cloudStore sweeps only the tenants with a running link.
type cloudStore struct {
	Store
	w *Worker
}

func (s cloudStore) ActiveTenants(context.Context) ([]string, error) { return s.w.tenants(), nil }

// deliver issues one claimed cloud command: {"<code>":<raw>} to POST .../shadow/properties/issue, bounded by
// tuyacloud.CommandTimeout. The state report that confirms it arrives through the Message Service.
func (w *Worker) deliver(ctx context.Context, tenant string, c domain.Command) (err error) {
	// A panic while delivering one command fails that command, not the dispatcher (the value is not logged).
	defer func() {
		if recover() != nil {
			slog.Error("tuya cloud: command delivery panicked", "tenant", tenant, "gateway", c.GatewayID, "command", c.ID)
			err = errors.New(ReasonRejected)
		}
	}()
	l := w.linkFor(tenant, c.GatewayID)
	if l == nil || l.ctx.Err() != nil {
		return errors.New(ReasonLinkUnavailable)
	}
	l.mu.Lock()
	sess := l.sess
	l.mu.Unlock()
	if sess == nil {
		return errors.New(ReasonLinkUnavailable)
	}
	if l.usage.guarded() {
		return errors.New(ReasonQuotaNear)
	}
	var wire map[string]json.RawMessage
	if json.Unmarshal(c.Wire, &wire) != nil || len(wire) == 0 {
		return errors.New(ReasonInvalid)
	}
	properties := make(map[string]any, len(wire))
	for code, raw := range wire {
		properties[code] = raw
	}
	ctx, cancel := context.WithTimeout(ctx, tuyacloud.CommandTimeout)
	defer cancel()
	e := l.api(sess, func(client *tuyacloud.Client) error { return client.IssueProperties(ctx, c.IEEE, properties) })
	if e == nil {
		return nil
	}
	var api *tuyacloud.APIError
	switch {
	case errors.As(e, &api) && api.Code == tuyaDeviceOffline:
		return errors.New(ReasonOffline)
	case errors.Is(e, tuyacloud.ErrRateLimited):
		return errors.New(ReasonQuota)
	case errors.Is(e, tuyacloud.ErrAuth), errors.Is(e, tuyacloud.ErrNotSubscribed), errors.Is(e, tuyacloud.ErrPermission):
		return errors.New(ReasonAuth)
	case errors.Is(e, tuyacloud.ErrUnavailable), errors.Is(e, context.DeadlineExceeded):
		return errors.New(ReasonUnreachable)
	}
	return errors.New(ReasonRejected)
}
