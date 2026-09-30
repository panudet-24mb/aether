package postgres

import (
	"aether/backend/internal/adapters/edge"
	"aether/backend/internal/adapters/tuya"
	"aether/backend/internal/adapters/tuyacloud"
	"aether/backend/internal/adapters/zigbee2mqtt"
	"aether/backend/internal/domain"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// TuyaCloudChannel is the pg_notify channel that wakes the tuya-cloud worker when a link is saved, rotated,
// unlinked, revoked or asked to sync; the payload is the tenant id (never anything secret).
const TuyaCloudChannel = "aether_tuya_cloud"

// CloudSilentAfter is how long a Tuya Cloud gateway may record nothing (no event, no worker health) before its
// devices count as offline: the worker writes health every minute, so five minutes means the worker is gone.
const CloudSilentAfter = 5 * time.Minute

// CloudLinkGrace is how long a link the worker reports down (disconnected, credentials refused, quota, ...) may
// stay down before its devices are taken offline with reason cloud_link_down: a reconnect usually takes seconds.
const CloudLinkGrace = 2 * time.Minute

// CloudSyncDebounce is the least time between two device-list syncs asked for by events (an unknown or newly bound
// device): the sync costs API calls, which a trial project has few of.
const CloudSyncDebounce = 10 * time.Minute

var accessIDDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// cloudGatewayModel locks a gateway and checks it is a live Tuya Cloud gateway.
func cloudGatewayModel(tx *gorm.DB, gateway string, missing error) error {
	var models []string
	if e := tx.Raw(`SELECT model FROM core.gateways WHERE id=? AND revoked_at IS NULL FOR UPDATE`, gateway).Scan(&models).Error; e != nil {
		return e
	}
	if len(models) != 1 {
		return missing
	}
	if models[0] != domain.TuyaCloudGatewayModel {
		return domain.Because(domain.ErrInvalid, "not_a_cloud_gateway")
	}
	return nil
}

func notifyCloud(tx *gorm.DB, tenant string) error {
	return tx.Exec(`SELECT pg_notify(?,?)`, TuyaCloudChannel, tenant).Error
}

// DefaultCloudLinksPerTenant is how many Tuya Cloud projects one workspace may link unless configured otherwise.
const DefaultCloudLinksPerTenant = 2

// SaveTuyaCloudLink links a Tuya Cloud gateway to a cloud project, or rotates its credentials. The caller has
// sealed them for this tenant and gateway (security.SealTuyaCloud) and passes only a hint and a digest of the
// Access ID. A project already linked to any gateway, in any workspace, is refused as "already_linked" without
// saying where. A workspace links at most Options.CloudLinksPerTenant projects. The worker picks the new
// revision up from the notification.
//
// Contract: the caller (app.Service.LinkTuyaCloud) has proven the credentials with Tuya (GET /v1.0/token) before
// calling this. Otherwise anyone who knows a project's Access ID could claim its digest and lock the owner out.
func (r *Repository) SaveTuyaCloudLink(ctx context.Context, p domain.Principal, req domain.TuyaCloudLinkRequest) error {
	if !p.CanManageDevices() {
		return domain.ErrForbidden
	}
	if _, ok := tuyacloud.Regions[req.Region]; !ok || (req.Channel != tuyacloud.ChannelProd && req.Channel != tuyacloud.ChannelTest) ||
		len(req.AccessIDHint) > 8 || !accessIDDigestPattern.MatchString(req.AccessIDDigest) || req.CredentialsSealed == "" || len(req.CredentialsSealed) > 1024 {
		return domain.ErrInvalid
	}
	limit := r.opts.CloudLinksPerTenant
	if limit <= 0 {
		limit = DefaultCloudLinksPerTenant
	}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if e := cloudGatewayModel(tx, req.GatewayID, domain.ErrNotFound); e != nil {
			return e
		}
		// Counted workspace-wide under a per-tenant lock, so two concurrent links cannot both take the last slot.
		if e := tx.Exec(`SELECT pg_advisory_xact_lock(hashtextextended(?,34))`, p.TenantID).Error; e != nil {
			return e
		}
		var linked int64
		if e := tx.Raw(`SELECT core.tuya_cloud_link_count(?)`, req.GatewayID).Scan(&linked).Error; e != nil {
			return e
		}
		if linked >= int64(limit) {
			return domain.Because(domain.ErrConflict, "cloud_link_limit")
		}
		now := time.Now().UTC()
		// A new link (or new credentials) starts with a device-list sync.
		if e := tx.Exec(`INSERT INTO core.tuya_cloud_links(tenant_id,gateway_id,region,channel,access_id_hint,access_id_digest,credentials_sealed,state,state_at,reason,
      last_error_code,revision,linked_by,linked_at,updated_at,sync_requested_at)
    VALUES(?,?,?,?,?,?,?,'linking',?,'',0,1,?,?,?,?)
    ON CONFLICT(tenant_id,gateway_id) DO UPDATE SET region=EXCLUDED.region,channel=EXCLUDED.channel,access_id_hint=EXCLUDED.access_id_hint,
      sync_requested_at=EXCLUDED.sync_requested_at,
      access_id_digest=EXCLUDED.access_id_digest,credentials_sealed=EXCLUDED.credentials_sealed,state='linking',state_at=EXCLUDED.state_at,reason='',
      last_error_code=0,revision=core.tuya_cloud_links.revision+1,linked_by=EXCLUDED.linked_by,
      linked_at=CASE WHEN core.tuya_cloud_links.credentials_sealed IS NULL THEN EXCLUDED.linked_at ELSE core.tuya_cloud_links.linked_at END,
      rotated_at=CASE WHEN core.tuya_cloud_links.credentials_sealed IS NULL THEN core.tuya_cloud_links.rotated_at ELSE EXCLUDED.linked_at END,
      updated_at=EXCLUDED.updated_at`,
			p.TenantID, req.GatewayID, req.Region, req.Channel, clipText(req.AccessIDHint, 8), req.AccessIDDigest, req.CredentialsSealed, now, p.UserID, now, now, now).Error; e != nil {
			if errors.Is(e, gorm.ErrDuplicatedKey) {
				return domain.Because(domain.ErrConflict, "already_linked")
			}
			return e
		}
		if e := notifyCloud(tx, p.TenantID); e != nil {
			return e
		}
		if e := signal(tx, p.TenantID, "inventory", req.GatewayID); e != nil {
			return e
		}
		return audit(tx, p, "tuya_cloud.linked", req.GatewayID)
	})
	return e
}

