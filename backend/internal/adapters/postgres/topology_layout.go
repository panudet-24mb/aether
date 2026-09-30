package postgres

import (
	"aether/backend/internal/domain"
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
)

// Saving a layout is audited at most once per member and view in this window: a rearranging session is one
// decision, not one audit row per dragged node.
const layoutAuditWindow = 10 * time.Minute

// layoutWriteBatch is how many positions one INSERT carries.
const layoutWriteBatch = 500

func readLayout(tx *gorm.DB, view string) (domain.Layout, error) {
	out := domain.Layout{View: view, Positions: map[string]domain.CanvasPoint{}}
	var head []struct {
		Version   int64
		UpdatedBy *string
		UpdatedAt time.Time
	}
	if e := tx.Raw(`SELECT version,updated_by,updated_at FROM core.topology_layouts WHERE tenant_id=core.tenant_id() AND view=?`, view).Scan(&head).Error; e != nil {
		return out, e
	}
	if len(head) == 1 {
		out.Version, out.UpdatedBy = head[0].Version, head[0].UpdatedBy
		at := head[0].UpdatedAt
		out.UpdatedAt = &at
	}
	var rows []struct {
		NodeID string
		X, Y   float64
	}
	// A save never leaves more than MaxLayoutRows rows in a view, so this limit never cuts off a node.
	if e := tx.Raw(`SELECT node_id,x,y FROM core.topology_positions WHERE tenant_id=core.tenant_id() AND view=? ORDER BY node_id LIMIT ?`, view, domain.MaxLayoutRows).Scan(&rows).Error; e != nil {
		return out, e
	}
	for _, r := range rows {
		out.Positions[r.NodeID] = domain.CanvasPoint{X: r.X, Y: r.Y}
	}
	return out, nil
}

func (r *Repository) GetLayout(ctx context.Context, p domain.Principal, view string) (domain.Layout, error) {
	var out domain.Layout
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		var e error
		out, e = readLayout(tx, view)
		return e
	})
	return out, e
}

// pgArray renders ids as a PostgreSQL array literal. Only for values already matched against the node id pattern
// (lower-case letters, digits and . _ : -), none of which needs quoting inside an array literal.
func pgArray(ids []string) string { return "{" + strings.Join(ids, ",") + "}" }

// layoutGateways resolves the gateway each node belongs to, as far as the caller can see it (RLS). A node missing
// from the result is unknown to the server, gone (revoked gateway, removed device) or outside the caller's projects.
// The broker is not a gateway node and never appears.
func layoutGateways(tx *gorm.DB, nodes []string) (map[string]string, error) {
	out := map[string]string{}
	var gateways, externals []string
	for _, n := range nodes {
		switch {
		case strings.HasPrefix(n, "gw:"):
			gateways = append(gateways, strings.TrimPrefix(n, "gw:"))
		case strings.HasPrefix(n, "dev:"):
			externals = append(externals, strings.TrimPrefix(n, "dev:"))
		}
	}
	if len(gateways) > 0 {
		var ids []string
		if e := tx.Raw(`SELECT id::text FROM core.gateways WHERE tenant_id=core.tenant_id() AND id = ANY(?::uuid[]) AND revoked_at IS NULL`, pgArray(gateways)).Scan(&ids).Error; e != nil {
			return nil, e
		}
		for _, id := range ids {
			out["gw:"+id] = id
		}
	}
	if len(externals) > 0 {
		// Every place a device node on the canvas comes from: registrations first, then what gateways decoded or
		// listed (Zigbee coordinator, Tuya import, Edge LAN scan). The most recent sighting wins among those. The
		// lower() lookups use the expression indexes of migration 00041; Zigbee IEEE addresses are stored lower case.
		var rows []struct {
			Ext     string
			Gateway string
		}
		e := tx.Raw(`SELECT DISTINCT ON (ext) ext, gateway::text AS gateway FROM (
  SELECT lower(d.external_id) AS ext, d.gateway_id AS gateway, 0 AS prio, d.created_at AS at FROM core.devices d
   WHERE d.tenant_id=core.tenant_id() AND d.removed_at IS NULL AND lower(d.external_id) = ANY(CAST(@ext AS text[]))
  UNION ALL SELECT lower(s.external_id), s.gateway_id, 1, s.last_seen FROM core.sensor_streams s
   WHERE s.tenant_id=core.tenant_id() AND lower(s.external_id) = ANY(CAST(@ext AS text[]))
  UNION ALL SELECT z.ieee, z.gateway_id, 1, z.updated_at FROM core.z2m_devices z
   WHERE z.tenant_id=core.tenant_id() AND z.removed_at IS NULL AND z.ieee = ANY(CAST(@ext AS text[]))
  UNION ALL SELECT lower(t.tuya_id), t.gateway_id, 1, t.updated_at FROM core.tuya_devices t
   WHERE t.tenant_id=core.tenant_id() AND t.removed_at IS NULL AND lower(t.tuya_id) = ANY(CAST(@ext AS text[]))
  UNION ALL SELECT lower(l.device_id), l.gateway_id, 2, l.last_seen FROM core.edge_lan_devices l
   WHERE l.tenant_id=core.tenant_id() AND lower(l.device_id) = ANY(CAST(@ext AS text[]))
) c JOIN core.gateways g ON g.tenant_id=core.tenant_id() AND g.id=c.gateway AND g.revoked_at IS NULL
ORDER BY ext, prio, at DESC`, map[string]any{"ext": pgArray(externals)}).Scan(&rows).Error
		if e != nil {
			return nil, e
		}
		for _, row := range rows {
			out["dev:"+row.Ext] = row.Gateway
		}
	}
	return out, nil
}

