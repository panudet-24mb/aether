package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"aether/backend/internal/app"
	"aether/backend/internal/config"
	"aether/backend/internal/domain"
	"github.com/gofiber/fiber/v3"
)

type fakeAccessRepo struct {
	mu   sync.Mutex
	rows []domain.AccessRead
	fail bool
}

func (f *fakeAccessRepo) LogAccess(_ context.Context, _ domain.Principal, in domain.AccessRead) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("insert refused")
	}
	f.rows = append(f.rows, in)
	return nil
}

// readApp mounts two logged routes and one that is not, behind the read-log middleware, the way server.go
// does: a group with the middleware, routes registered on it afterwards.
func readApp(repo *fakeAccessRepo, clock *time.Time) *fiber.App {
	api := fiber.New(fiber.Config{ErrorHandler: func(c fiber.Ctx, e error) error {
		code := 500
		if errors.Is(e, domain.ErrUnavailable) {
			code = 503
		}
		return c.Status(code).JSON(fiber.Map{"error": "unavailable"})
	}})
	l := &readLog{repo: repo, seen: map[string]*readMark{}, now: func() time.Time { return *clock }}
	g := api.Group("/api/v1", func(c fiber.Ctx) error {
		c.Locals("principal", domain.Principal{UserID: "u1", TenantID: "t1", Role: "owner"})
		return c.Next()
	}, l.middleware)
	g.Get("/members/:user_id/access", func(c fiber.Ctx) error { return c.JSON(fiber.Map{"secret": "personal"}) })
	g.Post("/members/:user_id/export", func(c fiber.Ctx) error {
		c.Set(fiber.HeaderContentDisposition, `attachment; filename="x.json"`)
		return c.JSON(fiber.Map{"secret": "personal"})
	})
	g.Get("/gateways", func(c fiber.Ctx) error { return c.JSON(fiber.Map{"items": []string{}}) })
	g.Get("/members", func(c fiber.Ctx) error { return c.Status(403).JSON(fiber.Map{"error": "forbidden"}) })
	return api
}

func call(t *testing.T, api *fiber.App, method, path string) (int, string, string) {
	t.Helper()
	res, e := api.Test(httptest.NewRequest(method, path, nil))
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(raw), res.Header.Get(fiber.HeaderContentDisposition)
}

func TestReadLogMatchesRoutePatternsAndDedupes(t *testing.T) {
	repo := &fakeAccessRepo{}
	clock := time.Unix(1_800_000_000, 0)
	api := readApp(repo, &clock)
	if code, _, _ := call(t, api, "GET", "/api/v1/members/ABC/access"); code != 200 {
		t.Fatal(code)
	}
	if len(repo.rows) != 1 || repo.rows[0].Resource != "member_access" || repo.rows[0].SubjectKind != "member" || repo.rows[0].SubjectID != "abc" {
		t.Fatalf("logged %+v", repo.rows)
	}
	// Same actor, same subject within the window: one row. Another subject: a new row.
	call(t, api, "GET", "/api/v1/members/ABC/access")
	call(t, api, "GET", "/api/v1/members/other/access")
	if len(repo.rows) != 2 {
		t.Fatalf("dedupe: %d rows", len(repo.rows))
	}
	clock = clock.Add(readDedupe)
	call(t, api, "GET", "/api/v1/members/ABC/access")
	if len(repo.rows) != 3 {
		t.Fatalf("after the window: %d rows", len(repo.rows))
	}
	// Exports are logged every time.
	call(t, api, "POST", "/api/v1/members/abc/export")
	call(t, api, "POST", "/api/v1/members/abc/export")
	if len(repo.rows) != 5 || repo.rows[4].Resource != "export" {
		t.Fatalf("exports: %+v", repo.rows)
	}
	// Routes that are not personal data, and refused requests, are not logged.
	call(t, api, "GET", "/api/v1/gateways")
	call(t, api, "GET", "/api/v1/members")
	if len(repo.rows) != 5 {
		t.Fatalf("logged a non-personal or refused read: %+v", repo.rows)
	}
}