// UnlinkTuyaCloud forgets a gateway's credentials for good (the sealed value and the Access ID digest are wiped,
// so the project can be linked again elsewhere) and marks the link disabled; the worker disconnects. Its devices
// go offline after CloudLinkGrace. Devices and registrations are kept.
func (r *Repository) UnlinkTuyaCloud(ctx context.Context, p domain.Principal, gateway string) error {
	if !p.CanManageDevices() {
		return domain.ErrForbidden
	}
	return r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if e := cloudGatewayModel(tx, gateway, domain.ErrNotFound); e != nil {
			return e
		}
		res := tx.Exec(`UPDATE core.tuya_cloud_links SET credentials_sealed=NULL,access_id_digest=NULL,access_id_hint='',state='disabled',state_at=now(),
      reason='unlinked',revision=revision+1,updated_at=now() WHERE gateway_id=? AND credentials_sealed IS NOT NULL`, gateway)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return domain.ErrNotFound
		}
		if e := notifyCloud(tx, p.TenantID); e != nil {
			return e
		}
		if e := signal(tx, p.TenantID, "inventory", gateway); e != nil {
			return e
		}
		return audit(tx, p, "tuya_cloud.unlinked", gateway)
	})
}

// wipeCloudLink is the part of revoking a gateway that concerns Tuya Cloud: the credentials go with it.
func wipeCloudLink(tx *gorm.DB, tenant, gateway string) error {
	res := tx.Exec(`UPDATE core.tuya_cloud_links SET credentials_sealed=NULL,access_id_digest=NULL,access_id_hint='',state='disabled',state_at=now(),
      reason='revoked',revision=revision+1,updated_at=now() WHERE gateway_id=? AND (credentials_sealed IS NOT NULL OR state<>'disabled')`, gateway)
	if res.Error != nil || res.RowsAffected == 0 {
		return res.Error
	}
	return notifyCloud(tx, tenant)
}

// TuyaCloudLinkStatus is what the gateway page shows about a Tuya Cloud gateway. Never a credential.
func (r *Repository) TuyaCloudLinkStatus(ctx context.Context, p domain.Principal, gateway string) (domain.TuyaCloudLink, error) {
	out := domain.TuyaCloudLink{GatewayID: gateway}
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		var models []string
		if e := tx.Raw(`SELECT model FROM core.gateways WHERE id=?`, gateway).Scan(&models).Error; e != nil {
			return e
		}
		if len(models) != 1 {
			return domain.ErrNotFound
		}
		if models[0] != domain.TuyaCloudGatewayModel {
			return domain.Because(domain.ErrInvalid, "not_a_cloud_gateway")
		}
		var rows []domain.TuyaCloudLink
		if e := tx.Raw(`SELECT l.gateway_id,(l.credentials_sealed IS NOT NULL) AS linked,l.region,l.channel,l.access_id_hint,l.state,l.state_at,l.reason,
      l.last_error_code,l.last_event_at,l.last_health_at,l.usage_month::timestamptz AS usage_month,l.events_month,l.api_calls_month,l.dropped_month,
      l.sync_requested_at,l.linked_at,l.rotated_at
    FROM core.tuya_cloud_links l WHERE l.gateway_id=?`, gateway).Scan(&rows).Error; e != nil {
			return e
		}
		if len(rows) == 1 {
			out = rows[0]
		}
		var counts struct{ Devices, Registered int }
		if e := tx.Raw(`SELECT count(*) AS devices,count(*) FILTER (WHERE EXISTS(SELECT 1 FROM core.devices d WHERE d.gateway_id=t.gateway_id
      AND lower(d.external_id)=t.tuya_id AND d.removed_at IS NULL)) AS registered FROM core.tuya_devices t WHERE t.gateway_id=? AND t.removed_at IS NULL`, gateway).Scan(&counts).Error; e != nil {
			return e
		}
		out.Devices, out.Registered = counts.Devices, counts.Registered
		return nil
	})
	return out, e
}

