package tests

import (
	"aether/backend/internal/domain"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
)

// glb builds a binary glTF v2 of at least size bytes: a JSON chunk and a BIN chunk of filler.
func glb(size int) []byte {
	js := []byte(`{"asset":{"version":"2.0"}}    `)[:28]
	bin := make([]byte, (size-20-len(js)-8+3)/4*4)
	for i := range bin {
		bin[i] = byte(i * 7)
	}
	total := 12 + 8 + len(js) + 8 + len(bin)
	b := make([]byte, 0, total)
	le := binary.LittleEndian
	b = append(b, "glTF"...)
	b = le.AppendUint32(b, 2)
	b = le.AppendUint32(b, uint32(total))
	b = le.AppendUint32(b, uint32(len(js)))
	b = le.AppendUint32(b, 0x4E4F534A)
	b = append(b, js...)
	b = le.AppendUint32(b, uint32(len(bin)))
	b = le.AppendUint32(b, 0x004E4942)
	return append(b, bin...)
}

func hexSum(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func rawGet(t *testing.T, api *fiber.App, path, token, ifNoneMatch string) (int, []byte, map[string]string) {
	t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	r.Header.Set("Authorization", token)
	if ifNoneMatch != "" {
		r.Header.Set("If-None-Match", ifNoneMatch)
	}
	res, e := api.Test(r, fiber.TestConfig{Timeout: 30 * time.Second})
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	h := map[string]string{}
	for _, k := range []string{"Content-Type", "ETag", "Cache-Control", "X-Content-Type-Options", "Content-Length"} {
		h[k] = res.Header.Get(k)
	}
	return res.StatusCode, b, h
}

func TestSiteModelUploadCommitAndServe(t *testing.T) {
	f := setup(t)
	_, authA, owner := f.account(t)
	_, authB, _ := f.account(t)
	api := busyAPI(f)
	A, B := "Bearer "+authA.AccessToken, "Bearer "+authB.AccessToken
	code, site, _ := req(t, api, "POST", "/api/v1/sites", A, "", "", map[string]any{"name": "Model building"})
	if code != 201 {
		t.Fatalf("create site: %d %v", code, site)
	}
	base := "/api/v1/sites/" + site["id"].(string) + "/model"

	code, out, _ := req(t, api, "GET", base, A, "", "", nil)
	if code != 200 || out["model"] != nil {
		t.Fatalf("a new site has no model: %d %v", code, out)
	}
	if code, _, _ := rawGet(t, api, base+"/file", A, ""); code != 404 {
		t.Fatalf("file of a site without a model: %d", code)
	}

	start := func(token string, file []byte, sha, format string) (int, map[string]any) {
		code, out, _ := req(t, api, "POST", base+"/uploads", token, "", "", map[string]any{"name": "C:\\cad\\Ward\u202E.glb", "source_format": format, "size": len(file), "sha256": sha})
		return code, out
	}
	chunk := func(token, upload string, idx int, data []byte) int {
		code, _, _ := req(t, api, "POST", base+"/uploads/"+upload+"/chunks/"+strconv.Itoa(idx), token, "", "", map[string]any{"data_base64": base64.StdEncoding.EncodeToString(data)})
		return code
	}
	send := func(token, upload string, file []byte) {
		t.Helper()
		for i := 0; i*domain.SiteModelChunkSize < len(file); i++ {
			end := min((i+1)*domain.SiteModelChunkSize, len(file))
			if code := chunk(token, upload, i, file[i*domain.SiteModelChunkSize:end]); code != 204 {
				t.Fatalf("chunk %d: %d", i, code)
			}
		}
	}
	commit := func(token, upload string) (int, map[string]any) {
		code, out, _ := req(t, api, "POST", base+"/uploads/"+upload+"/commit", token, "", "", nil)
		return code, out
	}

	// Happy path: three chunks, the last one short.
	file := glb(2*domain.SiteModelChunkSize + 1000)
	code, up := start(A, file, hexSum(file), "ifc")
	if code != 201 || up["chunk_size"] != float64(domain.SiteModelChunkSize) || up["chunks"] != float64(3) {
		t.Fatalf("start upload: %d %v", code, up)
	}
	upload := up["upload_id"].(string)
	if code := chunk(A, upload, 0, file[:1000]); code != 400 {
		t.Fatalf("a short chunk that is not the last was accepted: %d", code)
	}
	if code := chunk(A, upload, 2, file[2*domain.SiteModelChunkSize:]); code != 204 {
		t.Fatalf("last chunk: %d", code)
	}
	if code := chunk(A, upload, 2, file[2*domain.SiteModelChunkSize+1:]); code != 400 {
		t.Fatalf("a last chunk of the wrong length was accepted: %d", code)
	}
	if code := chunk(A, upload, 3, file[:10]); code != 400 {
		t.Fatalf("an index past the last chunk was accepted: %d", code)
	}
	if code, _ := commit(A, upload); code != 400 {
		t.Fatalf("commit with missing chunks: %d", code)
	}
	// Retrying a chunk replaces it.
	if code := chunk(A, upload, 0, make([]byte, domain.SiteModelChunkSize)); code != 204 {
		t.Fatalf("chunk 0: %d", code)
	}
	send(A, upload, file)
	if code := chunk(B, upload, 0, file[:domain.SiteModelChunkSize]); code != 404 {
		t.Fatalf("another tenant wrote a chunk: %d", code)
	}
	if code, _ := commit(B, upload); code != 404 {
		t.Fatalf("another tenant committed: %d", code)
	}
	code, out = commit(A, upload)
	model, _ := out["model"].(map[string]any)
	if code != 200 || model["revision"] != float64(1) || model["sha256"] != hexSum(file) || model["size"] != float64(len(file)) || model["source_format"] != "ifc" || model["name"] != "Ward.glb" {
		t.Fatalf("commit: %d %v", code, out)
	}
	if tr := model["transform"].(map[string]any); tr["scale"] != float64(1) || tr["up"] != "y" || tr["mode"] != "overlay" || tr["visible"] != true || tr["clip_to_floor"] != true {
		t.Fatalf("default transform: %v", tr)
	}
	if code, _ := commit(A, upload); code != 404 {
		t.Fatalf("an upload commits once: %d", code)
	}
	code, got, h := rawGet(t, api, base+"/file?rev=1", A, "")
	if code != 200 || !bytes.Equal(got, file) {
		t.Fatalf("file: %d, %d bytes, equal %v", code, len(got), bytes.Equal(got, file))
	}
	etag := `"` + hexSum(file) + `"`
	if h["Content-Type"] != "model/gltf-binary" || h["ETag"] != etag || h["Cache-Control"] != "private, max-age=86400" || h["X-Content-Type-Options"] != "nosniff" {
		t.Fatalf("file headers: %v", h)
	}
	// Without ?rev the URL names no revision: no day-long caching, revalidate with the ETag.
	if code, got, h := rawGet(t, api, base+"/file", A, ""); code != 200 || !bytes.Equal(got, file) || h["Cache-Control"] != "private, no-cache" || h["ETag"] != etag {
		t.Fatalf("file without rev: %d %v", code, h)
	}
	// Finished downloads give their slot back (4 per workspace run at once).
	for i := 0; i < 6; i++ {
		if code, got, _ := rawGet(t, api, base+"/file?rev=1", A, ""); code != 200 || len(got) != len(file) {
			t.Fatalf("download %d: %d", i, code)
		}
	}
	// ?rev must name the current revision: another revision's bytes are never cached under its URL.
	for path, want := range map[string]int{"/file?rev=2": 404, "/file?rev=0": 400, "/file?rev=x": 400, "/file?rev=01": 400} {
		if code, _, _ := rawGet(t, api, base+path, A, ""); code != want {
			t.Fatalf("%s: %d, want %d", path, code, want)
		}
	}
	if code, body, _ := rawGet(t, api, base+"/file?rev=1", A, `W/"x", `+etag); code != 304 || len(body) != 0 {
		t.Fatalf("If-None-Match: %d %d", code, len(body))
	}
	if code, _, _ := rawGet(t, api, base+"/file", B, ""); code != 404 {
		t.Fatalf("file leaked across tenants: %d", code)
	}
	if code, _, _ := req(t, api, "GET", base, B, "", "", nil); code != 404 {
		t.Fatalf("model leaked across tenants: %d", code)
	}
	if code, _ := start(B, file, hexSum(file), "glb"); code != 404 {
		t.Fatalf("another tenant started an upload: %d", code)
	}

	// A viewer reads the model but cannot change it.
	email := memberEmail()
	addMember(t, api, authA.AccessToken, email, "viewer", nil)
	viewer, _ := changeInitialPassword(t, f, api, email, owner.TenantID)
	V := "Bearer " + viewer.AccessToken
	if code, out, _ := req(t, api, "GET", base, V, "", "", nil); code != 200 || out["model"] == nil {
		t.Fatalf("viewer read: %d %v", code, out)
	}
	if code, _ := start(V, file, hexSum(file), "glb"); code != 403 {
		t.Fatalf("viewer started an upload: %d", code)
	}
	if code, _, _ := req(t, api, "POST", base+"/transform", V, "", "", map[string]any{"scale": 2}); code != 403 {
		t.Fatalf("viewer changed the transform: %d", code)
	}
	if code, _, _ := req(t, api, "POST", base+"/delete", V, "", "", nil); code != 403 {
		t.Fatalf("viewer deleted the model: %d", code)
	}

	// Transform: every field is checked.
	tr := map[string]any{"scale": 0.001, "up": "z", "rotation_deg": []float64{-90, 0, 45}, "offset_m": []float64{1.5, -2, 0}, "opacity": 0.5, "visible": false, "mode": "replace", "clip_to_floor": false}
	code, out, _ = req(t, api, "POST", base+"/transform", A, "", "", tr)
	if code != 200 || out["model"].(map[string]any)["transform"].(map[string]any)["mode"] != "replace" {
		t.Fatalf("transform: %d %v", code, out)
	}
	for _, bad := range []map[string]any{
		{"scale": 0},
		{"scale": 10001},
		{"up": "x"},
		{"rotation_deg": []float64{0, 0}},
		{"rotation_deg": []float64{0, 400, 0}},
		{"offset_m": []float64{0, 0, 1e6}},
		{"opacity": 0},
		{"mode": "hide"},
		{"visible": "yes"},
		{"colour": "red"},
	} {
		if code, _, _ := req(t, api, "POST", base+"/transform", A, "", "", bad); code != 400 {
			t.Fatalf("invalid transform %v accepted: %d", bad, code)
		}
	}

	// Rejected files: a sha256 that does not match, bytes that are not a GLB v2.
	code, up = start(A, file, hexSum([]byte("something else")), "glb")
	if code != 201 {
		t.Fatalf("start: %d", code)
	}
	send(A, up["upload_id"].(string), file)
	if code, _ := commit(A, up["upload_id"].(string)); code != 400 {
		t.Fatalf("sha256 mismatch accepted: %d", code)
	}
	notGLB := bytes.Repeat([]byte("solid cube\n"), 100)
	code, up = start(A, notGLB, hexSum(notGLB), "stl")
	if code != 201 {
		t.Fatalf("start: %d", code)
	}
	send(A, up["upload_id"].(string), notGLB)
	if code, _ := commit(A, up["upload_id"].(string)); code != 400 {
		t.Fatalf("non-GLB accepted: %d", code)
	}
	v1 := glb(5000)
	v1[4] = 1 // glTF version 1
	code, up = start(A, v1, hexSum(v1), "glb")
	if code != 201 {
		t.Fatalf("start: %d", code)
	}
	send(A, up["upload_id"].(string), v1)
	if code, _ := commit(A, up["upload_id"].(string)); code != 400 {
		t.Fatalf("glTF v1 accepted: %d", code)
	}
	for _, bad := range []map[string]any{
		{"name": "a.glb", "source_format": "dwg", "size": 10, "sha256": hexSum(file)},
		{"name": "a.glb", "source_format": "glb", "size": domain.SiteModelMaxSize + 1, "sha256": hexSum(file)},
		{"name": "a.glb", "source_format": "glb", "size": 0, "sha256": hexSum(file)},
		{"name": "a.glb", "source_format": "glb", "size": 19, "sha256": hexSum(file)},
		{"name": "a.glb", "source_format": "glb", "size": 10, "sha256": "abc"},
		{"name": " \u200B ", "source_format": "glb", "size": 10, "sha256": hexSum(file)},
		{"name": "a.glb", "source_format": "glb", "size": 10, "sha256": hexSum(file), "extra": 1},
	} {
		if code, _, _ := req(t, api, "POST", base+"/uploads", A, "", "", bad); code != 400 {
			t.Fatalf("invalid upload %v accepted: %d", bad, code)
		}
	}

	// A second upload replaces the file, bumps the revision and keeps the transform.
	file2 := glb(4000)
	code, up = start(A, file2, hexSum(file2), "glb")
	if code != 201 || up["chunks"] != float64(1) {
		t.Fatalf("start: %d %v", code, up)
	}
	send(A, up["upload_id"].(string), file2)
	code, out = commit(A, up["upload_id"].(string))
	model, _ = out["model"].(map[string]any)
	if code != 200 || model["revision"] != float64(2) || model["transform"].(map[string]any)["scale"] != 0.001 || model["transform"].(map[string]any)["up"] != "z" {
		t.Fatalf("second commit: %d %v", code, out)
	}
	if code, got, _ := rawGet(t, api, base+"/file?rev=2", A, etag); code != 200 || !bytes.Equal(got, file2) {
		t.Fatalf("the old ETag must not match the new file: %d", code)
	}
	if code, _, _ := rawGet(t, api, base+"/file?rev=1", A, ""); code != 404 {
		t.Fatalf("a replaced revision is still served: %d", code)
	}

	// Delete: the model is gone, and the next upload still gets a new revision (?rev=N never names two files).
	if code, _, _ := req(t, api, "POST", base+"/delete", A, "", "", nil); code != 204 {
		t.Fatalf("delete: %d", code)
	}
	if code, out, _ := req(t, api, "GET", base, A, "", "", nil); code != 200 || out["model"] != nil {
		t.Fatalf("after delete: %d %v", code, out)
	}
	if code, _, _ := rawGet(t, api, base+"/file", A, ""); code != 404 {
		t.Fatalf("file after delete: %d", code)
	}
	if code, _, _ := req(t, api, "POST", base+"/transform", A, "", "", map[string]any{"scale": 2}); code != 404 {
		t.Fatalf("transform without a model: %d", code)
	}
	if code, _, _ := req(t, api, "POST", base+"/delete", A, "", "", nil); code != 204 {
		t.Fatalf("delete is idempotent: %d", code)
	}
	code, up = start(A, file2, hexSum(file2), "glb")
	if code != 201 {
		t.Fatalf("start: %d", code)
	}
	send(A, up["upload_id"].(string), file2)
	if code, out := commit(A, up["upload_id"].(string)); code != 200 || out["model"].(map[string]any)["revision"] != float64(3) || out["model"].(map[string]any)["transform"].(map[string]any)["scale"] != float64(1) {
		t.Fatalf("upload after delete: %d %v", code, out)
	}
}

func TestSiteModelUploadsExpireAndAreCapped(t *testing.T) {
	f := setup(t)
	_, auth, owner := f.account(t)
	api := busyAPI(f)
	A := "Bearer " + auth.AccessToken
	file := glb(3000)
	sites := []string{}
	for i := 0; i < domain.SiteModelOpenUploads+1; i++ {
		code, site, _ := req(t, api, "POST", "/api/v1/sites", A, "", "", map[string]any{"name": "Site " + strconv.Itoa(i)})
		if code != 201 {
			t.Fatalf("create site: %d", code)
		}
		sites = append(sites, site["id"].(string))
	}
	start := func(site string) (int, map[string]any) {
		code, out, _ := req(t, api, "POST", "/api/v1/sites/"+site+"/model/uploads", A, "", "", map[string]any{"name": "m.glb", "source_format": "glb", "size": len(file), "sha256": hexSum(file)})
		return code, out
	}
	ids := []string{}
	for _, site := range sites[:domain.SiteModelOpenUploads] {
		code, out := start(site)
		if code != 201 {
			t.Fatalf("start: %d %v", code, out)
		}
		ids = append(ids, out["upload_id"].(string))
	}
	if code, _ := start(sites[domain.SiteModelOpenUploads]); code != 409 {
		t.Fatalf("open uploads must be capped: %d", code)
	}
	// Starting again on the same site replaces that site's unfinished upload.
	code, again := start(sites[0])
	if code != 201 {
		t.Fatalf("restart: %d", code)
	}
	if code, _, _ := req(t, api, "POST", "/api/v1/sites/"+sites[0]+"/model/uploads/"+ids[0]+"/chunks/0", A, "", "", map[string]any{"data_base64": base64.StdEncoding.EncodeToString(file)}); code != 404 {
		t.Fatalf("the replaced upload is gone: %d", code)
	}
	// An upload older than an hour is refused and swept by the next start.
	if _, e := f.admin.ExecContext(context.Background(), `UPDATE core.site_model_uploads SET created_at=now()-interval '61 minutes' WHERE tenant_id=$1 AND id=$2`, owner.TenantID, again["upload_id"]); e != nil {
		t.Fatal(e)
	}
	if code, _, _ := req(t, api, "POST", "/api/v1/sites/"+sites[0]+"/model/uploads/"+again["upload_id"].(string)+"/chunks/0", A, "", "", map[string]any{"data_base64": base64.StdEncoding.EncodeToString(file)}); code != 404 {
		t.Fatalf("an expired upload took a chunk: %d", code)
	}
	if code, _ := start(sites[domain.SiteModelOpenUploads]); code != 201 {
		t.Fatalf("the expired upload must free its slot: %d", code)
	}
	var left int
	if e := f.admin.QueryRowContext(context.Background(), `SELECT count(*) FROM core.site_model_uploads WHERE tenant_id=$1 AND id=$2`, owner.TenantID, again["upload_id"]).Scan(&left); e != nil || left != 0 {
		t.Fatalf("expired upload not swept: %d %v", left, e)
	}
	// Archived sites take no model.
	if code, _, _ := req(t, api, "POST", "/api/v1/sites/"+sites[1]+"/archive", A, "", "", map[string]any{}); code != 204 {
		t.Fatalf("archive: %d", code)
	}
	if code, _, _ := req(t, api, "POST", "/api/v1/sites/"+sites[1]+"/model/uploads/"+ids[1]+"/commit", A, "", "", nil); code != 404 {
		t.Fatalf("commit on an archived site: %d", code)
	}
}

func TestSiteModelFollowsProjectScope(t *testing.T) {
	f := setup(t)
	_, auth, owner := f.account(t)
	api := busyAPI(f)
	ctx := context.Background()
	A := "Bearer " + auth.AccessToken
	pa, e := f.service.CreateProject(ctx, owner, "Building A", "", "mint")
	if e != nil {
		t.Fatal(e)
	}
	pb, e := f.service.CreateProject(ctx, owner, "Building B", "", "blue")
	if e != nil {
		t.Fatal(e)
	}
	code, site, _ := req(t, api, "POST", "/api/v1/sites", A, "", "", map[string]any{"name": "B building", "project_id": pb.ID})
	if code != 201 {
		t.Fatalf("create site: %d", code)
	}
	email := memberEmail()
	addMember(t, api, auth.AccessToken, email, "operator", []string{pa.ID})
	limited, _ := changeInitialPassword(t, f, api, email, owner.TenantID)
	L := "Bearer " + limited.AccessToken
	base := "/api/v1/sites/" + site["id"].(string) + "/model"
	file := glb(3000)
	// A member restricted to project A sees nothing of project B's building (owner/admin are never restricted).
	if code, _, _ := req(t, api, "GET", base, L, "", "", nil); code != 404 {
		t.Fatalf("out-of-scope model read: %d", code)
	}
	if code, _, _ := rawGet(t, api, base+"/file", L, ""); code != 404 {
		t.Fatalf("out-of-scope file read: %d", code)
	}
	if code, _, _ := req(t, api, "POST", base+"/uploads", L, "", "", map[string]any{"name": "m.glb", "source_format": "glb", "size": len(file), "sha256": hexSum(file)}); code != 403 {
		t.Fatalf("an operator started an upload: %d", code)
	}
}

// The largest model goes through: 205 chunks, read back in several slices.
func TestSiteModelLargestFile(t *testing.T) {
	f := setup(t)
	_, auth, _ := f.account(t)
	api := busyAPI(f)
	A := "Bearer " + auth.AccessToken
	code, site, _ := req(t, api, "POST", "/api/v1/sites", A, "", "", map[string]any{"name": "Large"})
	if code != 201 {
		t.Fatalf("create site: %d", code)
	}
	base := "/api/v1/sites/" + site["id"].(string) + "/model"
	file := glb(domain.SiteModelMaxSize - 64)
	if len(file) > domain.SiteModelMaxSize {
		t.Fatalf("test file too large: %d", len(file))
	}
	code, up, _ := req(t, api, "POST", base+"/uploads", A, "", "", map[string]any{"name": "large.glb", "source_format": "glb", "size": len(file), "sha256": hexSum(file)})
	if code != 201 || up["chunks"] != float64(domain.SiteModelChunks(len(file))) {
		t.Fatalf("start: %d %v", code, up)
	}
	upload := up["upload_id"].(string)
	for i := 0; i*domain.SiteModelChunkSize < len(file); i++ {
		part := file[i*domain.SiteModelChunkSize : min((i+1)*domain.SiteModelChunkSize, len(file))]
		if code, out, _ := req(t, api, "POST", base+"/uploads/"+upload+"/chunks/"+strconv.Itoa(i), A, "", "", map[string]any{"data_base64": base64.StdEncoding.EncodeToString(part)}); code != 204 {
			t.Fatalf("chunk %d: %d %v", i, code, out)
		}
	}
	if code, out, _ := req(t, api, "POST", base+"/uploads/"+upload+"/commit", A, "", "", nil); code != 200 {
		t.Fatalf("commit: %d %v", code, out)
	}
	code, got, h := rawGet(t, api, base+"/file?rev=1", A, "")
	if code != 200 || !bytes.Equal(got, file) || h["Content-Length"] != strconv.Itoa(len(file)) {
		t.Fatalf("file: %d, %d of %d bytes", code, len(got), len(file))
	}
}

// Another member's unfinished upload is not cancelled by a new start (409); one's own is replaced. Expired uploads
// are swept by the maintenance loop even when nobody uploads again.
func TestSiteModelUploadOwnershipAndSweep(t *testing.T) {
	f := setup(t)
	_, auth, owner := f.account(t)
	api := busyAPI(f)
	A := "Bearer " + auth.AccessToken
	code, site, _ := req(t, api, "POST", "/api/v1/sites", A, "", "", map[string]any{"name": "Shared"})
	if code != 201 {
		t.Fatalf("create site: %d", code)
	}
	base := "/api/v1/sites/" + site["id"].(string) + "/model"
	email := memberEmail()
	addMember(t, api, auth.AccessToken, email, "admin", nil)
	admin, _ := changeInitialPassword(t, f, api, email, owner.TenantID)
	D := "Bearer " + admin.AccessToken
	file := glb(3000)
	start := func(token string) (int, map[string]any) {
		code, out, _ := req(t, api, "POST", base+"/uploads", token, "", "", map[string]any{"name": "m.glb", "source_format": "glb", "size": len(file), "sha256": hexSum(file)})
		return code, out
	}
	code, first := start(D)
	if code != 201 {
		t.Fatalf("admin start: %d", code)
	}
	if code, out := start(A); code != 409 || out["error"] != "upload_in_progress" || out["upload_id"] != first["upload_id"] || out["started_at"] == nil {
		t.Fatalf("another member's unfinished upload was cancelled, or the 409 does not name it: %d %v", code, out)
	}
	// Any manager may cancel it; then the start goes through.
	cancel := func(token, upload string) int {
		code, _, _ := req(t, api, "POST", base+"/uploads/"+upload+"/cancel", token, "", "", nil)
		return code
	}
	if code := cancel(A, first["upload_id"].(string)); code != 204 {
		t.Fatalf("cancel a colleague's upload: %d", code)
	}
	if code := cancel(A, first["upload_id"].(string)); code != 404 {
		t.Fatalf("cancel twice: %d", code)
	}
	code, mine := start(A)
	if code != 201 {
		t.Fatalf("start after cancel: %d", code)
	}
	if code := cancel(A, mine["upload_id"].(string)); code != 204 {
		t.Fatalf("cancel own upload: %d", code)
	}
	code, first = start(D)
	if code != 201 {
		t.Fatalf("admin start again: %d", code)
	}
	code, second := start(D)
	if code != 201 {
		t.Fatalf("restarting one's own upload: %d", code)
	}
	chunk := func(token, upload string) int {
		code, _, _ := req(t, api, "POST", base+"/uploads/"+upload+"/chunks/0", token, "", "", map[string]any{"data_base64": base64.StdEncoding.EncodeToString(file)})
		return code
	}
	if code := chunk(D, first["upload_id"].(string)); code != 404 {
		t.Fatalf("the replaced upload still takes chunks: %d", code)
	}
	if code := chunk(D, second["upload_id"].(string)); code != 204 {
		t.Fatalf("chunk: %d", code)
	}
	// An hour later the upload is expired: the sweep removes it and its chunks, across workspaces.
	ctx := context.Background()
	if _, e := f.admin.ExecContext(ctx, `UPDATE core.site_model_uploads SET created_at=now()-interval '61 minutes' WHERE tenant_id=$1`, owner.TenantID); e != nil {
		t.Fatal(e)
	}
	if n, e := f.repo.SweepSiteModelUploads(ctx); e != nil || n < 1 {
		t.Fatalf("sweep: %d %v", n, e)
	}
	var left int
	if e := f.admin.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM core.site_model_uploads WHERE tenant_id=$1)+(SELECT count(*) FROM core.site_model_chunks WHERE tenant_id=$1)`, owner.TenantID).Scan(&left); e != nil || left != 0 {
		t.Fatalf("expired upload not swept: %d %v", left, e)
	}
	if code, _ := start(A); code != 201 {
		t.Fatalf("start after the sweep: %d", code)
	}
}

// A chunk write and the commit of its upload exclude each other (FOR KEY SHARE against FOR UPDATE on the upload
// row), so the bytes a commit stores are the bytes it checked: a stored model always matches its sha256.
func TestSiteModelChunkCannotChangeDuringCommit(t *testing.T) {
	f := setup(t)
	_, auth, owner := f.account(t)
	api := busyAPI(f)
	A := "Bearer " + auth.AccessToken
	code, site, _ := req(t, api, "POST", "/api/v1/sites", A, "", "", map[string]any{"name": "Race"})
	if code != 201 {
		t.Fatalf("create site: %d", code)
	}
	base := "/api/v1/sites/" + site["id"].(string) + "/model"
	file := glb(domain.SiteModelChunkSize + 5000)
	bad := bytes.Repeat([]byte{0xAB}, domain.SiteModelChunkSize)
	start := func() string {
		t.Helper()
		code, up, _ := req(t, api, "POST", base+"/uploads", A, "", "", map[string]any{"name": "m.glb", "source_format": "glb", "size": len(file), "sha256": hexSum(file)})
		if code != 201 {
			t.Fatalf("start: %d %v", code, up)
		}
		upload := up["upload_id"].(string)
		for i := 0; i*domain.SiteModelChunkSize < len(file); i++ {
			part := file[i*domain.SiteModelChunkSize : min((i+1)*domain.SiteModelChunkSize, len(file))]
			if code, _, _ := req(t, api, "POST", base+"/uploads/"+upload+"/chunks/"+strconv.Itoa(i), A, "", "", map[string]any{"data_base64": base64.StdEncoding.EncodeToString(part)}); code != 204 {
				t.Fatalf("chunk %d: %d", i, code)
			}
		}
		return upload
	}
	rewrite := func(upload string) int {
		code, _, _ := req(t, api, "POST", base+"/uploads/"+upload+"/chunks/0", A, "", "", map[string]any{"data_base64": base64.StdEncoding.EncodeToString(bad)})
		return code
	}

	// While the upload row is locked FOR UPDATE (what the commit holds), a chunk write waits.
	ctx := context.Background()
	upload := start()
	tx, e := f.admin.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := tx.ExecContext(ctx, `SELECT 1 FROM core.site_model_uploads WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, owner.TenantID, upload); e != nil {
		t.Fatal(e)
	}
	done := make(chan int, 1)
	go func() { done <- rewrite(upload) }()
	select {
	case code := <-done:
		t.Fatalf("a chunk was written while the commit held the upload: %d", code)
	case <-time.After(700 * time.Millisecond):
	}
	// The commit finishes and deletes the upload: the waiting chunk write then finds nothing to write to.
	if _, e := tx.ExecContext(ctx, `DELETE FROM core.site_model_uploads WHERE tenant_id=$1 AND id=$2`, owner.TenantID, upload); e != nil {
		t.Fatal(e)
	}
	if e := tx.Commit(); e != nil {
		t.Fatal(e)
	}
	if code := <-done; code != 404 {
		t.Fatalf("a chunk written after its upload committed: %d", code)
	}

	// Commit and a rewrite of chunk 0 race: either the rewrite lands first (the sha256 check refuses the commit) or
	// the commit stores the checked bytes; never a model whose bytes differ from its sha256.
	stored := 0
	for round := 0; round < 12; round++ {
		upload := start()
		var wg sync.WaitGroup
		var commitCode, chunkCode int
		var model map[string]any
		wg.Add(2)
		go func() {
			defer wg.Done()
			var out map[string]any
			commitCode, out, _ = req(t, api, "POST", base+"/uploads/"+upload+"/commit", A, "", "", nil)
			model, _ = out["model"].(map[string]any)
		}()
		go func() { defer wg.Done(); chunkCode = rewrite(upload) }()
		wg.Wait()
		switch {
		case commitCode == 200:
			stored++
			code, got, h := rawGet(t, api, base+"/file?rev="+strconv.Itoa(int(model["revision"].(float64))), A, "")
			if code != 200 || !bytes.Equal(got, file) || h["ETag"] != `"`+hexSum(got)+`"` || model["sha256"] != hexSum(got) {
				t.Fatalf("round %d: stored bytes do not match their sha256 (chunk %d)", round, chunkCode)
			}
			if chunkCode != 204 && chunkCode != 404 {
				t.Fatalf("round %d: chunk %d", round, chunkCode)
			}
		case commitCode == 400 && chunkCode == 204:
			// The rewrite came first; the commit refused the changed bytes.
		default:
			t.Fatalf("round %d: commit %d, chunk %d", round, commitCode, chunkCode)
		}
	}
	t.Logf("%d of 12 rounds committed", stored)
}

