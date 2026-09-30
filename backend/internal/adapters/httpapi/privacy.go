package httpapi

import (
	"archive/zip"
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"aether/backend/internal/app"
	"aether/backend/internal/domain"
	"aether/backend/internal/security"
	"github.com/gofiber/fiber/v3"
)

// Personal data: the read-access log, the owner's privacy views, exports, erasure and the privacy notice
// (docs/platform/privacy.md).

// readRule says what a successful request to one route read: the resource, and whose data (subject kind and id).
// byQuery narrows the subject when the request names one tag or device in its query (param -> subject kind).
// Always is for exports: every one is logged, never folded into the dedupe window. Handled routes write their own
// row before they stream (the identity ZIP): the middleware leaves them alone.
type readRule struct {
	resource, kind string
	subject        func(c fiber.Ctx, p domain.Principal) string
	byQuery        [2]string
	always         bool
	handled        bool
}

func everything(fiber.Ctx, domain.Principal) string { return "*" }
func param(name string) func(fiber.Ctx, domain.Principal) string {
	return func(c fiber.Ctx, _ domain.Principal) string { return strings.ToLower(c.Params(name)) }
}

// readRules lists every route that returns personal data, by method and route pattern. Personal data here is who
// the members are and what they did, and anything that names or follows a worn tag (a person): device names,
// readings and raw packets, presence, events, alerts and their notifications, learned signals, flow runs and
// commands. A unit test checks each rule names a registered route; docs/platform/privacy.md lists them.
var readRules = map[string]readRule{
	// Members.
	"GET /api/v1/members":                 {resource: "members", kind: "member_list", subject: everything},
	"GET /api/v1/members/:user_id/access": {resource: "member_access", kind: "member", subject: param("user_id")},
	// Devices, their names, readings and raw packets.
	"GET /api/v1/devices":              {resource: "devices", kind: "device_list", subject: everything},
	"GET /api/v1/devices/:id/state":    {resource: "device_state", kind: "device", subject: param("id")},
	"GET /api/v1/devices/:id/controls": {resource: "device_controls", kind: "device", subject: param("id")},
	"GET /api/v1/gateways/:id/packets": {resource: "raw_packets", kind: "gateway", subject: param("id")},
	"GET /api/v1/discovery":            {resource: "discovery", kind: "device_list", subject: everything},
	"GET /api/v1/live":                 {resource: "live", kind: "sensor_history", subject: everything},
	"POST /api/v1/studio/render":       {resource: "studio_render", kind: "sensor_history", subject: everything},
	"GET /api/v1/studio/sources":       {resource: "studio_sources", kind: "device_list", subject: everything},
	"GET /api/v1/presence/:external":   {resource: "presence", kind: "device_identity", subject: param("external")},
	"GET /api/v1/assets":               {resource: "assets", kind: "device_list", subject: everything},
	"GET /api/v1/assets/:kind/:id":     {resource: "asset", kind: "asset", subject: param("id")},
	"GET /api/v1/assets/export.csv":    {resource: "assets_export", kind: "device_list", subject: everything},
	"GET /api/v1/sites/:id":            {resource: "site", kind: "site", subject: param("id")},
	"GET /api/v1/signals":              {resource: "signals", kind: "signal_list", subject: everything},
	"GET /api/v1/signals/sessions/:id": {resource: "signal_session", kind: "signal_session", subject: param("id")},
	// Events, alerts and what was sent about them.
	"GET /api/v1/events":         {resource: "events", kind: "event_list", subject: everything, byQuery: [2]string{"external_id", "device_identity"}},
	"GET /api/v1/alerts":         {resource: "alerts", kind: "alert_list", subject: everything},
	"GET /api/v1/alerts/summary": {resource: "alert_summary", kind: "alert_list", subject: everything},
	"GET /api/v1/notifications":  {resource: "notifications", kind: "alert_list", subject: everything},
	// What flows and people did to devices.
	"GET /api/v1/automations/:id/runs": {resource: "automation_runs", kind: "automation", subject: param("id")},
	"GET /api/v1/commands":             {resource: "commands", kind: "command_list", subject: everything, byQuery: [2]string{"device_id", "device"}},
	"GET /api/v1/commands/:id":         {resource: "command", kind: "command", subject: param("id")},
	// The privacy views and exports.
	"GET /api/v1/privacy/access-log":          {resource: "access_log", kind: "access_log", subject: everything},
	"GET /api/v1/privacy/audit":               {resource: "audit_log", kind: "audit_log", subject: everything},
	"GET /api/v1/privacy/erasures":            {resource: "erasure_log", kind: "erasure_log", subject: everything},
	"GET /api/v1/me/export":                   {resource: "export", kind: "member", subject: func(_ fiber.Ctx, p domain.Principal) string { return p.UserID }, always: true},
	"POST /api/v1/members/:user_id/export":    {resource: "export", kind: "member", subject: param("user_id"), always: true},
	"POST /api/v1/devices/:id/privacy-export": {resource: "export", kind: "device", subject: param("id"), always: true, handled: true},
}