// RequestCloudSync asks the worker to list the project's devices again (a member pressed "sync").
func (r *Repository) RequestCloudSync(ctx context.Context, p domain.Principal, gateway string) error {
	if !p.CanManageDevices() {
		return domain.ErrForbidden
	}
	return r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if e := cloudGatewayModel(tx, gateway, domain.ErrNotFound); e != nil {
			return e
		}
		res := tx.Exec(`UPDATE core.tuya_cloud_links SET sync_requested_at=now(),updated_at=now() WHERE gateway_id=? AND credentials_sealed IS NOT NULL`, gateway)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return domain.Because(domain.ErrConflict, "not_linked")
		}
		return notifyCloud(tx, p.TenantID)
	})
}

// CloudLinks lists every link the worker should run, across tenants, through the definer function: which links
// and their revision and state, never credentials.
func (r *Repository) CloudLinks(ctx context.Context) ([]domain.CloudLinkRef, error) {
	out := []domain.CloudLinkRef{}
	e := r.db.WithContext(ctx).Raw(`SELECT tenant_id::text,gateway_id::text,revision,state FROM core.tuya_cloud_link_ids() LIMIT 10000`).Scan(&out).Error
	return out, e
}

// OpenLinkRow reads one link's stored configuration, the credentials still sealed, inside the link's tenant. Only
// the tuya-cloud worker calls it (and only it can open the result).
func (r *Repository) OpenLinkRow(ctx context.Context, tenant, gateway string) (domain.CloudLinkSecret, error) {
	var rows []domain.CloudLinkSecret
	e := r.tx(ctx, "", tenant, func(tx *gorm.DB) error {
		return tx.Raw(`SELECT l.tenant_id::text,l.gateway_id::text,l.region,l.channel,l.credentials_sealed,l.revision FROM core.tuya_cloud_links l
    JOIN core.gateways g ON g.tenant_id=l.tenant_id AND g.id=l.gateway_id AND g.revoked_at IS NULL AND g.model=?
    WHERE l.gateway_id=? AND l.credentials_sealed IS NOT NULL`, domain.TuyaCloudGatewayModel, gateway).Scan(&rows).Error
	})
	if e != nil {
		return domain.CloudLinkSecret{}, e
	}
	if len(rows) != 1 {
		return domain.CloudLinkSecret{}, domain.ErrNotFound
	}
	return rows[0], nil
}

// CloudSyncWanted reports when a sync was last asked for (nil when never), and which Tuya ids are registered on
// the gateway: the worker syncs when the request is newer than its last sync, and reads the initial state of the
// registered devices only.
func (r *Repository) CloudSyncWanted(ctx context.Context, tenant, gateway string) (*time.Time, []string, error) {
	var at []*time.Time
	registered := []string{}
	e := r.tx(ctx, "", tenant, func(tx *gorm.DB) error {
		if e := tx.Raw(`SELECT sync_requested_at FROM core.tuya_cloud_links WHERE gateway_id=?`, gateway).Scan(&at).Error; e != nil {
			return e
		}
		return tx.Raw(`SELECT DISTINCT lower(d.external_id) FROM core.devices d WHERE d.gateway_id=? AND d.removed_at IS NULL ORDER BY 1 LIMIT 1000`, gateway).Scan(&registered).Error
	})
	if e != nil || len(at) == 0 {
		return nil, registered, e
	}
	return at[0], registered, nil
}

// CompleteCloudSync clears the sync request the worker just served, unless a newer one arrived meanwhile.
func (r *Repository) CompleteCloudSync(ctx context.Context, tenant, gateway string, requested time.Time) error {
	return r.tx(ctx, "", tenant, func(tx *gorm.DB) error {
		return tx.Exec(`UPDATE core.tuya_cloud_links SET sync_requested_at=NULL WHERE gateway_id=? AND sync_requested_at<=?`, gateway, requested).Error
	})
}