// At most domain.SiteModelCommitSlots commits run at once across workspaces; without a free slot the commit is 503
// with Retry-After, and nothing is changed.
func TestSiteModelCommitSlots(t *testing.T) {
	f := setup(t)
	_, auth, _ := f.account(t)
	api := busyAPI(f)
	A := "Bearer " + auth.AccessToken
	code, site, _ := req(t, api, "POST", "/api/v1/sites", A, "", "", map[string]any{"name": "Slots"})
	if code != 201 {
		t.Fatalf("create site: %d", code)
	}
	base := "/api/v1/sites/" + site["id"].(string) + "/model"
	file := glb(3000)
	code, up, _ := req(t, api, "POST", base+"/uploads", A, "", "", map[string]any{"name": "m.glb", "source_format": "glb", "size": len(file), "sha256": hexSum(file)})
	if code != 201 {
		t.Fatalf("start: %d", code)
	}
	upload := up["upload_id"].(string)
	if code, _, _ := req(t, api, "POST", base+"/uploads/"+upload+"/chunks/0", A, "", "", map[string]any{"data_base64": base64.StdEncoding.EncodeToString(file)}); code != 204 {
		t.Fatalf("chunk: %d", code)
	}
	// Other commits hold every slot (the keys of postgres.siteModelCommitKey).
	ctx := context.Background()
	tx, e := f.admin.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < domain.SiteModelCommitSlots; i++ {
		if _, e := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "aether.site_model_commit."+strconv.Itoa(i)); e != nil {
			t.Fatal(e)
		}
	}
	code, _, res := req(t, api, "POST", base+"/uploads/"+upload+"/commit", A, "", "", nil)
	if code != 503 || res.Header.Get("Retry-After") == "" {
		t.Fatalf("commit without a free slot: %d %q", code, res.Header.Get("Retry-After"))
	}
	if e := tx.Rollback(); e != nil {
		t.Fatal(e)
	}
	if code, out, _ := req(t, api, "POST", base+"/uploads/"+upload+"/commit", A, "", "", nil); code != 200 || out["model"].(map[string]any)["revision"] != float64(1) {
		t.Fatalf("commit once a slot is free: %d %v", code, out)
	}
}