func insufficientPrivilege(e error) bool {
	var pg *pgconn.PgError
	return errors.As(e, &pg) && pg.Code == "42501"
}

type layoutWrite struct {
	node    string
	gateway *string
	pt      domain.CanvasPoint
}

// writePositions upserts positions in batches and reports how many rows actually changed (an identical
// position is not a change).
func writePositions(tx *gorm.DB, view, actor string, writes []layoutWrite) (int64, error) {
	var changed int64
	for start := 0; start < len(writes); start += layoutWriteBatch {
		batch := writes[start:min(start+layoutWriteBatch, len(writes))]
		args := []any{view, actor}
		rows := make([]string, len(batch))
		for i, w := range batch {
			rows[i] = "(?::text,?::uuid,?::float8,?::float8)"
			args = append(args, w.node, w.gateway, w.pt.X, w.pt.Y)
		}
		res := tx.Exec(`INSERT INTO core.topology_positions AS p(tenant_id,view,node_id,gateway_id,x,y,updated_by,updated_at)
 SELECT core.tenant_id(),?,v.node_id,v.gateway_id,v.x,v.y,nullif(?,'')::uuid,now() FROM (VALUES `+strings.Join(rows, ",")+`) v(node_id,gateway_id,x,y)
 ON CONFLICT(tenant_id,view,node_id) DO UPDATE SET gateway_id=excluded.gateway_id,x=excluded.x,y=excluded.y,updated_by=excluded.updated_by,updated_at=excluded.updated_at
 WHERE (p.gateway_id,p.x,p.y) IS DISTINCT FROM (excluded.gateway_id,excluded.x,excluded.y)`, args...)
		if res.Error != nil {
			return changed, res.Error
		}
		changed += res.RowsAffected
	}
	return changed, nil
}

