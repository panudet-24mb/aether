package httpapi

import (
	"aether/backend/internal/app"
	"aether/backend/internal/domain"
	"aether/backend/internal/security"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v3"
)

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// etagMatches reads If-None-Match: a list of entity tags (weak ones compare equal too) or "*".
func etagMatches(header, etag string) bool {
	for _, tag := range strings.Split(header, ",") {
		tag = strings.TrimPrefix(strings.TrimSpace(tag), "W/")
		if tag == "*" || tag == etag {
			return true
		}
	}
	return false
}

// Downloads of model files stream from the database in slices; these caps bound how many run at once (each holds
// one slice in memory and runs a short transaction per slice). A request over a cap gets 503 with Retry-After.
const (
	modelSlice            = 1 << 20
	modelDownloads        = 16
	modelDownloadsPerTeam = 4
	// A download that averages less than modelMinRate after modelGrace is dropped, so slow clients cannot hold the
	// slots for the whole 10 minutes.
	modelMinRate = 32 << 10
	modelGrace   = 30 * time.Second
)

// downloadGate counts the model downloads in flight, in total and per workspace.
type downloadGate struct {
	mu        sync.Mutex
	total     int
	perTenant map[string]int
}

// acquire takes a slot for tenant; the release it returns is safe to call more than once.
func (g *downloadGate) acquire(tenant string) (func(), bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.total >= modelDownloads || g.perTenant[tenant] >= modelDownloadsPerTeam {
		return nil, false
	}
	g.total++
	g.perTenant[tenant]++
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.total--
			if g.perTenant[tenant]--; g.perTenant[tenant] <= 0 {
				delete(g.perTenant, tenant)
			}
		})
	}, true
}

// modelStream reads the file one slice at a time as the client takes it, and moves the connection's write deadline
// forward while the client keeps reading: fasthttp arms the server's WriteTimeout (15 s) once for the whole response,
// which a 32 MiB model on a slow link would not meet. A client that stalls for that long is still cut off, and a
// download is capped at 10 minutes and dropped when it averages under modelMinRate after modelGrace. fasthttp closes
// the stream when the response is done or abandoned, which frees the download slot.
type modelStream struct {
	fetch                func(offset, n int) ([]byte, error)
	size, offset, served int
	buf                  []byte
	conn                 net.Conn
	started, limit       time.Time
	pushed               time.Time
	now                  func() time.Time
	release              func()
}

var errTooSlow = errors.New("model download below the minimum rate")

func (m *modelStream) Read(p []byte) (int, error) {
	now := time.Now()
	if m.now != nil {
		now = m.now()
	}
	if elapsed := now.Sub(m.started); elapsed > modelGrace && float64(m.served) < modelMinRate*elapsed.Seconds() {
		m.release()
		return 0, errTooSlow
	}
	if m.conn != nil && now.Sub(m.pushed) >= time.Second {
		m.pushed = now
		deadline := now.Add(15 * time.Second)
		if deadline.After(m.limit) {
			deadline = m.limit
		}
		_ = m.conn.SetWriteDeadline(deadline)
	}
	if len(m.buf) == 0 {
		if m.offset >= m.size {
			m.release()
			return 0, io.EOF
		}
		b, e := m.fetch(m.offset, min(modelSlice, m.size-m.offset))
		if e != nil || len(b) == 0 {
			// The model changed or went away mid-download: the response ends short of its Content-Length.
			m.release()
			return 0, io.ErrUnexpectedEOF
		}
		m.buf, m.offset = b, m.offset+len(b)
	}
	n := copy(p, m.buf)
	m.buf = m.buf[n:]
	m.served += n
	return n, nil
}

func (m *modelStream) Close() error {
	m.release()
	return nil
}

