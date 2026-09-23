package httpapi

import (
	"aether/backend/internal/app"
	"aether/backend/internal/domain"
	"aether/backend/internal/security"
	"bytes"
	"encoding/csv"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

// "อุปกรณ์ทั้งหมด": one registry row per gateway and per active device, with the owner's own bookkeeping
// (serial, asset tag, warranty), preventive plans and an append-only maintenance history.
// Viewers may read everything; every mutation needs owner/admin.

const (
	assetNameMax   = 128  // bytes; names, titles, tags, serials, vendors, people
	assetDetailMax = 4000 // bytes; notes and log details
)

// text checks a UTF-8 length in bytes and rejects control characters that would corrupt a CSV export.
func assetText(s string, max int) (string, bool) {
	s = strings.TrimSpace(s)
	return s, len(s) <= max && !strings.ContainsAny(s, "\x00")
}

// assetDate accepts "" (not set) or a real calendar day; anything else is invalid input.
func assetDate(s string) (string, bool) {
	if s == "" {
		return "", true
	}
	d, e := time.Parse("2006-01-02", s)
	return s, e == nil && d.Format("2006-01-02") == s
}

// csvCell neutralises formula injection: Excel and Sheets execute a cell that starts with = + - @.
func csvCell(s string) string {
	if s != "" && strings.ContainsAny(s[:1], "=+-@") {
		return "'" + s
	}
	return s
}

func assetRoutes(r fiber.Router, s *app.Service) {
	principal := func(c fiber.Ctx) domain.Principal { return c.Locals("principal").(domain.Principal) }
	manage := func(c fiber.Ctx) (domain.Principal, error) {
		p := principal(c)
		if !p.CanManageDevices() {
			return p, domain.ErrForbidden
		}
		return p, nil
	}
	// Every /assets/:kind/:id route addresses one polymorphic asset; the repository proves it is live.
	target := func(c fiber.Ctx) (string, string, error) {
		kind, id := c.Params("kind"), c.Params("id")
		if !slices.Contains(domain.AssetKinds, kind) || !security.ValidID(id) {
			return "", "", domain.ErrInvalid
		}
		return kind, id, nil
	}

	r.Get("/assets", func(c fiber.Ctx) error {
		out, e := s.Repo.ListAssets(c.Context(), principal(c))
		if e != nil {
			return e
		}
		return c.JSON(fiber.Map{"items": out, "server_time": time.Now().UTC(), "limit": 500})
	})

	// UTF-8 with a BOM so Excel opens the Thai headers correctly without an import wizard.
	r.Get("/assets/export.csv", func(c fiber.Ctx) error {
		rows, e := s.Repo.ListAssets(c.Context(), principal(c))
		if e != nil {
			return e
		}
		buf := bytes.NewBufferString("\ufeff")
		w := csv.NewWriter(buf)
		if e := w.Write([]string{"ชนิด", "ชื่อ", "รุ่น/โปรไฟล์", "MAC/ID", "โปรเจค", "gateway", "สถานะ", "last_seen", "แบตเตอรี่ (%)", "asset tag", "serial", "ประกันถึง", "เปลี่ยนแบตล่าสุด", "MA/PM ถัดไป", "เกินกำหนด", "จุดติดตั้ง", "หมายเหตุ"}); e != nil {
			return e
		}
		for _, a := range rows {
			lastSeen, battery := "", ""
			if a.LastSeen != nil {
				lastSeen = a.LastSeen.UTC().Format(time.RFC3339)
			}
			if a.Battery != nil {
				battery = strconv.FormatFloat(*a.Battery, 'f', -1, 64)
			}
			id := a.ExternalID
			if id == "" {
				id = a.AssetID
			}
			if e := w.Write([]string{
				csvCell(a.AssetKind), csvCell(a.Name), csvCell(a.Model), csvCell(id), csvCell(a.ProjectName),
				csvCell(a.GatewayName), csvCell(a.Status), lastSeen, battery, csvCell(a.AssetTag), csvCell(a.SerialNo),
				a.WarrantyUntil, a.BatteryChangedAt, a.NextDue, strconv.Itoa(a.OverdueCount),
				csvCell(a.LocationNote), csvCell(a.Notes),
			}); e != nil {
				return e
			}
		}
		w.Flush()
		if e := w.Error(); e != nil {
			return e
		}
		c.Set("Content-Type", "text/csv; charset=utf-8")
		c.Set("Content-Disposition", `attachment; filename="aether-assets.csv"`)
		return c.Send(buf.Bytes())
	})

	r.Get("/assets/:kind/:id", func(c fiber.Ctx) error {
		kind, id, e := target(c)
		if e != nil {
			return e
		}
		out, e := s.Repo.GetAsset(c.Context(), principal(c), kind, id)
		if e != nil {
			return e
		}
		return c.JSON(out)
	})

	r.Post("/assets/:kind/:id", func(c fiber.Ctx) error {
		p, e := manage(c)
		if e != nil {
			return e
		}
		kind, id, e := target(c)
		if e != nil {
			return e
		}
		var in struct {
			SerialNo         string `json:"serial_no"`
			AssetTag         string `json:"asset_tag"`
			LocationNote     string `json:"location_note"`
			Vendor           string `json:"vendor"`
			PurchasedAt      string `json:"purchased_at"`
			WarrantyUntil    string `json:"warranty_until"`
			BatteryChangedAt string `json:"battery_changed_at"`
			Status           string `json:"status"`
			Notes            string `json:"notes"`
		}
		if e := body(c, &in, 8192); e != nil {
			return e
		}
		rec := domain.AssetRecord{AssetKind: kind, AssetID: id, Status: in.Status}
		ok := true
		for _, f := range []struct {
			dst *string
			src string
			max int
		}{
			{&rec.SerialNo, in.SerialNo, assetNameMax}, {&rec.AssetTag, in.AssetTag, assetNameMax},
			{&rec.LocationNote, in.LocationNote, assetNameMax}, {&rec.Vendor, in.Vendor, assetNameMax},
			{&rec.Notes, in.Notes, assetDetailMax},
		} {
			v, good := assetText(f.src, f.max)
			*f.dst, ok = v, ok && good
		}
		var purchased, warranty, battery bool
		rec.PurchasedAt, purchased = assetDate(in.PurchasedAt)
		rec.WarrantyUntil, warranty = assetDate(in.WarrantyUntil)
		rec.BatteryChangedAt, battery = assetDate(in.BatteryChangedAt)
		if rec.Status == "" {
			rec.Status = "in_service"
		}
		if !ok || !purchased || !warranty || !battery || !slices.Contains(domain.AssetStatuses, rec.Status) {
			return domain.ErrInvalid
		}
		if e := s.Repo.UpsertAssetRecord(c.Context(), p, rec); e != nil {
			return e
		}
		out, e := s.Repo.GetAsset(c.Context(), p, kind, id)
		if e != nil {
			return e
		}
		return c.JSON(out)
	})

	type planInput struct {
		Title        string `json:"title"`
		Kind         string `json:"kind"`
		IntervalDays int    `json:"interval_days"`
		NextDue      string `json:"next_due"`
		Enabled      *bool  `json:"enabled"`
	}
	readPlan := func(c fiber.Ctx) (domain.MaintenancePlan, error) {
		var in planInput
		if e := body(c, &in, 4096); e != nil {
			return domain.MaintenancePlan{}, e
		}
		title, ok := assetText(in.Title, assetNameMax)
		due, dateOK := assetDate(in.NextDue)
		if !ok || title == "" || due == "" || !dateOK || !slices.Contains(domain.PlanKinds, in.Kind) || in.IntervalDays < 1 || in.IntervalDays > 3650 {
			return domain.MaintenancePlan{}, domain.ErrInvalid
		}
		enabled := in.Enabled == nil || *in.Enabled
		return domain.MaintenancePlan{Title: title, Kind: in.Kind, IntervalDays: in.IntervalDays, NextDue: due, Enabled: enabled}, nil
	}

	r.Post("/assets/:kind/:id/plans", func(c fiber.Ctx) error {
		p, e := manage(c)
		if e != nil {
			return e
		}
		kind, id, e := target(c)
		if e != nil {
			return e
		}
		plan, e := readPlan(c)
		if e != nil {
			return e
		}
		plan.ID, plan.AssetKind, plan.AssetID = uuid.NewString(), kind, id
		if e := s.Repo.CreatePlan(c.Context(), p, plan); e != nil {
			return e
		}
		return c.Status(201).JSON(plan)
	})

	r.Post("/plans/:id/update", func(c fiber.Ctx) error {
		p, e := manage(c)
		if e != nil {
			return e
		}
		if !security.ValidID(c.Params("id")) {
			return domain.ErrInvalid
		}
		plan, e := readPlan(c)
		if e != nil {
			return e
		}
		plan.ID = c.Params("id")
		if e := s.Repo.UpdatePlan(c.Context(), p, plan); e != nil {
			return e
		}
		return c.JSON(plan)
	})

	r.Post("/plans/:id/delete", func(c fiber.Ctx) error {
		p, e := manage(c)
		if e != nil {
			return e
		}
		if !security.ValidID(c.Params("id")) {
			return domain.ErrInvalid
		}
		if e := s.Repo.DeletePlan(c.Context(), p, c.Params("id")); e != nil {
			return e
		}
		return c.SendStatus(204)
	})

	// A log bound to a plan is how a round is closed: the repository advances the plan in the same transaction.
	r.Post("/assets/:kind/:id/logs", func(c fiber.Ctx) error {
		p, e := manage(c)
		if e != nil {
			return e
		}
		kind, id, e := target(c)
		if e != nil {
			return e
		}
		var in struct {
			PlanID      *string  `json:"plan_id"`
			Kind        string   `json:"kind"`
			Title       string   `json:"title"`
			Detail      string   `json:"detail"`
			PerformedAt string   `json:"performed_at"`
			PerformedBy string   `json:"performed_by"`
			Cost        *float64 `json:"cost"`
		}
		if e := body(c, &in, 8192); e != nil {
			return e
		}
		title, titleOK := assetText(in.Title, assetNameMax)
		detail, detailOK := assetText(in.Detail, assetDetailMax)
		by, byOK := assetText(in.PerformedBy, assetNameMax)
		day, dayOK := assetDate(in.PerformedAt)
		if !titleOK || title == "" || !detailOK || !byOK || !dayOK || day == "" || !slices.Contains(domain.LogKinds, in.Kind) {
			return domain.ErrInvalid
		}
		if in.Cost != nil && (*in.Cost < 0 || *in.Cost > 1e10) {
			return domain.ErrInvalid
		}
		if in.PlanID != nil && !security.ValidID(*in.PlanID) {
			return domain.ErrInvalid
		}
		performed, _ := time.Parse("2006-01-02", day)
		log := domain.MaintenanceLog{ID: uuid.NewString(), AssetKind: kind, AssetID: id, PlanID: in.PlanID, Kind: in.Kind,
			Title: title, Detail: detail, PerformedAt: performed.UTC(), PerformedBy: by, Cost: in.Cost}
		if e := s.Repo.AddLog(c.Context(), p, log); e != nil {
			return e
		}
		out, e := s.Repo.GetAsset(c.Context(), p, kind, id)
		if e != nil {
			return e
		}
		return c.Status(201).JSON(out)
	})
}
