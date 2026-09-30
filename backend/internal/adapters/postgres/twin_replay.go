package postgres

import (
	"aether/backend/internal/domain"
	"container/list"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
)

// Digital twin replay (docs/platform/digital-twin.md, phase P3): the history of one site between two times, as a
// keyframe at `from` plus the changes after it. Environment values come from core.sample_rollup (5-minute buckets,
// re-aggregated for longer windows), people from core.presence_history, alerts from core.alerts. Everything is read
// under the caller's row level security (tenant and project scope), with a 5 s statement timeout, TwinReplayTimeout
// for the whole request and TwinReplayConcurrency requests per workspace at once (twinBounded).

// TwinSettings reads the workspace's twin settings (the defaults when its owner saved none).
func (r *Repository) TwinSettings(ctx context.Context, p domain.Principal) (domain.TwinSettings, error) {
	var out domain.TwinSettings
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		var e error
		out, e = loadTwinSettings(tx)
		return e
	})
	return out, e
}

// SetTwinSettings stores them; owner only (checked here and by the table's policies), audited.
func (r *Repository) SetTwinSettings(ctx context.Context, p domain.Principal, in domain.TwinSettings) (domain.TwinSettings, error) {
	if e := in.Validate(); e != nil {
		return domain.TwinSettings{}, e
	}
	var out domain.TwinSettings
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if e := requireOwner(tx, p); e != nil {
			return e
		}
		if e := tx.Exec(`INSERT INTO core.twin_settings(tenant_id,people_replay,people_replay_days,display_people,updated_by,updated_at)
      VALUES(core.tenant_id(),?,?,?,?,now())
      ON CONFLICT(tenant_id) DO UPDATE SET people_replay=EXCLUDED.people_replay,people_replay_days=EXCLUDED.people_replay_days,
        display_people=EXCLUDED.display_people,updated_by=EXCLUDED.updated_by,updated_at=now()`,
			in.PeopleReplay, in.PeopleReplayDays, in.DisplayPeople, p.UserID).Error; e != nil {
			return e
		}
		action := fmt.Sprintf("twin.settings_changed:%s:%d:%s", in.PeopleReplay, in.PeopleReplayDays, in.DisplayPeople)
		if e := audit(tx, p, action, p.TenantID); e != nil {
			return e
		}
		var e error
		out, e = loadTwinSettings(tx)
		return e
	})
	return out, e
}

// twinSiteRows is what a replay needs to know about a site: its placed gateways and devices, the layout revision and
// the workspace's settings. ErrNotFound when the site is outside the caller's scope.
type twinSiteRows struct {
	now       time.Time
	settings  domain.TwinSettings
	gateways  map[string]bool
	devices   map[string][]string // external id -> placed device ids
	externals []string
	revision  int
	scope     string
	erasures  int64
}

func loadTwinSite(tx *gorm.DB, siteID string) (twinSiteRows, error) {
	out := twinSiteRows{gateways: map[string]bool{}, devices: map[string][]string{}}
	var head []struct {
		Now      time.Time
		Found    bool
		Revision int
		Scope    string
		Erasures int64
	}
	if e := tx.Raw(`SELECT now() AS now, EXISTS(SELECT 1 FROM core.sites WHERE id=? AND archived_at IS NULL) AS found,
       coalesce((SELECT sum(revision) FROM core.floors WHERE site_id=?),0) AS revision,
       coalesce(current_setting('app.project_scope',true),'') AS scope, core.erasure_count() AS erasures`, siteID, siteID).Scan(&head).Error; e != nil {
		return out, e
	}
	if len(head) != 1 || !head[0].Found {
		return out, domain.ErrNotFound
	}
	out.now, out.revision, out.scope, out.erasures = head[0].Now, head[0].Revision, head[0].Scope, head[0].Erasures
	settings, e := loadTwinSettings(tx)
	if e != nil {
		return out, e
	}
	out.settings = settings
	var placed []struct{ AssetKind, AssetID, External string }
	if e := tx.Raw(`SELECT pl.asset_kind, pl.asset_id, coalesce(lower(d.external_id),'') AS external
     FROM core.floor_placements pl
     JOIN core.floors f ON f.tenant_id=pl.tenant_id AND f.id=pl.floor_id AND f.site_id=?
     LEFT JOIN core.gateways g ON pl.asset_kind='gateway' AND g.tenant_id=pl.tenant_id AND g.id=pl.asset_id AND g.revoked_at IS NULL
     LEFT JOIN core.devices d ON pl.asset_kind='device' AND d.tenant_id=pl.tenant_id AND d.id=pl.asset_id AND d.removed_at IS NULL
     WHERE g.id IS NOT NULL OR d.id IS NOT NULL ORDER BY pl.asset_kind, pl.asset_id LIMIT ?`, siteID, domain.TwinMaxPlacements).Scan(&placed).Error; e != nil {
		return out, e
	}
	for _, pl := range placed {
		if pl.AssetKind == "gateway" {
			out.gateways[pl.AssetID] = true
			continue
		}
		if _, ok := out.devices[pl.External]; !ok {
			out.externals = append(out.externals, pl.External)
		}
		out.devices[pl.External] = append(out.devices[pl.External], pl.AssetID)
	}
	return out, nil
}