func siteModelRoutes(r fiber.Router, s *app.Service) {
	principal := func(c fiber.Ctx) domain.Principal { return c.Locals("principal").(domain.Principal) }
	manage := func(c fiber.Ctx) (domain.Principal, error) {
		p := principal(c)
		if !p.CanManageDevices() {
			return p, domain.ErrForbidden
		}
		return p, nil
	}
	respond := func(c fiber.Ctx, m *domain.SiteModel, e error) error {
		if e != nil {
			return e
		}
		return c.JSON(fiber.Map{"model": m})
	}
	r.Get("/sites/:id/model", func(c fiber.Ctx) error {
		if !security.ValidID(c.Params("id")) {
			return domain.ErrInvalid
		}
		m, e := s.Repo.SiteModel(c.Context(), principal(c), c.Params("id"))
		return respond(c, m, e)
	})
	// The client asks for ?rev=N, so a cached file is never stale: a rev that is not the current one is 404, never
	// another revision's bytes under its URL. The ETag (the sha256) covers revalidation.
	gate := &downloadGate{perTenant: map[string]int{}}
	r.Get("/sites/:id/model/file", func(c fiber.Ctx) error {
		site := c.Params("id")
		if !security.ValidID(site) {
			return domain.ErrInvalid
		}
		rev := 0
		if raw := c.Query("rev"); raw != "" {
			n, e := strconv.Atoi(raw)
			if e != nil || n < 1 || raw != strconv.Itoa(n) {
				return domain.ErrInvalid
			}
			rev = n
		}
		p := principal(c)
		m, e := s.Repo.SiteModel(c.Context(), p, site)
		if e != nil {
			return e
		}
		if m == nil || (rev != 0 && rev != m.Revision) {
			return domain.ErrNotFound
		}
		etag := `"` + m.SHA256 + `"`
		c.Set(fiber.HeaderETag, etag)
		// Only a URL that names the revision may be cached for a day; without one the client revalidates (ETag).
		if rev != 0 {
			c.Set(fiber.HeaderCacheControl, "private, max-age=86400")
		} else {
			c.Set(fiber.HeaderCacheControl, "private, no-cache")
		}
		c.Set("X-Content-Type-Options", "nosniff")
		if match := c.Get(fiber.HeaderIfNoneMatch); match != "" && etagMatches(match, etag) {
			return c.SendStatus(304)
		}
		c.Set(fiber.HeaderContentType, "model/gltf-binary")
		if c.Method() == fiber.MethodHead {
			c.Response().Header.SetContentLength(m.Size)
			return nil
		}
		release, ok := gate.acquire(p.TenantID)
		if !ok {
			c.Set(fiber.HeaderRetryAfter, "5")
			return domain.ErrUnavailable
		}
		revision := m.Revision
		// The body is written after the handler returns: every slice gets its own deadline on a context that keeps the
		// request's values (a display session, domain.WithDisplay) but not its end.
		base := context.WithoutCancel(c.Context())
		fetch := func(offset, n int) ([]byte, error) {
			ctx, cancel := context.WithTimeout(base, 15*time.Second)
			defer cancel()
			return s.Repo.SiteModelSlice(ctx, p, site, revision, offset, n)
		}
		now := time.Now()
		return c.SendStream(&modelStream{fetch: fetch, size: m.Size, conn: c.RequestCtx().Conn(), started: now, limit: now.Add(10 * time.Minute), release: release}, m.Size)
	})
	r.Post("/sites/:id/model/uploads", func(c fiber.Ctx) error {
		p, e := manage(c)
		if e != nil {
			return e
		}
		var in struct {
			Name         string `json:"name"`
			SourceFormat string `json:"source_format"`
			Size         int    `json:"size"`
			SHA256       string `json:"sha256"`
		}
		if e := body(c, &in, 4096); e != nil || !security.ValidID(c.Params("id")) {
			return domain.ErrInvalid
		}
		name, ok := domain.CleanModelName(in.Name)
		in.SHA256 = strings.ToLower(in.SHA256)
		if !ok || !slices.Contains(domain.SiteModelFormats, in.SourceFormat) || in.Size < 20 || in.Size > domain.SiteModelMaxSize || !sha256Hex.MatchString(in.SHA256) {
			return domain.ErrInvalid
		}
		out, e := s.Repo.StartSiteModelUpload(c.Context(), p, domain.SiteModelUpload{SiteID: c.Params("id"), Name: name, SourceFormat: in.SourceFormat, Size: in.Size, SHA256: in.SHA256})
		var busy *domain.SiteModelUploadInProgress
		if errors.As(e, &busy) {
			// Names the upload so the page can offer to cancel it (POST …/uploads/:upload/cancel).
			return c.Status(409).JSON(fiber.Map{"error": "upload_in_progress", "upload_id": busy.UploadID, "started_at": busy.StartedAt, "request_id": c.GetRespHeader("X-Request-ID")})
		}
		if e != nil {
			return e
		}
		return c.Status(201).JSON(out)
	})
	r.Post("/sites/:id/model/uploads/:upload/chunks/:index", func(c fiber.Ctx) error {
		p, e := manage(c)
		if e != nil {
			return e
		}
		raw := c.Params("index")
		idx, e := strconv.Atoi(raw)
		if e != nil || raw != strconv.Itoa(idx) || idx < 0 || idx >= domain.SiteModelChunks(domain.SiteModelMaxSize) || !security.ValidID(c.Params("id")) || !security.ValidID(c.Params("upload")) {
			return domain.ErrInvalid
		}
		var in struct {
			Data string `json:"data_base64"`
		}
		if e := body(c, &in, 256*1024); e != nil {
			return e
		}
		data, e := base64.StdEncoding.DecodeString(in.Data)
		if e != nil || len(data) == 0 || len(data) > domain.SiteModelChunkSize {
			return domain.ErrInvalid
		}
		if e := s.Repo.PutSiteModelChunk(c.Context(), p, c.Params("id"), c.Params("upload"), idx, data); e != nil {
			return e
		}
		return c.SendStatus(204)
	})
	r.Post("/sites/:id/model/uploads/:upload/commit", func(c fiber.Ctx) error {
		p, e := manage(c)
		if e != nil {
			return e
		}
		if !security.ValidID(c.Params("id")) || !security.ValidID(c.Params("upload")) {
			return domain.ErrInvalid
		}
		m, e := s.Repo.CommitSiteModel(c.Context(), p, c.Params("id"), c.Params("upload"))
		if errors.Is(e, domain.ErrUnavailable) {
			c.Set(fiber.HeaderRetryAfter, "10")
		}
		return respond(c, m, e)
	})
	// Any manager may cancel an unfinished upload (for example one a colleague abandoned); the model is untouched.
	r.Post("/sites/:id/model/uploads/:upload/cancel", func(c fiber.Ctx) error {
		p, e := manage(c)
		if e != nil {
			return e
		}
		if !security.ValidID(c.Params("id")) || !security.ValidID(c.Params("upload")) {
			return domain.ErrInvalid
		}
		if e := s.Repo.CancelSiteModelUpload(c.Context(), p, c.Params("id"), c.Params("upload")); e != nil {
			return e
		}
		return c.SendStatus(204)
	})
	r.Post("/sites/:id/model/transform", func(c fiber.Ctx) error {
		p, e := manage(c)
		if e != nil {
			return e
		}
		// Fields left out keep their default; unknown fields, a wrong type or a value out of range are refused.
		in := domain.DefaultModelTransform()
		if e := body(c, &in, 4096); e != nil || !security.ValidID(c.Params("id")) {
			return domain.ErrInvalid
		}
		if e := in.Validate(); e != nil {
			return e
		}
		m, e := s.Repo.SetSiteModelTransform(c.Context(), p, c.Params("id"), in)
		return respond(c, m, e)
	})
	r.Post("/sites/:id/model/delete", func(c fiber.Ctx) error {
		p, e := manage(c)
		if e != nil {
			return e
		}
		if !security.ValidID(c.Params("id")) {
			return domain.ErrInvalid
		}
		if e := s.Repo.DeleteSiteModel(c.Context(), p, c.Params("id")); e != nil {
			return e
		}
		return c.SendStatus(204)
	})
}