// SetCloudLinkState records the worker's verdict on a link of a given revision (a verdict for credentials that
// were since rotated or unlinked is dropped). A link that comes back online restores its devices at once, as a
// returning bridge does; one that goes down takes them offline only after CloudLinkGrace (scanCloudLinksDown).
func (r *Repository) SetCloudLinkState(ctx context.Context, tenant, gateway string, revision int64, state, reason string, code int64) error {
	switch state {
	case domain.CloudLinkOnline, domain.CloudLinkOffline, domain.CloudLinkAuthFailed, domain.CloudLinkNotSubscribed, domain.CloudLinkQuota:
	default:
		return domain.ErrInvalid
	}
	return r.tx(ctx, "", tenant, func(tx *gorm.DB) error {
		var current []string
		if e := tx.Raw(`SELECT state FROM core.tuya_cloud_links WHERE gateway_id=? AND revision=? AND credentials_sealed IS NOT NULL FOR UPDATE`, gateway, revision).Scan(&current).Error; e != nil {
			return e
		}
		if len(current) != 1 {
			return domain.ErrNotFound
		}
		now := time.Now().UTC()
		if state == domain.CloudLinkOnline {
			if e := r.cloudOnline(tx, tenant, gateway, now); e != nil {
				return e
			}
			reason, code = "", 0
		}
		if e := tx.Exec(`UPDATE core.tuya_cloud_links SET state=?,state_at=CASE WHEN state<>? OR state_at IS NULL THEN ? ELSE state_at END,reason=?,last_error_code=?,updated_at=?
    WHERE gateway_id=?`, state, state, now, clipText(reason, 64), code, now, gateway).Error; e != nil {
			return e
		}
		if current[0] == state {
			return nil
		}
		return signal(tx, tenant, "inventory", gateway)
	})
}

// cloudOnline is agentOnline for a Tuya Cloud link: any down state ends, the devices it had taken offline come
// back, and the cloud_link_down reason is cleared.
func (r *Repository) cloudOnline(tx *gorm.DB, tenant, gateway string, now time.Time) error {
	if e := r.agentOnline(tx, tenant, gateway, cloudAgent, "cloud", now); e != nil {
		return e
	}
	return tx.Exec(`UPDATE core.tuya_devices SET reason='',updated_at=? WHERE gateway_id=? AND reason='cloud_link_down'`, now, gateway).Error
}

// scanCloudLinksDown is the part of ScanOffline for links the worker reports down for longer than CloudLinkGrace
// (or left linking, or unlinked): their devices go offline with source cloud_link_down. Only links that still have
// a device online are picked, so a long outage costs nothing per scan.
func (r *Repository) scanCloudLinksDown(tx *gorm.DB, tenant string, now time.Time) (int, error) {
	var gateways []string
	if e := tx.Raw(`SELECT l.gateway_id FROM core.tuya_cloud_links l
    JOIN core.gateways g ON g.tenant_id=l.tenant_id AND g.id=l.gateway_id AND g.revoked_at IS NULL AND g.model=?
    WHERE l.state IN ? AND l.state_at<?
      AND EXISTS(SELECT 1 FROM core.sensor_streams s LEFT JOIN core.stream_state st ON st.tenant_id=s.tenant_id AND st.gateway_id=s.gateway_id AND st.external_id=s.external_id
        WHERE s.gateway_id=l.gateway_id AND s.liveness='reported' AND st.offline IS NOT TRUE)
    ORDER BY l.gateway_id LIMIT 100`, domain.TuyaCloudGatewayModel, append(append([]string{}, cloudAgent.down...), cloudAgent.idle...), now.Add(-CloudLinkGrace)).Scan(&gateways).Error; e != nil {
		return 0, e
	}
	for _, g := range gateways {
		var streams []struct{ ExternalID, Name string }
		if e := tx.Raw(`SELECT s.external_id,s.name FROM core.sensor_streams s WHERE s.gateway_id=? AND s.liveness='reported' ORDER BY s.external_id LIMIT 1000`, g).Scan(&streams).Error; e != nil {
			return 0, e
		}
		for _, s := range streams {
			if e := r.setReportedLiveness(tx, tenant, g, s.ExternalID, s.Name, false, "cloud_link_down", now); e != nil {
				return 0, e
			}
		}
		if e := tx.Exec(`UPDATE core.tuya_devices SET reason='cloud_link_down',updated_at=? WHERE gateway_id=? AND removed_at IS NULL AND reason=''`, now, g).Error; e != nil {
			return 0, e
		}
	}
	return len(gateways), nil
}

// CloudImport is one device of a Tuya Cloud project as a sync found it: no key, its specification, and for a
// sub-device its hub and node.
type CloudImport struct {
	TuyaID, Name, Category, ProductID string
	ParentID, NodeID                  string
	Sub                               bool
	Spec                              []tuya.DP
}

