package postgres

import (
	"aether/backend/internal/domain"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type siteModelRow struct {
	Revision     int             `gorm:"column:revision"`
	Name         string          `gorm:"column:name"`
	SourceFormat string          `gorm:"column:source_format"`
	Size         int             `gorm:"column:size"`
	Sha          string          `gorm:"column:sha"`
	UploadedAt   time.Time       `gorm:"column:uploaded_at"`
	Transform    json.RawMessage `gorm:"column:transform"`
}

func (row siteModelRow) model() *domain.SiteModel {
	t := domain.DefaultModelTransform()
	_ = json.Unmarshal(row.Transform, &t)
	if t.Validate() != nil {
		t = domain.DefaultModelTransform()
	}
	return &domain.SiteModel{Revision: row.Revision, Name: row.Name, SourceFormat: row.SourceFormat, Size: row.Size, SHA256: row.Sha, UploadedAt: row.UploadedAt, Transform: t}
}

const siteModelColumns = `m.revision,m.name,m.source_format,m.size,m.sha256 AS sha,m.uploaded_at,m.transform`

// activeSite answers whether the caller sees the site and it is not archived; row level security decides the first.
func activeSite(tx *gorm.DB, site string) error {
	var n int64
	if e := tx.Raw(`SELECT count(*) FROM core.sites WHERE id=? AND archived_at IS NULL`, site).Scan(&n).Error; e != nil {
		return e
	}
	if n != 1 {
		return domain.ErrNotFound
	}
	return nil
}

func readSiteModel(tx *gorm.DB, site string) (*domain.SiteModel, error) {
	var rows []siteModelRow
	if e := tx.Raw(`SELECT `+siteModelColumns+` FROM core.site_models m WHERE m.site_id=?`, site).Scan(&rows).Error; e != nil {
		return nil, e
	}
	if len(rows) != 1 {
		return nil, nil
	}
	return rows[0].model(), nil
}

// SiteModel returns the model of a site, or nil when it has none.
func (r *Repository) SiteModel(ctx context.Context, p domain.Principal, site string) (*domain.SiteModel, error) {
	var out *domain.SiteModel
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) (e error) {
		if e = activeSite(tx, site); e != nil {
			return e
		}
		out, e = readSiteModel(tx, site)
		return e
	})
	return out, e
}

// SiteModelSlice reads n bytes of the site's model from offset (0-based), only while the model is still at revision:
// GET …/model/file streams the file in slices, each in its own short transaction, so no request holds the whole file
// in memory or a connection for the length of a download. A model replaced meanwhile is ErrNotFound.
func (r *Repository) SiteModelSlice(ctx context.Context, p domain.Principal, site string, revision, offset, n int) ([]byte, error) {
	var part []struct{ Part []byte }
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		return tx.Raw(`SELECT substring(m.data FROM ? FOR ?) AS part FROM core.site_models m
      JOIN core.sites s ON s.tenant_id=m.tenant_id AND s.id=m.site_id AND s.archived_at IS NULL
      WHERE m.site_id=? AND m.revision=?`, offset+1, n, site, revision).Scan(&part).Error
	})
	if e != nil {
		return nil, e
	}
	if len(part) != 1 || len(part[0].Part) == 0 {
		return nil, domain.ErrNotFound
	}
	return part[0].Part, nil
}

// StartSiteModelUpload opens an upload. It first drops every expired upload of the workspace, refuses (ErrConflict)
// while another member's unfinished upload to the same site is still usable, replaces the caller's own, and refuses
// when the workspace already has domain.SiteModelOpenUploads open.
func (r *Repository) StartSiteModelUpload(ctx context.Context, p domain.Principal, u domain.SiteModelUpload) (domain.SiteModelUpload, error) {
	u.ID = uuid.NewString()
	u.ChunkSize = domain.SiteModelChunkSize
	u.Chunks = domain.SiteModelChunks(u.Size)
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if e := activeSite(tx, u.SiteID); e != nil {
			return e
		}
		// One upload start at a time per workspace, so the checks below cannot be raced past.
		if e := tx.Exec(`SELECT pg_advisory_xact_lock(hashtextextended(?,48))`, p.TenantID).Error; e != nil {
			return e
		}
		if e := tx.Exec(`DELETE FROM core.site_model_uploads WHERE created_at < now() - make_interval(secs => ?)`, domain.SiteModelUploadTTL.Seconds()).Error; e != nil {
			return e
		}
		var other []struct {
			ID        string
			CreatedAt time.Time
		}
		if e := tx.Raw(`SELECT id,created_at FROM core.site_model_uploads WHERE site_id=? AND created_by IS DISTINCT FROM ? ORDER BY created_at LIMIT 1`, u.SiteID, p.UserID).Scan(&other).Error; e != nil {
			return e
		}
		if len(other) > 0 {
			return &domain.SiteModelUploadInProgress{UploadID: other[0].ID, StartedAt: other[0].CreatedAt}
		}
		var n int64
		if e := tx.Exec(`DELETE FROM core.site_model_uploads WHERE site_id=?`, u.SiteID).Error; e != nil {
			return e
		}
		if e := tx.Raw(`SELECT count(*) FROM core.site_model_uploads`).Scan(&n).Error; e != nil {
			return e
		}
		if n >= domain.SiteModelOpenUploads {
			return domain.ErrConflict
		}
		return tx.Exec(`INSERT INTO core.site_model_uploads(tenant_id,id,site_id,name,source_format,size,sha256,chunks,created_by) VALUES(?,?,?,?,?,?,?,?,?)`,
			p.TenantID, u.ID, u.SiteID, u.Name, u.SourceFormat, u.Size, u.SHA256, u.Chunks, p.UserID).Error
	})
	return u, classify(e)
}