func (s twinSiteRows) gatewayList() []string {
	out := make([]string, 0, len(s.gateways))
	for g := range s.gateways {
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}

// peopleFrom is how far back this workspace shows people: its setting, within the movement history's retention.
func (s twinSiteRows) peopleFrom() time.Time {
	days := min(s.settings.PeopleReplayDays, max(s.settings.HistoryDays, 1))
	return s.now.Add(-time.Duration(days) * 24 * time.Hour)
}

// personalSet asks core.twin_impersonal_tags which of these tags are not carried by people; every other is personal.
func personalSet(tx *gorm.DB, ids []string) (func(string) bool, error) {
	impersonal := map[string]bool{}
	if len(ids) > 0 {
		var rows []string
		if e := tx.Raw(`SELECT t FROM core.twin_impersonal_tags(?::text[], ?::text[]) t`, pgArray(domain.WornProfileIDs()), pgTextArray(ids)).Scan(&rows).Error; e != nil {
			return nil, e
		}
		for _, id := range rows {
			impersonal[id] = true
		}
	}
	return func(ext string) bool { return ext != "" && !impersonal[ext] }, nil
}

// twinSlots lets TwinReplayConcurrency timeline / replay requests of one workspace run at once; a request waits up
// to 3 s for a slot and then answers ErrRateLimited.
type twinSlotSet struct {
	mu    sync.Mutex
	slots map[string]chan struct{}
}

var twinSlots = &twinSlotSet{slots: map[string]chan struct{}{}}

func (s *twinSlotSet) acquire(ctx context.Context, tenant string) (func(), error) {
	s.mu.Lock()
	ch, ok := s.slots[tenant]
	if !ok {
		ch = make(chan struct{}, domain.TwinReplayConcurrency)
		s.slots[tenant] = ch
	}
	s.mu.Unlock()
	wait := time.NewTimer(3 * time.Second)
	defer wait.Stop()
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-wait.C:
		return nil, domain.ErrRateLimited
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// twinBounded runs one timeline / replay read: a slot of the workspace, TwinReplayTimeout for the whole transaction
// and 5 s per statement. Running out of either is ErrTooDense with half the window as the end to ask for.
func (r *Repository) twinBounded(ctx context.Context, p domain.Principal, from, to time.Time, fn func(*gorm.DB) error) error {
	release, e := twinSlots.acquire(ctx, p.TenantID)
	if e != nil {
		return e
	}
	defer release()
	bounded, cancel := context.WithTimeout(ctx, domain.TwinReplayTimeout)
	defer cancel()
	e = r.tx(bounded, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if e := tx.Exec(`SET LOCAL statement_timeout = '5s'`).Error; e != nil {
			return e
		}
		return fn(tx)
	})
	var pg *pgconn.PgError
	if e != nil && ctx.Err() == nil && (errors.Is(bounded.Err(), context.DeadlineExceeded) || (errors.As(e, &pg) && pg.Code == "57014")) {
		return domain.ErrTooDense{HintTo: from.Add(to.Sub(from) / 2)}
	}
	return e
}

type twinAlertHist struct {
	ID, Severity, EventType, Title, GatewayID, External string
	OpenedAt                                            time.Time
	AckedAt, ResolvedAt                                 *time.Time
}

// siteAlerts: alerts of the site's gateways and placed devices (and of the given people) that were open at some
// point of [from, to].
func siteAlerts(tx *gorm.DB, site twinSiteRows, people []string, from, to time.Time, limit int) ([]twinAlertHist, error) {
	var out []twinAlertHist
	exts := append(slices.Clone(site.externals), people...)
	if len(site.gateways)+len(exts) == 0 {
		return out, nil
	}
	e := tx.Raw(`SELECT id, severity, event_type, title, coalesce(gateway_id::text,'') AS gateway_id, lower(external_id) AS external,
       opened_at, acked_at, resolved_at
     FROM core.alerts WHERE opened_at <= ? AND (resolved_at IS NULL OR resolved_at > ?)
       AND (gateway_id = ANY(?::uuid[]) OR lower(external_id) = ANY(?::text[]))
     ORDER BY opened_at, id LIMIT ?`, to, from, pgArray(site.gatewayList()), pgTextArray(exts), limit).Scan(&out).Error
	return out, e
}

// TwinTimeline is the replay's time track: what history exists, the SOS / hazard / serious alerts of the window as
// markers, and per-bucket density of alerts, door changes and people moving.
func (r *Repository) TwinTimeline(ctx context.Context, p domain.Principal, siteID string, from, to time.Time, people string) (domain.TwinTimeline, error) {
	out := domain.TwinTimeline{SiteID: siteID, From: from.UTC(), To: to.UTC(), Markers: []domain.TwinMarker{}}
	bucket, e := domain.TwinReplayBucket(to.Sub(from), 0)
	if e != nil {
		return out, e
	}
	_, isDisplay := domain.DisplayFrom(ctx)
	e = r.twinBounded(ctx, p, from, to, func(tx *gorm.DB) error {
		site, e := loadTwinSite(tx, siteID)
		if e != nil {
			return e
		}
		mode := domain.TwinPeopleFor(p, people, site.settings, isDisplay, false)
		out.PeopleMode = mode
		out.Available.Now = site.now
		var avail []struct{ EnvFrom, PeopleFrom, MarkersFrom *time.Time }
		// With people off, the movement history is not read at all.
		if e := tx.Raw(`SELECT (SELECT min(bucket) FROM core.sample_rollup WHERE external_id = ANY(?::text[])) AS env_from,
         CASE WHEN ? THEN (SELECT min(at) FROM core.presence_history) END AS people_from,
         (SELECT min(occurred_at) FROM core.device_events) AS markers_from`, pgTextArray(site.externals), mode != domain.TwinPeopleOff).Scan(&avail).Error; e != nil {
			return e
		}
		if len(avail) == 1 {
			out.Available.EnvFrom, out.Available.MarkersFrom = avail[0].EnvFrom, avail[0].MarkersFrom
			if mode != domain.TwinPeopleOff {
				pf := site.peopleFrom()
				if avail[0].PeopleFrom != nil && avail[0].PeopleFrom.After(pf) {
					pf = *avail[0].PeopleFrom
				}
				out.Available.PeopleFrom = &pf
			}
		}
		n := int((to.Sub(from) + time.Duration(bucket)*time.Second - 1) / (time.Duration(bucket) * time.Second))
		out.Density = domain.TwinDensity{BucketSec: bucket, From: out.From, Lanes: map[string][]int{"alerts": make([]int, n), "door": make([]int, n), "zone": make([]int, n)}}
		index := func(at time.Time) int {
			i := int(at.Sub(from) / (time.Duration(bucket) * time.Second))
			if i < 0 || i >= n {
				return -1
			}
			return i
		}
		alerts, e := siteAlerts(tx, site, nil, from, to, 5000)
		if e != nil {
			return e
		}
		mentioned := []string{}
		for _, a := range alerts {
			if a.External != "" {
				mentioned = append(mentioned, a.External)
			}
		}
		personal, e := personalSet(tx, mentioned)
		if e != nil {
			return e
		}
		for _, a := range alerts {
			i := index(a.OpenedAt)
			if i < 0 {
				continue
			}
			out.Density.Lanes["alerts"][i]++
			kind := "alert"
			switch {
			case a.EventType == domain.EventButton:
				kind = "sos"
			case a.EventType == domain.EventHazard:
				kind = "hazard"
			case a.Severity != "critical" && a.Severity != "warning":
				continue
			}
			if len(out.Markers) >= domain.TwinTimelineMarkers {
				continue
			}
			label := a.Title
			if mode != domain.TwinPeopleNamed && personal(a.External) {
				label = domain.TwinEventTitle(a.EventType)
			}
			m := domain.TwinMarker{At: a.OpenedAt, Kind: kind, Severity: a.Severity, AlertID: a.ID, GatewayID: a.GatewayID, Label: label}
			if ids := site.devices[a.External]; len(ids) > 0 {
				m.DeviceID = ids[0]
			}
			out.Markers = append(out.Markers, m)
		}
		if len(site.externals) > 0 {
			var doors []time.Time
			if e := tx.Raw(`SELECT occurred_at FROM core.device_events WHERE event_type IN ('door_open','door_closed')
           AND lower(external_id) = ANY(?::text[]) AND occurred_at >= ? AND occurred_at < ? ORDER BY occurred_at LIMIT 20000`,
				pgTextArray(site.externals), from, to).Scan(&doors).Error; e != nil {
				return e
			}
			for _, at := range doors {
				if i := index(at); i >= 0 {
					out.Density.Lanes["door"][i]++
				}
			}
		}
		if mode != domain.TwinPeopleOff && len(site.gateways) > 0 {
			var moves []time.Time
			gws := pgArray(site.gatewayList())
			if e := tx.Raw(`SELECT at FROM core.presence_history WHERE at >= ? AND at < ? AND at >= ?
           AND (gateway_id = ANY(?::uuid[]) OR from_gateway_id = ANY(?::uuid[])) ORDER BY at LIMIT 50000`,
				from, to, site.peopleFrom(), gws, gws).Scan(&moves).Error; e != nil {
				return e
			}
			for _, at := range moves {
				if i := index(at); i >= 0 {
					out.Density.Lanes["zone"][i]++
				}
			}
		}
		return nil
	})
	return out, e
}

// twinPersonMove is one row of core.presence_history.
type twinPersonMove struct {
	External  string
	At        time.Time
	GatewayID *string
}

// TwinReplay answers the replay of one window as JSON, with its ETag and the people mode it used. Windows wholly in
// the past are cached (see twinReplayCache); tracks responses never are (their pseudonyms are drawn per response).
func (r *Repository) TwinReplay(ctx context.Context, p domain.Principal, q domain.TwinReplayQuery) ([]byte, string, string, error) {
	bucket, e := domain.TwinReplayBucket(q.To.Sub(q.From), q.Bucket)
	if e != nil {
		return nil, "", "", e
	}
	display := ""
	if d, ok := domain.DisplayFrom(ctx); ok {
		display = d.ID
	}
	step := time.Duration(bucket) * time.Second
	from := q.From.UTC().Truncate(step)
	to := q.To.UTC()
	if rem := to.Sub(from) % step; rem != 0 {
		to = to.Add(step - rem)
	}
	if to.Sub(from) > domain.TwinReplayMaxWindow || int(to.Sub(from)/step) > domain.TwinReplayMaxBuckets {
		return nil, "", "", domain.ErrInvalid
	}
	var body []byte
	var mode string
	e = r.twinBounded(ctx, p, from, to, func(tx *gorm.DB) error {
		site, e := loadTwinSite(tx, q.SiteID)
		if e != nil {
			return e
		}
		mode = domain.TwinPeopleFor(p, q.People, site.settings, display != "", false)
		if len(site.externals) > domain.TwinReplayMaxDevices {
			return domain.ErrInvalid
		}
		// Size first: devices x buckets of the environment series, before anything is read.
		if n := int(to.Sub(from) / step); (len(q.Layers) == 0 || slices.Contains(q.Layers, "env")) && len(site.externals)*n > domain.TwinReplayMaxCells {
			fit := max(1, domain.TwinReplayMaxCells/len(site.externals))
			return domain.ErrTooDense{HintTo: from.Add(time.Duration(fit) * step)}
		}
		past := to.Before(site.now.Add(-10 * time.Minute))
		updated := ""
		if site.settings.UpdatedAt != nil {
			updated = site.settings.UpdatedAt.UTC().Format(time.RFC3339Nano)
		}
		key := strings.Join([]string{p.TenantID, p.UserID, display, p.Role, site.scope, strconv.FormatInt(site.erasures, 10), strconv.Itoa(site.revision),
			updated, mode, q.SiteID, from.Format(time.RFC3339), to.Format(time.RFC3339), strconv.Itoa(bucket), strings.Join(q.Layers, ",")}, "|")
		cacheable := past && mode != domain.TwinPeopleTracks
		if cacheable {
			if b, ok := twinReplays.get(key, site.now); ok {
				body = b
				return nil
			}
		}
		out, e := buildReplay(tx, site, q, from, to, bucket, mode)
		if e != nil {
			return e
		}
		if body, e = json.Marshal(out); e != nil {
			return e
		}
		if cacheable {
			twinReplays.put(key, body, site.now)
		}
		return nil
	})
	if e != nil {
		return nil, "", mode, e
	}
	sum := sha256.Sum256(body)
	return body, `"` + hex.EncodeToString(sum[:12]) + `"`, mode, nil
}

func buildReplay(tx *gorm.DB, site twinSiteRows, q domain.TwinReplayQuery, from, to time.Time, bucket int, mode string) (domain.TwinReplay, error) {
	out := domain.TwinReplay{SiteID: q.SiteID, From: from, To: to, BucketSec: bucket, GeneratedAt: site.now, PeopleMode: mode}
	step := time.Duration(bucket) * time.Second
	n := int(to.Sub(from) / step)
	layer := func(l string) bool { return len(q.Layers) == 0 || slices.Contains(q.Layers, l) }

	if layer("env") {
		env := &domain.TwinReplayEnv{Buckets: n, Series: map[string]domain.TwinSeries{}}
		// Only fixed sensors (the rollup holds no others; a tag registered as a person's since is left out too).
		personal, e := personalSet(tx, site.externals)
		if e != nil {
			return out, e
		}
		fixed := []string{}
		for _, ext := range site.externals {
			if !personal(ext) {
				fixed = append(fixed, ext)
			}
		}
		if len(fixed) > 0 {
			var rows []struct {
				External string
				I        int
				T, H     *float64
				Motion   *int
				Door     *int
			}
			if e := tx.Raw(`SELECT r.external_id AS external, floor(extract(epoch FROM (r.bucket - ?::timestamptz)) / ?)::int AS i,
           (sum(r.t_avg * r.n) FILTER (WHERE r.t_avg IS NOT NULL) / nullif(sum(r.n) FILTER (WHERE r.t_avg IS NOT NULL), 0))::float8 AS t,
           (sum(r.h_avg * r.n) FILTER (WHERE r.h_avg IS NOT NULL) / nullif(sum(r.n) FILTER (WHERE r.h_avg IS NOT NULL), 0))::float8 AS h,
           max(r.motion)::int AS motion,
           ((array_agg(r.door ORDER BY r.bucket DESC) FILTER (WHERE r.door IS NOT NULL))[1])::int AS door
         FROM core.sample_rollup r
         WHERE r.external_id = ANY(?::text[]) AND r.bucket >= ? AND r.bucket < ?
         GROUP BY 1, 2`, from, bucket, pgTextArray(fixed), from, to).Scan(&rows).Error; e != nil {
				return out, e
			}
			for _, row := range rows {
				if row.I < 0 || row.I >= n {
					continue
				}
				for _, id := range site.devices[row.External] {
					s, ok := env.Series[id]
					if !ok {
						s = domain.TwinSeries{T: make([]*float64, n), H: make([]*float64, n), Motion: make([]*int, n), Door: make([]*int, n)}
					}
					if row.T != nil {
						v := round1(*row.T)
						s.T[row.I] = &v
					}
					if row.H != nil {
						v := round1(*row.H)
						s.H[row.I] = &v
					}
					s.Motion[row.I], s.Door[row.I] = row.Motion, row.Door
					env.Series[id] = s
				}
			}
		}
		out.Env = env
	}

	// People, and which of them are on this site (their zone gateway is placed here) at some point of the window. Only
	// moves with an end on this site are read, so other sites cost nothing and never make this window too dense; with
	// people off nothing of the movement history is read (alerts then come from the site's gateways and devices).
	var peopleExts []string
	peopleFrom := site.peopleFrom()
	if layer("people") && mode != domain.TwinPeopleOff && len(site.gateways) > 0 && to.After(peopleFrom) {
		start := from
		if start.Before(peopleFrom) {
			start = peopleFrom
		}
		gws := pgArray(site.gatewayList())
		var moves []twinPersonMove
		if e := tx.Raw(`SELECT external_id AS external, at, gateway_id::text AS gateway_id FROM core.presence_history
         WHERE at > ? AND at <= ? AND (gateway_id = ANY(?::uuid[]) OR from_gateway_id = ANY(?::uuid[]))
         ORDER BY at, external_id LIMIT ?`, start, to, gws, gws, domain.TwinReplayMaxDeltas+1).Scan(&moves).Error; e != nil {
			return out, e
		}
		if len(moves) > domain.TwinReplayMaxDeltas {
			return out, domain.ErrTooDense{HintTo: moves[domain.TwinReplayMaxDeltas-1].At}
		}
		// Who could be here at `start`: every tag with a zone or a roaming registration (one row per tag), and whoever
		// moved on or off this site in the window.
		var candidates []string
		if e := tx.Raw(`SELECT DISTINCT e FROM (
           SELECT external_id AS e FROM core.presence_state
           UNION SELECT lower(external_id) FROM core.devices WHERE roaming AND removed_at IS NULL
           UNION SELECT external_id FROM core.presence_history WHERE at > ? AND at <= ? AND (gateway_id = ANY(?::uuid[]) OR from_gateway_id = ANY(?::uuid[]))) c
         ORDER BY e LIMIT ?`, start, to, gws, gws, domain.TwinMaxPeople).Scan(&candidates).Error; e != nil {
			return out, e
		}
		key := map[string]*string{}
		if len(candidates) > 0 {
			var rows []struct {
				External  string
				GatewayID *string
			}
			if e := tx.Raw(`SELECT c.e AS external, k.gateway_id::text AS gateway_id FROM unnest(?::text[]) AS c(e)
           CROSS JOIN LATERAL (SELECT h.gateway_id FROM core.presence_history h WHERE h.tenant_id = core.tenant_id() AND h.external_id = c.e AND h.at <= ?
             ORDER BY h.at DESC LIMIT 1) k`, pgTextArray(candidates), start).Scan(&rows).Error; e != nil {
				return out, e
			}
			for _, row := range rows {
				key[row.External] = row.GatewayID
			}
		}
		onSite := func(g *string) bool { return g != nil && site.gateways[*g] }
		relevant := map[string]bool{}
		for ext, g := range key {
			if onSite(g) {
				relevant[ext] = true
			}
		}
		prev := map[string]*string{}
		for ext, g := range key {
			prev[ext] = g
		}
		for _, m := range moves {
			if onSite(m.GatewayID) || onSite(prev[m.External]) {
				relevant[m.External] = true
			}
			prev[m.External] = m.GatewayID
		}
		for ext := range relevant {
			peopleExts = append(peopleExts, ext)
		}
		sort.Strings(peopleExts)
		people, e := renderReplayPeople(tx, site, mode, peopleExts, key, moves, relevant, from, to, step)
		if e != nil {
			return out, e
		}
		out.People = people
	}

	if layer("alerts") {
		alerts, e := siteAlerts(tx, site, peopleExts, from, to, domain.TwinReplayMaxAlerts+1)
		if e != nil {
			return out, e
		}
		if len(alerts) > domain.TwinReplayMaxAlerts {
			return out, domain.ErrTooDense{HintTo: alerts[domain.TwinReplayMaxAlerts-1].OpenedAt}
		}
		mentioned := []string{}
		for _, a := range alerts {
			if a.External != "" {
				mentioned = append(mentioned, a.External)
			}
		}
		personal, e := personalSet(tx, mentioned)
		if e != nil {
			return out, e
		}
		res := &domain.TwinReplayAlerts{Key: []string{}, Items: []domain.TwinReplayAlert{}}
		for _, a := range alerts {
			item := domain.TwinReplayAlert{ID: a.ID, Severity: a.Severity, Event: a.EventType, SOS: a.EventType == domain.EventButton, Hazard: a.EventType == domain.EventHazard,
				GatewayID: a.GatewayID, Title: a.Title, OpenedAt: a.OpenedAt, AckedAt: a.AckedAt, ResolvedAt: a.ResolvedAt}
			if mode != domain.TwinPeopleNamed && personal(a.External) {
				item.Title = domain.TwinEventTitle(a.EventType)
			}
			if ids := site.devices[a.External]; len(ids) > 0 {
				item.DeviceID = ids[0]
			}
			if !a.OpenedAt.After(from) && (a.ResolvedAt == nil || a.ResolvedAt.After(from)) {
				res.Key = append(res.Key, a.ID)
			}
			res.Items = append(res.Items, item)
		}
		out.Alerts = res
	}
	return out, nil
}

// renderReplayPeople applies the people mode. counts sends headcounts only: per gateway, the net change of each
// bucket of the response, stamped at the bucket's end (a count at T is the one at the end of the last closed
// bucket), so no identity and no single move's time leaves the server; zones that net to zero in a bucket send
// nothing. A zone with one person in it is shown as one: counts are for reviewing incidents (a lone worker is the
// case that matters), and no identity leaves with it. tracks draws one pseudonym per person for this response only
// (and cuts times to the minute, like the live view); named uses the tag id and the registration's name.
func renderReplayPeople(tx *gorm.DB, site twinSiteRows, mode string, exts []string, key map[string]*string, moves []twinPersonMove, relevant map[string]bool, from, to time.Time, step time.Duration) (*domain.TwinReplayPeople, error) {
	out := &domain.TwinReplayPeople{Mode: mode}
	here := func(g *string) *string {
		if g != nil && site.gateways[*g] {
			return g
		}
		return nil
	}
	if mode == domain.TwinPeopleCounts {
		out.KeyCounts, out.CountDeltas = map[string]int{}, [][3]any{}
		cur := map[string]*string{}
		for _, ext := range exts {
			if g := here(key[ext]); g != nil {
				out.KeyCounts[*g]++
				cur[ext] = g
			}
		}
		last := int(to.Sub(from)/step) - 1
		net := map[int]map[string]int{}
		for _, m := range moves {
			if !relevant[m.External] {
				continue
			}
			was, now := cur[m.External], here(m.GatewayID)
			cur[m.External] = now
			if was != nil && now != nil && *was == *now {
				continue
			}
			// The bucket (lo, hi] the move falls in: it shows from that bucket's end, never before it happened.
			i := min(max(int((m.At.Sub(from)-1)/step), 0), last)
			if net[i] == nil {
				net[i] = map[string]int{}
			}
			if was != nil {
				net[i][*was]--
			}
			if now != nil {
				net[i][*now]++
			}
		}
		buckets := make([]int, 0, len(net))
		for i := range net {
			buckets = append(buckets, i)
		}
		sort.Ints(buckets)
		for _, i := range buckets {
			gws := make([]string, 0, len(net[i]))
			for g, d := range net[i] {
				if d != 0 {
					gws = append(gws, g)
				}
			}
			sort.Strings(gws)
			at := from.Add(time.Duration(i+1) * step).UnixMilli()
			for _, g := range gws {
				out.CountDeltas = append(out.CountDeltas, [3]any{at, g, net[i][g]})
			}
		}
		return out, nil
	}
	pid := func(ext string) string { return ext }
	if mode == domain.TwinPeopleTracks {
		salt := make([]byte, 16)
		_, _ = rand.Read(salt)
		pid = func(ext string) string {
			m := hmac.New(sha256.New, salt)
			m.Write([]byte(ext))
			return "p" + hex.EncodeToString(m.Sum(nil)[:6])
		}
	}
	out.Key, out.Deltas = map[string]*string{}, [][3]any{}
	for _, ext := range exts {
		out.Key[pid(ext)] = here(key[ext])
	}
	for _, m := range moves {
		if !relevant[m.External] {
			continue
		}
		at := m.At
		if mode == domain.TwinPeopleTracks {
			at = at.Truncate(time.Minute)
		}
		var g any
		if h := here(m.GatewayID); h != nil {
			g = *h
		}
		out.Deltas = append(out.Deltas, [3]any{at.UnixMilli(), pid(m.External), g})
	}
	if mode == domain.TwinPeopleNamed {
		out.Names = map[string]string{}
		if len(exts) > 0 {
			var rows []struct{ External, Name string }
			if e := tx.Raw(`SELECT DISTINCT ON (lower(external_id)) lower(external_id) AS external, name FROM core.devices
           WHERE lower(external_id) = ANY(?::text[]) ORDER BY lower(external_id), removed_at IS NOT NULL, created_at DESC`, pgTextArray(exts)).Scan(&rows).Error; e != nil {
				return nil, e
			}
			for _, row := range rows {
				out.Names[row.External] = row.Name
			}
		}
		for _, ext := range exts {
			if _, ok := out.Names[ext]; !ok {
				out.Names[ext] = TwinPersonalName
			}
		}
	}
	return out, nil
}

func round1(v float64) float64 {
	if v < 0 {
		return -float64(int64(-v*10+0.5)) / 10
	}
	return float64(int64(v*10+0.5)) / 10
}

// TwinTrail is one named person's zone changes in a window (at most 5000): only in named mode (the caller must be
// allowed names by the settings and their role), never for a display. Always in the access log. A zone outside the
// caller's projects is null.
func (r *Repository) TwinTrail(ctx context.Context, p domain.Principal, external string, from, to time.Time) ([]domain.TwinTrailPoint, error) {
	out := []domain.TwinTrailPoint{}
	if !to.After(from) || to.Sub(from) > domain.TwinReplayMaxWindow {
		return out, domain.ErrInvalid
	}
	if _, isDisplay := domain.DisplayFrom(ctx); isDisplay {
		return out, domain.ErrForbidden
	}
	e := r.twinBounded(ctx, p, from, to, func(tx *gorm.DB) error {
		settings, e := loadTwinSettings(tx)
		if e != nil {
			return e
		}
		if domain.TwinPeopleFor(p, domain.TwinPeopleNamed, settings, false, false) != domain.TwinPeopleNamed {
			return domain.ErrForbidden
		}
		since := time.Now().Add(-time.Duration(min(settings.PeopleReplayDays, max(settings.HistoryDays, 1))) * 24 * time.Hour)
		if from.Before(since) {
			from = since
		}
		var rows []struct {
			At        time.Time
			GatewayID *string
		}
		// A move is visible when either end is in the caller's scope; the zone it went to is named only when that
		// one is (a zone in another project reads as "elsewhere", null).
		if e := tx.Raw(`SELECT at, CASE WHEN (SELECT core.scope_all()) OR core.gateway_in_scope(gateway_id) THEN gateway_id::text END AS gateway_id
       FROM core.presence_history WHERE external_id=? AND at >= ? AND at <= ? ORDER BY at LIMIT 5000`, strings.ToLower(external), from, to).Scan(&rows).Error; e != nil {
			return e
		}
		for _, row := range rows {
			out = append(out, domain.TwinTrailPoint{At: row.At, GatewayID: row.GatewayID})
		}
		return nil
	})
	return out, e
}

// twinReplayCache keeps encoded replay windows that lie wholly in the past (they cannot change until an erasure, a
// layout, settings or scope change, all of which are in the key) for 10 minutes, up to 64 MB counting keys and
// bodies, least recently used out. A body over 8 MB is not kept, and expired entries are swept at most once a minute
// (a put), so an idle cache does not hold them to the end of the LRU.
type twinReplayCache struct {
	mu       sync.Mutex
	max      int
	maxEntry int
	size     int
	ttl      time.Duration
	swept    time.Time
	order    *list.List
	items    map[string]*list.Element
}

type twinReplayEntry struct {
	key  string
	body []byte
	at   time.Time
}

func (e *twinReplayEntry) cost() int { return len(e.key) + len(e.body) }

var twinReplays = &twinReplayCache{max: 64 << 20, maxEntry: 8 << 20, ttl: 10 * time.Minute, order: list.New(), items: map[string]*list.Element{}}

func (c *twinReplayCache) get(key string, now time.Time) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return nil, false
	}
	entry := el.Value.(*twinReplayEntry)
	if now.Sub(entry.at) > c.ttl {
		c.remove(el)
		return nil, false
	}
	c.order.MoveToFront(el)
	return entry.body, true
}