// SaveCloudDevices stores the devices a sync found under a Tuya Cloud gateway, in system scope (the worker). A
// device found again gets the new name and specification and is no longer removed; devices missing from a sync
// are left alone. Nothing here holds a key.
func (r *Repository) SaveCloudDevices(ctx context.Context, tenant, gateway string, devices []CloudImport) (int, error) {
	saved := 0
	e := r.tx(ctx, "", tenant, func(tx *gorm.DB) error {
		if e := cloudGatewayModel(tx, gateway, domain.ErrNotFound); e != nil {
			return e
		}
		now := time.Now().UTC()
		for _, d := range devices {
			parent := d.ParentID
			if parent != "" && !edge.ValidDevice(parent) {
				parent = ""
			}
			if !edge.ValidDevice(d.TuyaID) || tuya.Validate(d.Spec) != nil {
				continue // one malformed entry does not cost the rest of the sync
			}
			tr := tuya.Translate(d.Category, d.Spec)
			spec, _ := json.Marshal(d.Spec)
			dpMap, _ := json.Marshal(tr.DPMap)
			gangs, _ := json.Marshal(tr.Gangs)
			if len(spec) > 65536 || len(tr.Exposes) > 65536 || len(dpMap) > 65536 {
				continue
			}
			if e := tx.Exec(`INSERT INTO core.tuya_devices(tenant_id,gateway_id,tuya_id,name,tuya_category,product_id,sub,spec,exposes,dp_map,gangs,category,local_capable,
      key_status,parent_tuya_id,node_id,imported_at,updated_at,removed_at)
    VALUES(?,?,?,?,?,?,?,?::jsonb,?::jsonb,?::jsonb,?::jsonb,?,?,'missing',?,?,?,?,NULL)
    ON CONFLICT(tenant_id,gateway_id,tuya_id) DO UPDATE SET name=EXCLUDED.name,tuya_category=EXCLUDED.tuya_category,product_id=EXCLUDED.product_id,sub=EXCLUDED.sub,
      spec=EXCLUDED.spec,exposes=EXCLUDED.exposes,dp_map=EXCLUDED.dp_map,gangs=EXCLUDED.gangs,category=EXCLUDED.category,local_capable=EXCLUDED.local_capable,
      parent_tuya_id=EXCLUDED.parent_tuya_id,node_id=EXCLUDED.node_id,updated_at=EXCLUDED.updated_at,removed_at=NULL`,
				tenant, gateway, d.TuyaID, clipText(d.Name, 128), clipText(d.Category, 32), clipText(d.ProductID, 64), d.Sub, string(spec), string(tr.Exposes), string(dpMap),
				string(gangs), tr.Category, tr.LocalCapable && !d.Sub, parent, clipText(d.NodeID, 64), now, now).Error; e != nil {
				return e
			}
			saved++
		}
		return signal(tx, tenant, "inventory", gateway)
	})
	return saved, dataError(e)
}

// CloudUsage is a link's monthly counters: messages accepted, OpenAPI calls made, messages dropped.
type CloudUsage struct {
	Events, APICalls, Dropped int64
}

// RecordCloudUsage adds the worker's counts since its last report to the link's monthly counters (which start
// over when the month changes, in UTC) and returns the month's totals. lastEvent moves last_event_at forward.
func (r *Repository) RecordCloudUsage(ctx context.Context, tenant, gateway string, add CloudUsage, lastEvent *time.Time, now time.Time) (CloudUsage, error) {
	var rows []struct{ EventsMonth, APICallsMonth, DroppedMonth int64 }
	now = now.UTC()
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
	e := r.tx(ctx, "", tenant, func(tx *gorm.DB) error {
		return tx.Raw(`UPDATE core.tuya_cloud_links SET
      events_month=CASE WHEN usage_month IS DISTINCT FROM ?::date THEN 0 ELSE events_month END+?,
      api_calls_month=CASE WHEN usage_month IS DISTINCT FROM ?::date THEN 0 ELSE api_calls_month END+?,
      dropped_month=CASE WHEN usage_month IS DISTINCT FROM ?::date THEN 0 ELSE dropped_month END+?,
      usage_month=?::date,last_event_at=greatest(last_event_at,?::timestamptz),updated_at=?
    WHERE gateway_id=? RETURNING events_month,api_calls_month,dropped_month`,
			month, add.Events, month, add.APICalls, month, add.Dropped, month, lastEvent, now, gateway).Scan(&rows).Error
	})
	if e != nil {
		return CloudUsage{}, e
	}
	if len(rows) != 1 {
		return CloudUsage{}, domain.ErrNotFound
	}
	return CloudUsage{Events: rows[0].EventsMonth, APICalls: rows[0].APICallsMonth, Dropped: rows[0].DroppedMonth}, nil
}