// readDedupe is how long the same actor reading the same subject is one log row (screens poll).
const readDedupe = 10 * time.Minute

// readLog writes core.access_log for the routes in readRules, after the handler succeeded and before the
// response leaves. It fails closed: when the row cannot be written the data is withheld and the caller gets 503.
type readLog struct {
	repo interface {
		LogAccess(context.Context, domain.Principal, domain.AccessRead) error
	}
	mu   sync.Mutex
	seen map[string]*readMark
	now  func() time.Time
}

// readMark is one logged (or being logged) read. Requests that arrive while it is being written wait for the
// outcome: they are covered only once the row exists, and try themselves if it could not be written.
type readMark struct {
	at     time.Time
	done   chan struct{}
	failed bool
}

func newReadLog(s *app.Service) *readLog {
	return &readLog{repo: s.Repo, seen: map[string]*readMark{}, now: time.Now}
}

// forgetExpired drops marks older than the window once the map grows; the caller holds l.mu.
func (l *readLog) forgetExpired(now time.Time) {
	if len(l.seen) <= 50000 {
		return
	}
	for k, m := range l.seen {
		if now.Sub(m.at) >= readDedupe {
			delete(l.seen, k)
		}
	}
	if len(l.seen) > 50000 {
		l.seen = map[string]*readMark{}
	}
}

// insert writes the row with its own few seconds: the request's deadline may be nearly spent by a large export.
func (l *readLog) insert(ctx context.Context, p domain.Principal, in domain.AccessRead) error {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if e := l.repo.LogAccess(wctx, p, in); e != nil {
		slog.Error("access log write failed; response withheld", "resource", in.Resource, "request_id", in.RequestID, "error", e.Error())
		return domain.ErrUnavailable
	}
	return nil
}

// write logs one read, deduplicated per actor, resource and subject within readDedupe unless always, and answers
// an error when no row covers it.
func (l *readLog) write(ctx context.Context, p domain.Principal, in domain.AccessRead, always bool) error {
	if always {
		return l.insert(ctx, p, in)
	}
	key := strings.Join([]string{p.TenantID, p.UserID, in.Resource, in.SubjectKind, in.SubjectID}, "|")
	for attempt := 0; attempt < 2; attempt++ {
		l.mu.Lock()
		now := l.now()
		if m, ok := l.seen[key]; ok && now.Sub(m.at) < readDedupe {
			l.mu.Unlock()
			select {
			case <-m.done:
			case <-time.After(6 * time.Second):
				return domain.ErrUnavailable
			}
			if !m.failed {
				return nil
			}
			continue // that write failed: this request tries its own
		}
		l.forgetExpired(now)
		m := &readMark{at: now, done: make(chan struct{})}
		l.seen[key] = m
		l.mu.Unlock()
		e := l.insert(ctx, p, in)
		l.mu.Lock()
		if e != nil {
			m.failed = true
			if l.seen[key] == m {
				delete(l.seen, key)
			}
		}
		l.mu.Unlock()
		close(m.done)
		return e
	}
	return domain.ErrUnavailable
}

func (l *readLog) middleware(c fiber.Ctx) error {
	if e := c.Next(); e != nil {
		return e
	}
	if c.Response().StatusCode() >= 400 {
		return nil
	}
	rule, ok := readRules[c.Method()+" "+c.Route().Path]
	if !ok || rule.subject == nil || rule.handled {
		return nil
	}
	p, ok := c.Locals("principal").(domain.Principal)
	if !ok {
		return nil
	}
	kind, subject := rule.kind, rule.subject(c, p)
	if rule.byQuery[0] != "" {
		if v := strings.ToLower(strings.TrimSpace(c.Query(rule.byQuery[0]))); v != "" {
			kind, subject = rule.byQuery[1], v
		}
	}
	in := domain.AccessRead{Resource: rule.resource, SubjectKind: kind, SubjectID: subject, RequestID: requestID(c), ClientIP: c.IP()}
	if e := l.write(c.Context(), p, in, rule.always); e != nil {
		// The handler already produced the body; the error handler replaces it, and nothing names a download.
		c.Response().Header.Del(fiber.HeaderContentDisposition)
		c.Response().Header.SetContentType(fiber.MIMEApplicationJSON)
		return e
	}
	return nil
}