func (c *twinReplayCache) put(key string, body []byte, now time.Time) {
	if len(key)+len(body) > c.maxEntry {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if now.Sub(c.swept) > time.Minute {
		c.swept = now
		for el := c.order.Back(); el != nil; {
			prev := el.Prev()
			if now.Sub(el.Value.(*twinReplayEntry).at) > c.ttl {
				c.remove(el)
			}
			el = prev
		}
	}
	if el, ok := c.items[key]; ok {
		c.remove(el)
	}
	entry := &twinReplayEntry{key: key, body: body, at: now}
	c.items[key] = c.order.PushFront(entry)
	c.size += entry.cost()
	for c.size > c.max {
		c.remove(c.order.Back())
	}
}

func (c *twinReplayCache) remove(el *list.Element) {
	entry := el.Value.(*twinReplayEntry)
	c.order.Remove(el)
	delete(c.items, entry.key)
	c.size -= entry.cost()
}

// RollupSamples runs one step of core.rollup_samples (every active workspace in one pass, at most span of buckets,
// fixed sensors only) and reports the buckets written and how many seconds it is still behind the live edge. The
// step runs under a 60 s statement timeout, which the function requires.
func (r *Repository) RollupSamples(ctx context.Context, span time.Duration) (int64, int64, error) {
	days, _ := r.retention()
	var rows []struct{ Buckets, BehindSec int64 }
	e := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if e := tx.Exec(`SET LOCAL statement_timeout = '60s'`).Error; e != nil {
			return e
		}
		return tx.Raw(`SELECT buckets, behind_sec FROM core.rollup_samples(make_interval(secs => ?), ?, ?::text[])`, int(span.Seconds()), days, pgArray(domain.WornProfileIDs())).Scan(&rows).Error
	})
	if e != nil || len(rows) != 1 {
		return 0, 0, e
	}
	return rows[0].Buckets, rows[0].BehindSec, nil
}