// CloudHealth is the worker's periodic report on one link, kept as a diagnostic packet (never a credential).
type CloudHealth struct {
	State     string `json:"state"`
	Connected bool   `json:"connected"`
	Events    int64  `json:"events_month"`
	APICalls  int64  `json:"api_calls_month"`
	Dropped   int64  `json:"dropped_month"`
	Budget    string `json:"budget,omitempty"`
}

// RecordCloudHealth stores the worker's health report for a link: last_health_at and a diagnostic packet. The
// packet is what the silence scan measures (CloudSilentAfter).
func (r *Repository) RecordCloudHealth(ctx context.Context, tenant, gateway string, h CloudHealth) error {
	e := r.tx(ctx, "", tenant, func(tx *gorm.DB) error {
		if e := cloudGatewayModel(tx, gateway, domain.ErrNotFound); e != nil {
			return e
		}
		now := time.Now().UTC()
		b, _ := json.Marshal(map[string]any{"cloud_health": h})
		if e := insertCloudDiagnostic(tx, tenant, gateway, string(b)); e != nil {
			return e
		}
		return tx.Exec(`UPDATE core.tuya_cloud_links SET last_health_at=?,updated_at=? WHERE gateway_id=?`, now, now, gateway).Error
	})
	return dataError(e)
}

func insertCloudDiagnostic(tx *gorm.DB, tenant, gateway, payload string) error {
	if e := tx.Exec(`INSERT INTO core.gateway_packets(id,tenant_id,gateway_id,payload) VALUES(?,?,?,?::jsonb)`, uuid.NewString(), tenant, gateway, payload).Error; e != nil {
		return e
	}
	return tx.Exec(`DELETE FROM core.gateway_packets WHERE gateway_id=? AND id IN (SELECT id FROM core.gateway_packets WHERE gateway_id=? ORDER BY received_at DESC,id DESC OFFSET 100)`, gateway, gateway).Error
}

// CloudCaptureResult is what one Message Service message did.
type CloudCaptureResult struct {
	Applied       int  // events that changed something
	Dropped       int  // events dropped by the budget guard (devices not registered)
	SyncRequested bool // an unknown or newly bound device asked for a device-list sync
}

var cloudEventNames = map[tuyacloud.EventKind]string{
	tuyacloud.EventStatus: "status", tuyacloud.EventOnline: "online", tuyacloud.EventOffline: "offline",
	tuyacloud.EventRenamed: "renamed", tuyacloud.EventRemoved: "removed", tuyacloud.EventBound: "bound",
}

// cloudDiagnostic is the copy of a message kept in core.gateway_packets: the protocol and, per event, its kind,
// device and the codes it reported. Never a value (a lock's state, a camera's text): an object, so the Minew
// projection over stored packets skips it.
func cloudDiagnostic(protocol int, events []tuyacloud.Event) string {
	list := []map[string]any{}
	for i, ev := range events {
		if i == 16 {
			break
		}
		d := map[string]any{"kind": cloudEventNames[ev.Kind], "dev": ev.DevID}
		if len(ev.Items) > 0 {
			codes := []string{}
			for j, it := range ev.Items {
				if j == 32 {
					break
				}
				codes = append(codes, it.Code)
			}
			d["codes"] = codes
		}
		list = append(list, d)
	}
	b, _ := json.Marshal(map[string]any{"cloud_protocol": protocol, "events": list, "count": len(events)})
	return string(b)
}