// SaveLayout applies one change when the caller's base version is still current. Nodes the server does not know,
// and nodes outside the caller's projects, are skipped and reported rather than failing the whole save: a device
// removed a moment ago must not make everyone's drag fail. A save that changes nothing keeps the version and sends
// no signal.
func (r *Repository) SaveLayout(ctx context.Context, p domain.Principal, ch domain.LayoutChange) (domain.LayoutSaved, error) {
	var out domain.LayoutSaved
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if e := tx.Exec(`INSERT INTO core.topology_layouts(tenant_id,view) VALUES(core.tenant_id(),?) ON CONFLICT DO NOTHING`, ch.View).Error; e != nil {
			return e
		}
		var version []int64
		if e := tx.Raw(`SELECT version FROM core.topology_layouts WHERE tenant_id=core.tenant_id() AND view=? FOR UPDATE`, ch.View).Scan(&version).Error; e != nil {
			return e
		}
		if len(version) != 1 {
			return domain.ErrNotFound
		}
		if version[0] != ch.BaseVersion {
			current, e := readLayout(tx, ch.View)
			if e != nil {
				return e
			}
			return domain.LayoutConflict{Current: current}
		}
		var all bool
		if e := tx.Raw(`SELECT core.scope_all()`).Scan(&all).Error; e != nil {
			return e
		}

		nodes := make([]string, 0, len(ch.Positions))
		for id := range ch.Positions {
			nodes = append(nodes, id)
		}
		sort.Strings(nodes)
		// A member who sees every row also clears orphans (nodes whose gateway or device is gone) on the way, so
		// the stored rows stay close to the live board. Their ids are resolved in the same query as the save's.
		var stored []string
		if all {
			if e := tx.Raw(`SELECT node_id FROM core.topology_positions WHERE tenant_id=core.tenant_id() AND view=? AND node_id<>'broker' ORDER BY node_id`, ch.View).Scan(&stored).Error; e != nil {
				return e
			}
		}
		gateways, e := layoutGateways(tx, append(append([]string{}, nodes...), stored...))
		if e != nil {
			return e
		}

		var changed int64
		var orphans []string
		for _, id := range stored {
			if _, live := gateways[id]; !live {
				orphans = append(orphans, id)
			}
		}
		removals := append(orphans, ch.Remove...)
		if len(removals) > 0 {
			sort.Strings(removals)
			// The broker may be forgotten only by a member who sees every project.
			res := tx.Exec(`DELETE FROM core.topology_positions WHERE tenant_id=core.tenant_id() AND view=? AND node_id = ANY(?::text[]) AND (? OR node_id<>'broker')`, ch.View, pgArray(removals), all)
			if res.Error != nil {
				return res.Error
			}
			changed += res.RowsAffected
		}

		ignored := map[string]bool{}
		writes := []layoutWrite{}
		for _, id := range nodes {
			pt := ch.Positions[id]
			if id == "broker" {
				if !all {
					ignored[id] = true
					continue
				}
				writes = append(writes, layoutWrite{id, nil, pt})
				continue
			}
			gw, ok := gateways[id]
			if !ok {
				ignored[id] = true
				continue
			}
			writes = append(writes, layoutWrite{id, &gw, pt})
		}
		if all {
			n, e := writePositions(tx, ch.View, p.UserID, writes)
			if e != nil {
				return e
			}
			changed += n
		} else {
			n, skipped, e := writeScoped(tx, ch.View, p.UserID, writes)
			if e != nil {
				return e
			}
			changed += n
			for _, id := range skipped {
				ignored[id] = true
			}
		}

		out.Ignored = make([]string, 0, len(ignored))
		for id := range ignored {
			out.Ignored = append(out.Ignored, id)
		}
		sort.Strings(out.Ignored)
		if changed == 0 {
			layout, e := readLayout(tx, ch.View)
			out.Layout = layout
			return e
		}

		var rows int64
		if e := tx.Raw(`SELECT core.topology_layout_rows(?)`, ch.View).Scan(&rows).Error; e != nil {
			return e
		}
		if rows > domain.MaxLayoutRows {
			return domain.Because(domain.ErrInvalid, "layout_full")
		}
		if e := tx.Exec(`UPDATE core.topology_layouts SET version=version+1,updated_by=nullif(?,'')::uuid,updated_at=now() WHERE tenant_id=core.tenant_id() AND view=?`, p.UserID, ch.View).Error; e != nil {
			return e
		}
		action := "topology.layout_saved"
		var recent int64
		if e := tx.Raw(`SELECT count(*) FROM core.audit_logs WHERE tenant_id=core.tenant_id() AND actor_id=? AND action=? AND at > now() - make_interval(secs => ?)`, p.UserID, action, layoutAuditWindow.Seconds()).Scan(&recent).Error; e != nil {
			return e
		}
		if recent == 0 {
			if e := audit(tx, p, action, uuid.Nil.String()); e != nil {
				return e
			}
		}
		if e := signal(tx, p.TenantID, "layout", ""); e != nil {
			return e
		}
		layout, e := readLayout(tx, ch.View)
		out.Layout = layout
		return e
	})
	return out, e
}

// writeScoped is writePositions for a member restricted to some projects. Such a member can meet a stored row it
// cannot see (a device that moved in from another project while its row still names the old gateway): the upsert
// then fails the row policy. The whole batch is tried in one savepoint first; only when it is refused does each
// node get its own savepoint, so that node alone is skipped.
func writeScoped(tx *gorm.DB, view, actor string, writes []layoutWrite) (int64, []string, error) {
	if len(writes) == 0 {
		return 0, nil, nil
	}
	if e := tx.Exec(`SAVEPOINT layout_batch`).Error; e != nil {
		return 0, nil, e
	}
	n, e := writePositions(tx, view, actor, writes)
	if e == nil {
		return n, nil, tx.Exec(`RELEASE SAVEPOINT layout_batch`).Error
	}
	if !insufficientPrivilege(e) {
		return 0, nil, e
	}
	if e := tx.Exec(`ROLLBACK TO SAVEPOINT layout_batch`).Error; e != nil {
		return 0, nil, e
	}
	var changed int64
	var skipped []string
	for _, w := range writes {
		if e := tx.Exec(`SAVEPOINT layout_node`).Error; e != nil {
			return changed, skipped, e
		}
		n, e := writePositions(tx, view, actor, []layoutWrite{w})
		if e != nil {
			if !insufficientPrivilege(e) {
				return changed, skipped, e
			}
			if e := tx.Exec(`ROLLBACK TO SAVEPOINT layout_node`).Error; e != nil {
				return changed, skipped, e
			}
			skipped = append(skipped, w.node)
			continue
		}
		if e := tx.Exec(`RELEASE SAVEPOINT layout_node`).Error; e != nil {
			return changed, skipped, e
		}
		changed += n
	}
	return changed, skipped, tx.Exec(`RELEASE SAVEPOINT layout_batch`).Error
}