// requestID is the id the server generated for this request (server.go drops any X-Request-ID the client sent).
func requestID(c fiber.Ctx) string { return c.GetRespHeader(fiber.HeaderXRequestID) }

// noticeRoutes serves the privacy notice pointer before sign-in: the login screen links to it.
func noticeRoutes(api *fiber.App, noticeURL string) {
	api.Get("/api/v1/notice", func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{"version": domain.NoticeVersion, "url": noticeURL})
	})
}

// logFilter parses the privacy views' query: actor, resource, subject, action, from/to (RFC 3339), the cursor
// (before_at, before_id) and limit (1..500).
func logFilter(c fiber.Ctx) (domain.LogFilter, error) {
	f := domain.LogFilter{ActorID: c.Query("actor"), Resource: c.Query("resource"), SubjectKind: c.Query("subject_kind"), SubjectID: c.Query("subject_id"), Action: c.Query("action"), BeforeID: c.Query("before_id")}
	for _, id := range []string{f.ActorID, f.BeforeID} {
		if id != "" && !security.ValidID(id) {
			return f, domain.ErrInvalid
		}
	}
	for _, s := range []string{f.Resource, f.SubjectKind, f.SubjectID, f.Action} {
		if len(s) > 128 || strings.ContainsAny(s, "\x00\r\n") {
			return f, domain.ErrInvalid
		}
	}
	for name, dst := range map[string]**time.Time{"from": &f.From, "to": &f.To, "before_at": &f.BeforeAt} {
		if raw := c.Query(name); raw != "" {
			t, e := time.Parse(time.RFC3339Nano, raw)
			if e != nil {
				return f, domain.ErrInvalid
			}
			*dst = &t
		}
	}
	n, e := security.ParseLimit(c.Query("limit"), 100)
	if e != nil || n > 500 {
		return f, domain.ErrInvalid
	}
	f.Limit = n
	return f, nil
}

// attachment sends v as a JSON download.
func attachment(c fiber.Ctx, name string, v any) error {
	raw, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	c.Set(fiber.HeaderContentDisposition, `attachment; filename="`+name+`"`)
	c.Type("json")
	return c.Send(raw)
}

// exportContext gives an export more time than an ordinary request: a tag's history can be large.
func exportContext(c fiber.Ctx) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(c.Context()), 60*time.Second)
}

// Exports are bounded per API process: at most maxExportsPerTenant at once for one workspace, and maxExports in
// all, so one workspace cannot hold every slot. One more is refused with 429 rather than queued.
const (
	maxExports          = 4
	maxExportsPerTenant = 2
)

// exportSlots counts running exports per workspace and in total. take reports false when either limit is reached; a
// taken slot is given back with the release it returns (exactly once, also when a stream ends early).
type exportSlots struct {
	mu        sync.Mutex
	total     int
	perTenant map[string]int
}

func newExportSlots() *exportSlots { return &exportSlots{perTenant: map[string]int{}} }

func (s *exportSlots) take(tenant string) (func(), bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.total >= maxExports || s.perTenant[tenant] >= maxExportsPerTenant {
		return nil, false
	}
	s.total++
	s.perTenant[tenant]++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.total--
			if s.perTenant[tenant]--; s.perTenant[tenant] <= 0 {
				delete(s.perTenant, tenant)
			}
		})
	}, true
}

// streamWriteWindow is how long a streamed export may go without the client taking data. The server's WriteTimeout
// (15 s) is one absolute deadline for the whole response, which would cut a large export off however fast the client
// reads; the stream moves the connection's deadline forward as it makes progress instead.
const streamWriteWindow = 30 * time.Second

// refreshRows is how many rows a stream writes between moves of the write deadline.
const refreshRows = 1000