// CaptureTuyaCloud stores one decrypted Message Service message of a Tuya Cloud gateway. Like CaptureEdge it runs
// in one transaction under the gateway's row lock and keeps a bounded diagnostic copy; any message proves the link
// works. A status report is ordered and de-duplicated per data-point code by its report time (a redelivered or late
// message never overwrites a newer value), converted through the device's own specification and, for a device
// registered on this gateway, fed to the same pipeline as every other device; for any other device only the last
// values are kept. Events never create devices: an unknown or newly bound device asks for a sync instead.
// registeredOnly is the budget guard: events of devices not registered here are dropped.
func (r *Repository) CaptureTuyaCloud(ctx context.Context, tenant, gateway string, protocol int, events []tuyacloud.Event, registeredOnly bool) (CloudCaptureResult, error) {
	var out CloudCaptureResult
	e := r.tx(ctx, "", tenant, func(tx *gorm.DB) error {
		out = CloudCaptureResult{}
		if e := cloudGatewayModel(tx, gateway, domain.ErrUnauthorized); e != nil {
			if errors.Is(e, domain.ErrInvalid) {
				return domain.ErrForbidden
			}
			return e
		}
		// A message still in flight when the link was unlinked or revoked is not stored.
		var linked int64
		if e := tx.Raw(`SELECT count(*) FROM core.tuya_cloud_links WHERE gateway_id=? AND credentials_sealed IS NOT NULL`, gateway).Scan(&linked).Error; e != nil {
			return e
		}
		if linked != 1 {
			return domain.ErrForbidden
		}
		now := time.Now().UTC()
		if e := tx.Exec(`INSERT INTO core.gateway_packets(id,tenant_id,gateway_id,payload) VALUES(?,?,?,?::jsonb)`, uuid.NewString(), tenant, gateway, cloudDiagnostic(protocol, events)).Error; e != nil {
			return e
		}
		// Protocol 0 is the worker's own snapshot read through the OpenAPI, which says nothing about the Message
		// Service connection.
		if protocol != 0 {
			if e := r.cloudOnline(tx, tenant, gateway, now); e != nil {
				return e
			}
		}
		for _, ev := range events {
			applied, dropped, sync, e := r.captureCloudEvent(tx, tenant, gateway, ev, registeredOnly, now)
			if e != nil {
				return e
			}
			if applied {
				out.Applied++
			}
			if dropped {
				out.Dropped++
			}
			if sync && !out.SyncRequested {
				requested, e := requestCloudSync(tx, gateway, now)
				if e != nil {
					return e
				}
				out.SyncRequested = requested
			}
		}
		if e := signal(tx, tenant, "packet", gateway); e != nil {
			return e
		}
		return tx.Exec(`DELETE FROM core.gateway_packets WHERE gateway_id=? AND id IN (SELECT id FROM core.gateway_packets WHERE gateway_id=? ORDER BY received_at DESC,id DESC OFFSET 100)`, gateway, gateway).Error
	})
	return out, dataError(e)
}

// requestCloudSync marks the link as wanting a device-list sync, at most once per CloudSyncDebounce.
func requestCloudSync(tx *gorm.DB, gateway string, now time.Time) (bool, error) {
	res := tx.Exec(`UPDATE core.tuya_cloud_links SET sync_requested_at=? WHERE gateway_id=? AND credentials_sealed IS NOT NULL
    AND (sync_requested_at IS NULL OR sync_requested_at<?)`, now, gateway, now.Add(-CloudSyncDebounce))
	return res.RowsAffected > 0, res.Error
}

// millis reads a Tuya time as milliseconds: some device events carry seconds (bizData.time), reports milliseconds.
func millis(t int64) int64 {
	if t > 0 && t < 100_000_000_000 {
		return t * 1000
	}
	return t
}

// maxClockSkew is how far ahead of Aether's clock a Tuya report time may be. A later one (a device with a wrong
// clock, or a crafted message) is read as now: otherwise it would outrank every genuine report until that time.
const maxClockSkew = 5 * time.Minute

// reportTime is a report time in ms, from Tuya's (seconds or ms), fallback when absent, clamped to now+maxClockSkew.
func reportTime(t, fallback int64, now time.Time) int64 {
	t = millis(t)
	if t <= 0 {
		t = fallback
	}
	if t > now.Add(maxClockSkew).UnixMilli() {
		return now.UnixMilli()
	}
	return t
}

// cloudDeviceRow is a device of a Tuya Cloud gateway as the ingest needs it beyond tuyaDevice.
type cloudDeviceRow struct {
	Spec, ReportedT json.RawMessage
	Registered      bool
}