// syncLayoutNodes keeps stored canvas positions in step with the inventory: after a registration is created,
// moved, removed or restored, or a gateway is revoked, each named node is re-resolved and its row either follows the
// node's new gateway (so project scope keeps applying to it) or is deleted when the node is gone. Callers run it last
// in their transaction, after their own row locks; it then takes the layout header rows (in view order) before any
// position row, the same order SaveLayout uses, so the two never wait on each other's rows in reverse.
//
// gateway, when set, also re-resolves every node stored under that gateway (read after the header lock, so a save
// that raced the caller cannot slip a row in unseen).
func syncLayoutNodes(tx *gorm.DB, tenant string, nodes []string, gateway string) error {
	var views []string
	if e := tx.Raw(`SELECT view FROM core.topology_layouts WHERE tenant_id=core.tenant_id() ORDER BY view FOR UPDATE`).Scan(&views).Error; e != nil {
		return e
	}
	if len(views) == 0 {
		return nil
	}
	if gateway != "" {
		var under []string
		if e := tx.Raw(`SELECT DISTINCT node_id FROM core.topology_positions WHERE tenant_id=core.tenant_id() AND gateway_id=?`, gateway).Scan(&under).Error; e != nil {
			return e
		}
		nodes = append(append(nodes, under...), "gw:"+gateway)
	}
	if len(nodes) == 0 {
		return nil
	}
	sort.Strings(nodes)
	gateways, e := layoutGateways(tx, nodes)
	if e != nil {
		return e
	}
	var gone []string
	var changed int64
	for _, id := range nodes {
		gw, live := gateways[id]
		if !live {
			gone = append(gone, id)
			continue
		}
		res := tx.Exec(`UPDATE core.topology_positions SET gateway_id=?::uuid WHERE tenant_id=core.tenant_id() AND node_id=? AND gateway_id IS DISTINCT FROM ?::uuid`, gw, id, gw)
		if res.Error != nil {
			return res.Error
		}
		changed += res.RowsAffected
	}
	if len(gone) > 0 {
		res := tx.Exec(`DELETE FROM core.topology_positions WHERE tenant_id=core.tenant_id() AND node_id = ANY(?::text[])`, pgArray(gone))
		if res.Error != nil {
			return res.Error
		}
		changed += res.RowsAffected
	}
	if changed == 0 {
		return nil
	}
	// Open canvases fetch the layout again (a node moved into or out of someone's projects).
	if e := tx.Exec(`UPDATE core.topology_layouts SET version=version+1,updated_at=now() WHERE tenant_id=core.tenant_id()`).Error; e != nil {
		return e
	}
	return signal(tx, tenant, "layout", "")
}

// deviceLayoutNode is the canvas node id of a registration.
func deviceLayoutNode(tx *gorm.DB, device string) ([]string, error) {
	var ext []string
	if e := tx.Raw(`SELECT lower(external_id) FROM core.devices WHERE id=?`, device).Scan(&ext).Error; e != nil {
		return nil, e
	}
	out := []string{}
	for _, x := range ext {
		if domain.ValidLayoutNode("dev:" + x) {
			out = append(out, "dev:"+x)
		}
	}
	return out, nil
}

// syncDeviceLayout re-resolves the canvas node of one registration (see syncLayoutNodes).
func syncDeviceLayout(tx *gorm.DB, tenant, device string) error {
	nodes, e := deviceLayoutNode(tx, device)
	if e != nil || len(nodes) == 0 {
		return e
	}
	return syncLayoutNodes(tx, tenant, nodes, "")
}

// syncGatewayLayout forgets a revoked gateway's node and re-resolves every device node stored under it.
func syncGatewayLayout(tx *gorm.DB, tenant, gateway string) error {
	return syncLayoutNodes(tx, tenant, nil, gateway)
}