func privacyRoutes(r fiber.Router, s *app.Service, reads *readLog) {
	slots := newExportSlots()
	principal := func(c fiber.Ctx) domain.Principal { return c.Locals("principal").(domain.Principal) }
	owner := func(c fiber.Ctx) (domain.Principal, error) {
		p := principal(c)
		if p.Role != "owner" {
			return p, domain.ErrForbidden
		}
		return p, nil
	}
	page := func(c fiber.Ctx, items any, cursor func() (string, string)) error {
		out := fiber.Map{"items": items}
		if at, id := cursor(); id != "" {
			out["next"] = fiber.Map{"before_at": at, "before_id": id}
		}
		return c.JSON(out)
	}

	r.Get("/privacy/access-log", func(c fiber.Ctx) error {
		p, e := owner(c)
		if e != nil {
			return e
		}
		f, e := logFilter(c)
		if e != nil {
			return e
		}
		items, e := s.Repo.ListAccessLog(c.Context(), p, f)
		if e != nil {
			return e
		}
		return page(c, items, func() (string, string) {
			if len(items) < f.Limit || len(items) == 0 {
				return "", ""
			}
			last := items[len(items)-1]
			return last.At.UTC().Format(time.RFC3339Nano), last.ID
		})
	})
	r.Get("/privacy/audit", func(c fiber.Ctx) error {
		p, e := owner(c)
		if e != nil {
			return e
		}
		f, e := logFilter(c)
		if e != nil {
			return e
		}
		items, e := s.Repo.ListAuditLog(c.Context(), p, f)
		if e != nil {
			return e
		}
		return page(c, items, func() (string, string) {
			if len(items) < f.Limit || len(items) == 0 {
				return "", ""
			}
			last := items[len(items)-1]
			return last.At.UTC().Format(time.RFC3339Nano), last.ID
		})
	})
	r.Get("/privacy/erasures", func(c fiber.Ctx) error {
		p, e := owner(c)
		if e != nil {
			return e
		}
		items, e := s.Repo.ListErasures(c.Context(), p, 200)
		if e != nil {
			return e
		}
		return c.JSON(fiber.Map{"items": items})
	})

	// Everybody may export what the workspace holds about themselves.
	r.Get("/me/export", func(c fiber.Ctx) error {
		p := principal(c)
		release, ok := slots.take(p.TenantID)
		if !ok {
			return domain.ErrRateLimited
		}
		defer release()
		ctx, cancel := exportContext(c)
		defer cancel()
		out, e := s.Repo.ExportMember(ctx, p, p.UserID)
		if e != nil {
			return e
		}
		return attachment(c, "aether-my-data.json", out)
	})
	r.Post("/me/notice-ack", func(c fiber.Ctx) error {
		var in struct {
			Version int `json:"version"`
		}
		if e := body(c, &in, 256); e != nil {
			return e
		}
		if in.Version < 1 || in.Version > domain.NoticeVersion {
			return domain.ErrInvalid
		}
		v, e := s.Repo.AckNotice(c.Context(), principal(c), in.Version)
		if e != nil {
			return e
		}
		return c.JSON(fiber.Map{"notice_ack_version": v})
	})

	memberTarget := func(c fiber.Ctx) (domain.Principal, string, error) {
		if e := body(c, &struct{}{}, 256); e != nil {
			return domain.Principal{}, "", e
		}
		p, e := owner(c)
		if e != nil {
			return p, "", e
		}
		id := c.Params("user_id")
		if !security.ValidID(id) {
			return p, "", domain.ErrInvalid
		}
		return p, id, nil
	}
	r.Post("/members/:user_id/export", func(c fiber.Ctx) error {
		p, id, e := memberTarget(c)
		if e != nil {
			return e
		}
		release, ok := slots.take(p.TenantID)
		if !ok {
			return domain.ErrRateLimited
		}
		defer release()
		ctx, cancel := exportContext(c)
		defer cancel()
		out, e := s.Repo.ExportMember(ctx, p, id)
		if e != nil {
			return e
		}
		return attachment(c, "aether-member-"+id+".json", out)
	})
	r.Post("/members/:user_id/erase", func(c fiber.Ctx) error {
		p, id, e := memberTarget(c)
		if e != nil {
			return e
		}
		out, e := s.Repo.EraseMember(c.Context(), p, id)
		if e != nil {
			return e
		}
		return c.JSON(out)
	})

	// A tag's history streams as a ZIP (summary.json, samples.ndjson, ble_history.ndjson, manifest.json), so the
	// server never holds it whole. Everything that must hold before data leaves happens first: the owner check, the
	// audit row and the read-access log row (fail closed, 503). A failure while streaming cannot change the status any
	// more: the archive then lacks manifest.json, whose "complete": true marks a whole export.
	r.Post("/devices/:id/privacy-export", func(c fiber.Ctx) error {
		if e := body(c, &struct{}{}, 256); e != nil {
			return e
		}
		p, e := owner(c)
		if e != nil {
			return e
		}
		id := c.Params("id")
		if !security.ValidID(id) {
			return domain.ErrInvalid
		}
		release, ok := slots.take(p.TenantID)
		if !ok {
			return domain.ErrRateLimited
		}
		streaming := false
		defer func() {
			if !streaming {
				release()
			}
		}()
		summary, e := s.Repo.IdentityExportSummary(c.Context(), p, id)
		if e != nil {
			return e
		}
		read := domain.AccessRead{Resource: "export", SubjectKind: "device", SubjectID: strings.ToLower(id), RequestID: requestID(c), ClientIP: c.IP()}
		if e := reads.write(c.Context(), p, read, true); e != nil {
			return e
		}
		c.Set(fiber.HeaderContentDisposition, `attachment; filename="aether-device-`+id+`.zip"`)
		c.Type("zip")
		conn := c.RequestCtx().Conn()
		refresh := func() {
			if conn != nil {
				_ = conn.SetWriteDeadline(time.Now().Add(streamWriteWindow))
			}
		}
		streaming = true
		return c.SendStreamWriter(func(w *bufio.Writer) {
			defer release()
			// A panic here would take the process down: this runs on fasthttp's goroutine, outside the recover
			// middleware. The archive is simply left without its manifest.
			defer func() {
				if r := recover(); r != nil {
					slog.Error("device export stream panicked; archive left incomplete", "device", id, "panic", fmt.Sprint(r))
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			if e := streamIdentityZip(ctx, s, p, summary, w, refresh); e != nil {
				slog.Error("device export stream failed; archive left incomplete", "device", id, "error", e.Error())
			}
		})
	})
	r.Post("/devices/:id/erase-history", func(c fiber.Ctx) error {
		var in struct {
			// Name, when given, renames the registration in the same transaction: a tag handed to a new wearer.
			Name *string `json:"name"`
			// TenantWide erases the tag's history in every project of the workspace, not only the registration's.
			TenantWide bool `json:"tenant_wide"`
		}
		if e := body(c, &in, 1024); e != nil {
			return e
		}
		p, e := owner(c)
		if e != nil {
			return e
		}
		out, e := s.EraseIdentityHistory(c.Context(), p, c.Params("id"), in.Name, in.TenantWide)
		if e != nil {
			return e
		}
		return c.JSON(out)
	})
}

// streamIdentityZip writes a tag export: summary.json, then the sample and advertisement history as JSON lines
// straight from the database, then manifest.json saying whether the history was complete or capped.
// refresh moves the connection's write deadline forward: at every part and every refreshRows rows.
func streamIdentityZip(ctx context.Context, s *app.Service, p domain.Principal, summary domain.IdentityExport, w *bufio.Writer, refresh func()) error {
	refresh()
	z := zip.NewWriter(w)
	head, e := json.MarshalIndent(summary, "", "  ")
	if e != nil {
		return e
	}
	f, e := z.Create("summary.json")
	if e != nil {
		return e
	}
	if _, e := f.Write(head); e != nil {
		return e
	}
	truncated := map[string]bool{}
	for _, kind := range []string{"samples", "ble_history"} {
		refresh()
		f, e := z.Create(kind + ".ndjson")
		if e != nil {
			return e
		}
		rows := 0
		capped, e := s.Repo.StreamIdentityHistory(ctx, p, summary.ExternalID, kind, domain.IdentityExportRows, func(line []byte) error {
			if rows++; rows%refreshRows == 0 {
				refresh()
			}
			if _, e := f.Write(line); e != nil {
				return e
			}
			_, e := f.Write([]byte{'\n'})
			return e
		})
		if e != nil {
			return e
		}
		truncated[kind] = capped
	}
	refresh()
	manifest, _ := json.Marshal(map[string]any{"complete": true, "truncated": truncated, "row_cap": domain.IdentityExportRows})
	if f, e = z.Create("manifest.json"); e != nil {
		return e
	}
	if _, e := f.Write(manifest); e != nil {
		return e
	}
	if e := z.Close(); e != nil {
		return e
	}
	return w.Flush()
}