// RollupRange recomputes the buckets of [lo, hi] for one workspace now (at most 7 days): the demo backfill writes
// history behind the worker's watermark.
func (r *Repository) RollupRange(ctx context.Context, tenant string, lo, hi time.Time) (int64, error) {
	var n int64
	e := r.tx(ctx, "", tenant, func(tx *gorm.DB) error {
		if e := tx.Exec(`SET LOCAL statement_timeout = '120s'`).Error; e != nil {
			return e
		}
		return tx.Raw(`SELECT core.rollup_samples_range(?, ?, ?::text[])`, lo, hi, pgArray(domain.WornProfileIDs())).Scan(&n).Error
	})
	return n, e
}

// RunSampleRollup keeps core.sample_rollup current: a step every interval, and further steps while it is still
// behind (catching up after a start or an outage; the first start backfills the sample retention), a short pause
// apart so a catch-up never holds the database's attention back to back. A step that runs into the statement
// timeout (a large deployment catching up) is retried with half the span, down to one bucket, and the span grows
// back after steps that succeed.
func (r *Repository) RunSampleRollup(ctx context.Context, interval time.Duration) {
	const most, least = 6 * time.Hour, 5 * time.Minute
	span := most
	for {
		for i := 0; i < 1000; i++ {
			_, behind, e := r.RollupSamples(ctx, span)
			var pg *pgconn.PgError
			if errors.As(e, &pg) && pg.Code == "57014" && span > least {
				span = max(span/2, least)
				slog.Warn("sample rollup step timed out; retrying with a shorter span", "span", span.String())
				continue
			}
			if e != nil {
				if !errors.Is(e, context.Canceled) {
					slog.Warn("sample rollup failed; retried next run", "error", e.Error())
				}
				break
			}
			span = min(span*2, most)
			if behind <= 0 || ctx.Err() != nil {
				break
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(250 * time.Millisecond):
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// ResolveDemoAlertsBefore resolves (and acknowledges) every alert of a demo workspace opened before `at`, at that
// time: the demo backfill closes what its script raised so the next loop rings again. Refused for a real workspace.
func (r *Repository) ResolveDemoAlertsBefore(ctx context.Context, tenant string, at time.Time) (int64, error) {
	var n int64
	e := r.tx(ctx, "", tenant, func(tx *gorm.DB) error {
		var demo []bool
		if e := tx.Raw(`SELECT demo FROM core.tenants WHERE id=core.tenant_id()`).Scan(&demo).Error; e != nil {
			return e
		}
		if len(demo) != 1 || !demo[0] {
			return domain.ErrForbidden
		}
		res := tx.Exec(`UPDATE core.alerts SET status='resolved', acked_at=coalesce(acked_at, ?), resolved_at=?
       WHERE status<>'resolved' AND opened_at < ?`, at, at, at)
		n = res.RowsAffected
		return res.Error
	})
	return n, e
}
