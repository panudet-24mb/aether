package tests

import (
	"aether/backend/internal/domain"
	"aether/backend/internal/studio"
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"testing"
)

func TestStudioCatalogIsolationAndDashboardRevision(t *testing.T) {
	f := setup(t)
	_, _, a := f.account(t)
	_, _, b := f.account(t)
	ctx := context.Background()
	makeItem := func(kind, visibility string) studio.Item {
		return studio.Item{ID: uuid.NewString(), TenantID: a.TenantID, Kind: kind, Name: uuid.NewString(), Brand: "Minew", Model: "S1", Version: 1, Revision: 1, Visibility: visibility, Definition: json.RawMessage(`{"html":"<h1>Test</h1>"}`)}
	}
	private := makeItem("widget", "private")
	public := makeItem("widget", "community")
	for _, i := range []studio.Item{private, public} {
		if e := f.repo.StudioCreate(ctx, a, i); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := f.repo.StudioGet(ctx, b, private.ID); !errors.Is(e, domain.ErrNotFound) {
		t.Fatalf("private leaked: %v", e)
	}
	if got, e := f.repo.StudioGet(ctx, b, public.ID); e != nil || got.Name != public.Name || !json.Valid(got.Definition) {
		t.Fatalf("community inaccessible: %+v %v", got, e)
	}
	dash := makeItem("dashboard", "private")
	dash.Definition = json.RawMessage(`{"panels":[]}`)
	if e := f.repo.StudioCreate(ctx, a, dash); e != nil {
		t.Fatal(e)
	}
	if e := f.repo.StudioSave(ctx, b, dash); !errors.Is(e, domain.ErrConflict) {
		t.Fatalf("foreign save: %v", e)
	}
	dash.Name = "Updated dashboard"
	if e := f.repo.StudioSave(ctx, a, dash); e != nil {
		t.Fatal(e)
	}
	if e := f.repo.StudioSave(ctx, a, dash); !errors.Is(e, domain.ErrConflict) {
		t.Fatalf("stale save accepted: %v", e)
	}
	if e := f.repo.StudioSave(ctx, a, public); !errors.Is(e, domain.ErrConflict) {
		t.Fatalf("immutable widget updated: %v", e)
	}
	if e := f.repo.StudioDelete(ctx, b, dash.ID); !errors.Is(e, domain.ErrNotFound) {
		t.Fatalf("foreign delete: %v", e)
	}
}