type uploadRow struct {
	Name         string `gorm:"column:name"`
	SourceFormat string `gorm:"column:source_format"`
	Size, Chunks int
	Sha          string `gorm:"column:sha"`
}

// openUpload reads an unexpired upload of an active site and locks its row: a chunk write takes FOR KEY SHARE and the
// commit FOR UPDATE, which conflict, so no chunk can change between the commit's checks and the bytes it stores.
func openUpload(tx *gorm.DB, site, upload, lock string) (uploadRow, error) {
	if e := activeSite(tx, site); e != nil {
		return uploadRow{}, e
	}
	q := `SELECT name,source_format,size,chunks,sha256 AS sha FROM core.site_model_uploads WHERE id=? AND site_id=? AND created_at >= now() - make_interval(secs => ?)`
	q += ` FOR ` + lock
	var rows []uploadRow
	if e := tx.Raw(q, upload, site, domain.SiteModelUploadTTL.Seconds()).Scan(&rows).Error; e != nil {
		return uploadRow{}, e
	}
	if len(rows) != 1 {
		return uploadRow{}, domain.ErrNotFound
	}
	return rows[0], nil
}

// PutSiteModelChunk stores chunk idx. Every chunk but the last is exactly the chunk size, the last one makes up the
// declared size; sending an index again replaces it, so a client may retry.
func (r *Repository) PutSiteModelChunk(ctx context.Context, p domain.Principal, site, upload string, idx int, data []byte) error {
	return classify(r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		u, e := openUpload(tx, site, upload, "KEY SHARE")
		if e != nil {
			return e
		}
		want := domain.SiteModelChunkSize
		if idx == u.Chunks-1 {
			want = u.Size - domain.SiteModelChunkSize*(u.Chunks-1)
		}
		if idx < 0 || idx >= u.Chunks || len(data) != want {
			return domain.ErrInvalid
		}
		return tx.Exec(`INSERT INTO core.site_model_chunks(tenant_id,upload_id,site_id,idx,data) VALUES(?,?,?,?,?)
      ON CONFLICT(tenant_id,upload_id,idx) DO UPDATE SET data=EXCLUDED.data`, p.TenantID, upload, site, idx, data).Error
	}))
}

// siteModelCommitKey is the advisory lock key of commit slot i (domain.SiteModelCommitSlots of them).
func siteModelCommitKey(i int) string { return "aether.site_model_commit." + strconv.Itoa(i) }

// checkUpload reads the chunks one at a time in order (one chunk in memory, never the file) and checks that they are
// all there, add up to the declared size, hash to the declared sha256 and start with a GLB v2 header. The commit
// holds the upload FOR UPDATE, so these are the bytes it then stores.
func checkUpload(tx *gorm.DB, upload string, u uploadRow) error {
	rows, e := tx.Raw(`SELECT idx,data FROM core.site_model_chunks WHERE upload_id=? ORDER BY idx`, upload).Rows()
	if e != nil {
		return e
	}
	defer rows.Close()
	sum := sha256.New()
	head := make([]byte, 0, 20)
	next, size := 0, 0
	for rows.Next() {
		var idx int
		var data []byte
		if e := rows.Scan(&idx, &data); e != nil {
			return e
		}
		if idx != next {
			return domain.ErrInvalid
		}
		next++
		size += len(data)
		sum.Write(data)
		if len(head) < 20 {
			head = append(head, data[:min(len(data), 20-len(head))]...)
		}
	}
	if e := rows.Err(); e != nil {
		return e
	}
	if next != u.Chunks || size != u.Size || hex.EncodeToString(sum.Sum(nil)) != u.Sha || !domain.ValidGLBHeader(head, u.Size) {
		return domain.ErrInvalid
	}
	return nil
}