func (r *Repository) captureCloudEvent(tx *gorm.DB, tenant, gateway string, ev tuyacloud.Event, registeredOnly bool, now time.Time) (applied, dropped, sync bool, err error) {
	d, dpMap, found, e := tuyaDevice(tx, gateway, ev.DevID)
	if e != nil {
		return false, false, false, e
	}
	if !found {
		// Never create a device from an event: the sync lists it with its specification. Under the budget guard
		// the sync's API calls are not spent either.
		if registeredOnly {
			return false, true, false, nil
		}
		return false, false, ev.Kind != tuyacloud.EventRemoved, nil
	}
	var rows []cloudDeviceRow
	if e := tx.Raw(`SELECT t.spec,t.reported_t,EXISTS(SELECT 1 FROM core.devices x WHERE x.gateway_id=t.gateway_id AND lower(x.external_id)=t.tuya_id AND x.removed_at IS NULL) AS registered
    FROM core.tuya_devices t WHERE t.gateway_id=? AND t.tuya_id=?`, gateway, d.IEEE).Scan(&rows).Error; e != nil || len(rows) != 1 {
		return false, false, false, e
	}
	row := rows[0]
	if registeredOnly && !row.Registered {
		return false, true, false, nil
	}
	reported := map[string]int64{}
	_ = json.Unmarshal(row.ReportedT, &reported)
	at := reportTime(ev.T, now.UnixMilli(), now)
	switch ev.Kind {
	case tuyacloud.EventStatus:
		applied, e = r.saveCloudStatus(tx, tenant, gateway, d, dpMap, row, reported, ev, at, now)
		return applied, false, false, e
	case tuyacloud.EventOnline, tuyacloud.EventOffline:
		// A late or replayed liveness message never undoes a newer one.
		if at < reported["@online"] {
			return false, false, false, nil
		}
		online := ev.Kind == tuyacloud.EventOnline
		reason := ""
		if !online {
			reason = "unreachable"
		}
		mark, _ := json.Marshal(map[string]int64{"@online": at})
		if e := tx.Exec(`UPDATE core.tuya_devices SET available=?,available_at=?,reason=?,reported_t=reported_t||?::jsonb,updated_at=? WHERE gateway_id=? AND tuya_id=?`,
			online, now, reason, string(mark), now, gateway, d.IEEE).Error; e != nil {
			return false, false, false, e
		}
		if !row.Registered {
			return true, false, false, nil
		}
		return true, false, false, r.setReportedLiveness(tx, tenant, gateway, d.IEEE, d.FriendlyName, online, "cloud", now)
	case tuyacloud.EventRenamed:
		name := clipText(ev.Name, 128)
		res := tx.Exec(`UPDATE core.tuya_devices SET name=?,updated_at=? WHERE gateway_id=? AND tuya_id=? AND name<>?`, name, now, gateway, d.IEEE, name)
		return res.RowsAffected > 0, false, false, res.Error
	case tuyacloud.EventRemoved:
		res := tx.Exec(`UPDATE core.tuya_devices SET removed_at=?,updated_at=? WHERE gateway_id=? AND tuya_id=? AND removed_at IS NULL`, now, now, gateway, d.IEEE)
		return res.RowsAffected > 0, false, false, res.Error
	case tuyacloud.EventBound:
		// Re-bound (re-paired): its specification or hub may have changed.
		return false, false, true, nil
	}
	return false, false, false, nil
}

// saveCloudStatus orders a status report's items by report time, keeps per code only what is newer than the
// stored report time, converts the codes into data points through the device's specification and hands the values
// to the pipeline (registered devices) or keeps them as the device's last state (others).
func (r *Repository) saveCloudStatus(tx *gorm.DB, tenant, gateway string, d zigbee2mqtt.Device, dpMap map[string]tuya.Ref, row cloudDeviceRow,
	reported map[string]int64, ev tuyacloud.Event, at int64, now time.Time) (bool, error) {
	var spec []tuya.DP
	if e := json.Unmarshal(row.Spec, &spec); e != nil {
		return false, e
	}
	index := tuya.CodeIndex(spec)
	items := slices.Clone(ev.Items)
	for i := range items {
		items[i].T = reportTime(items[i].T, at, now)
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].T < items[j].T })
	dps := map[int]json.RawMessage{}
	fresh := map[string]int64{}
	for _, it := range items {
		id, ok := index[it.Code]
		if !ok || it.T <= reported[it.Code] {
			continue // a code the specification does not know, or a replayed / out-of-order value
		}
		reported[it.Code] = it.T
		fresh[it.Code] = it.T
		dps[id] = it.Value
	}
	if len(fresh) == 0 {
		return false, nil
	}
	// reported_t keeps only the specification's codes (and the liveness mark): at most tuya.MaxDPs entries, so it
	// stays within its bound whatever a device reports.
	kept := map[string]int64{}
	for code, t := range reported {
		if _, ok := index[code]; ok || code == "@online" {
			kept[code] = t
		}
	}
	marks, _ := json.Marshal(kept)
	if e := tx.Exec(`UPDATE core.tuya_devices SET reported_t=?::jsonb WHERE gateway_id=? AND tuya_id=?`, string(marks), gateway, d.IEEE).Error; e != nil {
		return false, e
	}
	properties := tuya.State(dpMap, dps)
	if len(properties) == 0 {
		return true, nil
	}
	converted, e := json.Marshal(properties)
	if e != nil {
		return false, e
	}
	features, _ := zigbee2mqtt.Features(d.Exposes)
	settable := zigbee2mqtt.SettableValuesIn(features, converted)
	if len(settable) > 0 {
		merged, _ := json.Marshal(settable)
		if e := tx.Exec(`UPDATE core.tuya_devices SET state=state||?::jsonb WHERE gateway_id=? AND tuya_id=? AND state IS DISTINCT FROM state||?::jsonb`, string(merged), gateway, d.IEEE, string(merged)).Error; e != nil {
			return false, e
		}
	}
	if !row.Registered {
		return true, nil
	}
	topic := "tuya-cloud/" + strconv.Itoa(ev.Protocol) + "/" + d.IEEE
	return true, r.ingestDeviceState(tx, tenant, gateway, d, features, settable, converted, topic, tuyacloud.FrameState, now)
}