func TestReadLogFailsClosed(t *testing.T) {
	repo := &fakeAccessRepo{fail: true}
	clock := time.Unix(1_800_000_000, 0)
	api := readApp(repo, &clock)
	code, body, disposition := call(t, api, "GET", "/api/v1/members/abc/access")
	if code != 503 || strings.Contains(body, "personal") {
		t.Fatalf("data left without a log row: %d %s", code, body)
	}
	code, body, disposition = call(t, api, "POST", "/api/v1/members/abc/export")
	if code != 503 || strings.Contains(body, "personal") || disposition != "" {
		t.Fatalf("export left without a log row: %d %s %q", code, body, disposition)
	}
	// A failure is not remembered: once the log works again the next read is logged.
	repo.fail = false
	if code, _, _ := call(t, api, "GET", "/api/v1/members/abc/access"); code != 200 || len(repo.rows) != 1 {
		t.Fatalf("after recovery: %d %d", code, len(repo.rows))
	}
}

func TestReadRulesNameRealRoutes(t *testing.T) {
	// Every rule must name a route the real API registers, or a renamed route would silently stop being logged.
	api := New(config.Config{Origin: "http://localhost"}, &app.Service{}, nil)
	registered := map[string]bool{}
	for _, route := range api.GetRoutes(true) {
		registered[route.Method+" "+route.Path] = true
	}
	for key, rule := range readRules {
		if !registered[key] {
			t.Fatalf("rule %q names no registered route", key)
		}
		method, path, ok := strings.Cut(key, " ")
		if !ok || (method != "GET" && method != "POST") || !strings.HasPrefix(path, "/api/v1/") || rule.subject == nil || rule.resource == "" || rule.kind == "" {
			t.Fatalf("bad rule %q", key)
		}
	}
}

// Concurrent reads of the same subject are one row, and none of them leaves before that row exists.
func TestReadLogConcurrentReadsShareOneRow(t *testing.T) {
	repo := &fakeAccessRepo{}
	clock := time.Unix(1_800_000_000, 0)
	api := readApp(repo, &clock)
	var wg sync.WaitGroup
	codes := make(chan int, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, e := api.Test(httptest.NewRequest("GET", "/api/v1/members/abc/access", nil))
			if e == nil {
				codes <- res.StatusCode
				res.Body.Close()
			}
		}()
	}
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != 200 {
			t.Fatalf("status %d", code)
		}
	}
	if len(repo.rows) != 1 {
		t.Fatalf("%d rows for 20 concurrent reads", len(repo.rows))
	}
}

func TestExportSlotsPerTenantAndTotal(t *testing.T) {
	slots := newExportSlots()
	a1, ok1 := slots.take("a")
	_, ok2 := slots.take("a")
	if _, ok3 := slots.take("a"); !ok1 || !ok2 || ok3 {
		t.Fatalf("per workspace: %v %v %v", ok1, ok2, ok3)
	}
	// Another workspace still gets its own slots, up to the total.
	_, okB1 := slots.take("b")
	_, okB2 := slots.take("b")
	if _, okC := slots.take("c"); !okB1 || !okB2 || okC {
		t.Fatalf("total: %v %v %v", okB1, okB2, okC)
	}
	a1()
	a1() // a second release of the same slot is a no-op
	if _, ok := slots.take("c"); !ok {
		t.Fatal("a released slot was not reusable")
	}
	if _, ok := slots.take("a"); ok {
		t.Fatal("a double release freed two slots")
	}
}

// Request ids are the server's: one the client sends is not echoed (nor recorded in the access log).
func TestClientRequestIDIsIgnored(t *testing.T) {
	api := New(config.Config{Origin: "http://localhost"}, &app.Service{}, nil)
	r := httptest.NewRequest("GET", "/health/live", nil)
	r.Header.Set("X-Request-ID", "chosen-by-client")
	res, e := api.Test(r)
	if e != nil {
		t.Fatal(e)
	}
	if got := res.Header.Get("X-Request-ID"); got == "" || got == "chosen-by-client" {
		t.Fatalf("request id %q", got)
	}
}