// CommitSiteModel checks the upload (every chunk, the size, the sha256, a GLB v2 container) and makes it the site's
// model in one transaction: the revision goes up by one and the transform of the previous model is kept. The bytes
// are concatenated once, by the INSERT. At most domain.SiteModelCommitSlots commits run at once across workspaces
// (transaction advisory locks); without a free slot the commit is ErrUnavailable and the client retries.
func (r *Repository) CommitSiteModel(ctx context.Context, p domain.Principal, site, upload string) (*domain.SiteModel, error) {
	var out *domain.SiteModel
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		slot := false
		for i := 0; i < domain.SiteModelCommitSlots && !slot; i++ {
			if e := tx.Raw(`SELECT pg_try_advisory_xact_lock(hashtextextended(?,0))`, siteModelCommitKey(i)).Scan(&slot).Error; e != nil {
				return e
			}
		}
		if !slot {
			return domain.ErrUnavailable
		}
		u, e := openUpload(tx, site, upload, "UPDATE")
		if e != nil {
			return e
		}
		// Checked before anything is written, so a rejected file costs no WAL.
		if e := checkUpload(tx, upload, u); e != nil {
			return e
		}
		var revision []int
		if e := tx.Raw(`UPDATE core.sites SET model_revision=model_revision+1 WHERE id=? AND archived_at IS NULL RETURNING model_revision`, site).Scan(&revision).Error; e != nil {
			return e
		}
		if len(revision) != 1 {
			return domain.ErrNotFound
		}
		transform, _ := json.Marshal(domain.DefaultModelTransform())
		if e := tx.Exec(`INSERT INTO core.site_models(tenant_id,site_id,revision,name,source_format,size,sha256,transform,data,uploaded_by)
      SELECT ?,?,?,?,?,?,?,?::jsonb,string_agg(data,''::bytea ORDER BY idx),? FROM core.site_model_chunks WHERE upload_id=?
      ON CONFLICT(tenant_id,site_id) DO UPDATE SET revision=EXCLUDED.revision,name=EXCLUDED.name,source_format=EXCLUDED.source_format,
        size=EXCLUDED.size,sha256=EXCLUDED.sha256,data=EXCLUDED.data,uploaded_by=EXCLUDED.uploaded_by,uploaded_at=now()`,
			p.TenantID, site, revision[0], u.Name, u.SourceFormat, u.Size, u.Sha, string(transform), p.UserID, upload).Error; e != nil {
			return e
		}
		if e := tx.Exec(`DELETE FROM core.site_model_uploads WHERE id=?`, upload).Error; e != nil {
			return e
		}
		if out, e = readSiteModel(tx, site); e != nil {
			return e
		}
		// The twin page refetches on this signal (frontend/app/twin/twin.tsx), like after a floor save.
		if e := signal(tx, p.TenantID, "inventory", ""); e != nil {
			return e
		}
		return audit(tx, p, "site.model.uploaded", site)
	})
	return out, e
}

func (r *Repository) SetSiteModelTransform(ctx context.Context, p domain.Principal, site string, t domain.ModelTransform) (*domain.SiteModel, error) {
	var out *domain.SiteModel
	e := r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if e := activeSite(tx, site); e != nil {
			return e
		}
		transform, _ := json.Marshal(t)
		res := tx.Exec(`UPDATE core.site_models SET transform=?::jsonb WHERE site_id=?`, string(transform), site)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return domain.ErrNotFound
		}
		var e error
		if out, e = readSiteModel(tx, site); e != nil {
			return e
		}
		if e := signal(tx, p.TenantID, "inventory", ""); e != nil {
			return e
		}
		return audit(tx, p, "site.model.transformed", site)
	})
	return out, e
}

// DeleteSiteModel removes the model and any upload in progress; the revision counter stays where it was. Deleting
// a model that is already gone succeeds.
func (r *Repository) DeleteSiteModel(ctx context.Context, p domain.Principal, site string) error {
	return r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if e := activeSite(tx, site); e != nil {
			return e
		}
		if e := tx.Exec(`DELETE FROM core.site_model_uploads WHERE site_id=?`, site).Error; e != nil {
			return e
		}
		res := tx.Exec(`DELETE FROM core.site_models WHERE site_id=?`, site)
		if res.Error != nil || res.RowsAffected == 0 {
			return res.Error
		}
		if e := signal(tx, p.TenantID, "inventory", ""); e != nil {
			return e
		}
		return audit(tx, p, "site.model.deleted", site)
	})
}

// SweepSiteModelUploads deletes the uploads of every workspace that expired (domain.SiteModelUploadTTL) with their
// chunks, through the owner-only core.sweep_site_model_uploads (no tenant context exists here). The partition
// maintenance loop calls it, so an upload nobody finishes is gone within two hours even if nobody uploads again.
func (r *Repository) SweepSiteModelUploads(ctx context.Context) (int64, error) {
	var n int64
	e := r.db.WithContext(ctx).Raw(`SELECT core.sweep_site_model_uploads()`).Scan(&n).Error
	return n, e
}

// CancelSiteModelUpload deletes an unfinished upload (and its chunks) of the site, whoever started it; the model is
// untouched. A commit running at the same time finishes first (it holds the upload row), and the cancel is then 404.
func (r *Repository) CancelSiteModelUpload(ctx context.Context, p domain.Principal, site, upload string) error {
	return r.tx(ctx, p.UserID, p.TenantID, func(tx *gorm.DB) error {
		if e := activeSite(tx, site); e != nil {
			return e
		}
		res := tx.Exec(`DELETE FROM core.site_model_uploads WHERE id=? AND site_id=?`, upload, site)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return domain.ErrNotFound
		}
		return audit(tx, p, "site.model.upload_cancelled", site)
	})
}
