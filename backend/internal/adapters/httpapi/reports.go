package httpapi

import (
	"aether/backend/internal/alerts"
	"aether/backend/internal/app"
	"aether/backend/internal/domain"
	"aether/backend/internal/reports"
	"aether/backend/internal/security"
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

// "รายงาน" (docs/platform/reports.md): CSV export of fixed sensors and summary reports with excursions. Export and
// summary are reads sent as POST (the sensor list is a body; CORS allows GET and POST only), so a member with read
// access to the reports module may run them (accessGate, readPost). Saved reports are written by owner / admin /
// operator. Wearables and roaming tags are refused by the repository (personal_sensor): they belong to the privacy export.

// reportWindow parses an RFC 3339 range and checks it (domain.ValidReportWindow).
func reportWindow(from, to string) (time.Time, time.Time, error) {
	f, e1 := time.Parse(time.RFC3339, from)
	t, e2 := time.Parse(time.RFC3339, to)
	if e1 != nil || e2 != nil {
		return time.Time{}, time.Time{}, domain.Because(domain.ErrInvalid, "invalid_range")
	}
	return f, t, domain.ValidReportWindow(f, t)
}

func reportRanges(temp, hum reports.Range) error {
	if e := domain.ValidReportRange(temp.Min, temp.Max, domain.ReportTempLow, domain.ReportTempHigh); e != nil {
		return e
	}
	return domain.ValidReportRange(hum.Min, hum.Max, domain.ReportHumLow, domain.ReportHumHigh)
}

type reportSummaryItem struct {
	domain.ReportSensorInfo
	reports.Summary
}

func reportRoutes(r fiber.Router, s *app.Service, reads *readLog, sender *alerts.Sender) {
	exports, summaries := newExportSlots(), newExportSlots()
	principal := func(c fiber.Ctx) domain.Principal { return c.Locals("principal").(domain.Principal) }
	writer := func(c fiber.Ctx) (domain.Principal, error) {
		p := principal(c)
		if !p.CanOperate() {
			return p, domain.ErrForbidden
		}
		return p, nil
	}

	r.Get("/reports/sensors", func(c fiber.Ctx) error {
		out, e := s.Repo.ReportSensors(c.Context(), principal(c))
		if e != nil {
			return e
		}
		return c.JSON(fiber.Map{"items": out, "max_sensors": domain.ReportMaxSensors, "max_days": int(domain.ReportMaxWindow / (24 * time.Hour))})
	})

	// CSV with a BOM (Excel opens the Thai headers), streamed: a month of raw samples does not fit the 15 s write
	// timeout or memory. Everything that can refuse the request (limits, sensors, the slot, the read-access log)
	// happens before the first byte; a failure while streaming leaves the file short and is logged.
	r.Post("/reports/export", func(c fiber.Ctx) error {
		var in struct {
			Sensors    []domain.ReportSensor `json:"sensors"`
			From       string                `json:"from"`
			To         string                `json:"to"`
			Resolution string                `json:"resolution"`
		}
		if e := body(c, &in, 16*1024); e != nil {
			return e
		}
		p := principal(c)
		from, to, e := reportWindow(in.From, in.To)
		if e != nil {
			return e
		}
		if !slices.Contains(domain.ReportResolutions, in.Resolution) {
			return domain.Because(domain.ErrInvalid, "invalid_resolution")
		}
		sensors, e := domain.NormaliseReportSensors(in.Sensors, security.ValidID)
		if e != nil {
			return e
		}
		if in.Resolution == "raw" {
			if e := domain.ValidRawBudget(len(sensors), from, to); e != nil {
				return e
			}
		}
		release, ok := exports.take(p.TenantID)
		if !ok {
			return domain.ErrRateLimited
		}
		streaming := false
		defer func() {
			if !streaming {
				release()
			}
		}()
		infos, e := s.Repo.ResolveReportSensors(c.Context(), p, sensors)
		if e != nil {
			return e
		}
		// Like every export, logged on its own row; the subject says how much was taken.
		read := domain.AccessRead{Resource: "reports_export", SubjectKind: "sensor_history", SubjectID: fmt.Sprintf("%d sensors, %s", len(infos), in.Resolution), RequestID: requestID(c), ClientIP: c.IP()}
		if e := reads.write(c.Context(), p, read, true); e != nil {
			return e
		}
		loc := reports.Bangkok()
		name := fmt.Sprintf("aether-report-%s-%s-%s.csv", from.In(loc).Format("20060102"), to.In(loc).Format("20060102"), in.Resolution)
		c.Set(fiber.HeaderContentDisposition, `attachment; filename="`+name+`"`)
		c.Set(fiber.HeaderContentType, "text/csv; charset=utf-8")
		conn := c.RequestCtx().Conn()
		refresh := func() {
			if conn != nil {
				_ = conn.SetWriteDeadline(time.Now().Add(streamWriteWindow))
			}
		}
		streaming = true
		return c.SendStreamWriter(func(w *bufio.Writer) {
			defer release()
			// This runs on fasthttp's goroutine, outside the recover middleware: a panic would end the process.
			defer func() {
				if r := recover(); r != nil {
					slog.Error("report export stream panicked; file left short", "panic", fmt.Sprint(r))
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			refresh()
			if e := streamReportCSV(ctx, s, p, infos, from, to, in.Resolution, w, refresh); e != nil {
				slog.Error("report export stream failed; file left short", "error", e.Error())
			}
		})
	})

	r.Post("/reports/summary", func(c fiber.Ctx) error {
		var in struct {
			ReportID string                `json:"report_id"`
			Sensors  []domain.ReportSensor `json:"sensors"`
			From     string                `json:"from"`
			To       string                `json:"to"`
			Temp     *reports.Range        `json:"temp"`
			Hum      *reports.Range        `json:"hum"`
		}
		if e := body(c, &in, 16*1024); e != nil {
			return e
		}
		p := principal(c)
		from, to, e := reportWindow(in.From, in.To)
		if e != nil {
			return e
		}
		release, ok := summaries.take(p.TenantID)
		if !ok {
			return domain.ErrRateLimited
		}
		defer release()
		ctx, cancel := exportContext(c)
		defer cancel()
		var temp, hum reports.Range
		var infos []domain.ReportSensorInfo
		omitted := []domain.ReportOmitted{}
		var report fiber.Map
		if in.ReportID != "" {
			if len(in.Sensors) > 0 || in.Temp != nil || in.Hum != nil {
				return domain.Because(domain.ErrInvalid, "report_or_sensors")
			}
			if !security.ValidID(in.ReportID) {
				return domain.ErrInvalid
			}
			def, e := s.Repo.GetReportDefinition(ctx, p, in.ReportID)
			if e != nil {
				return e
			}
			report = fiber.Map{"id": def.ID, "name": def.Name, "project_id": def.ProjectID}
			temp, hum = reports.Range{Min: def.TempMin, Max: def.TempMax}, reports.Range{Min: def.HumMin, Max: def.HumMax}
			// A saved report outlives its sensors: one that is gone, out of the caller's projects or no longer a
			// fixed sensor is left out and named, and the rest still run.
			for _, sensor := range def.Sensors {
				got, e := s.Repo.ResolveReportSensors(ctx, p, []domain.ReportSensor{sensor})
				var reason domain.ReasonError
				switch {
				case e == nil && def.ProjectID != nil && (got[0].ProjectID == nil || !strings.EqualFold(*got[0].ProjectID, *def.ProjectID)):
					// As when it is sent: a project report shows only its project's sensors.
					omitted = append(omitted, domain.ReportOmitted{ReportSensor: sensor, Reason: "sensor_outside_project"})
				case e == nil:
					infos = append(infos, got[0])
				case errors.As(e, &reason):
					omitted = append(omitted, domain.ReportOmitted{ReportSensor: sensor, Reason: reason.Reason})
				default:
					return e
				}
			}
		} else {
			if in.Temp != nil {
				temp = *in.Temp
			}
			if in.Hum != nil {
				hum = *in.Hum
			}
			sensors, e := domain.NormaliseReportSensors(in.Sensors, security.ValidID)
			if e != nil {
				return e
			}
			if infos, e = s.Repo.ResolveReportSensors(ctx, p, sensors); e != nil {
				return e
			}
		}
		if e := reportRanges(temp, hum); e != nil {
			return e
		}
		sensors := make([]domain.ReportSensor, len(infos))
		for i, info := range infos {
			sensors[i] = domain.ReportSensor{GatewayID: info.GatewayID, ExternalID: info.ExternalID}
		}
		buckets, workspace, e := s.Repo.ReportBuckets(ctx, p, sensors, from, to)
		if e != nil {
			return e
		}
		now, loc := time.Now(), reports.Bangkok()
		items := make([]reportSummaryItem, len(infos))
		for i, info := range infos {
			items[i] = reportSummaryItem{ReportSensorInfo: info, Summary: reports.Summarize(buckets[i], from, to, now, temp, hum, loc)}
		}
		return c.JSON(fiber.Map{"from": from.UTC(), "to": to.UTC(), "generated_at": now.UTC(), "time_zone": "Asia/Bangkok", "workspace": workspace,
			"temp": temp, "hum": hum, "report": report, "sensors": items, "omitted": omitted})
	})

	r.Get("/reports/definitions", func(c fiber.Ctx) error {
		out, e := s.Repo.ListReportDefinitions(c.Context(), principal(c))
		if e != nil {
			return e
		}
		return c.JSON(fiber.Map{"items": out, "limit": domain.ReportMaxDefinitions})
	})
	r.Get("/reports/definitions/:id", func(c fiber.Ctx) error {
		if !security.ValidID(c.Params("id")) {
			return domain.ErrInvalid
		}
		out, e := s.Repo.GetReportDefinition(c.Context(), principal(c), c.Params("id"))
		if e != nil {
			return e
		}
		return c.JSON(out)
	})
	save := func(c fiber.Ctx, id string, create bool) error {
		var in struct {
			Name      string                `json:"name"`
			ProjectID *string               `json:"project_id"`
			Sensors   []domain.ReportSensor `json:"sensors"`
			TempMin   *float64              `json:"temp_min"`
			TempMax   *float64              `json:"temp_max"`
			HumMin    *float64              `json:"hum_min"`
			HumMax    *float64              `json:"hum_max"`
			// The schedule replaces the stored one like every other field: leaving it out means "none".
			Schedule        string   `json:"schedule"`
			ScheduleWeekday *int     `json:"schedule_weekday"`
			ScheduleTime    *string  `json:"schedule_time"`
			Channels        []string `json:"channels"`
		}
		if e := body(c, &in, 16*1024); e != nil {
			return e
		}
		p, e := writer(c)
		if e != nil {
			return e
		}
		name := strings.TrimSpace(in.Name)
		if name == "" || len([]rune(name)) > domain.ReportNameMax || strings.ContainsAny(name, "\x00\r\n") {
			return domain.Because(domain.ErrInvalid, "invalid_name")
		}
		if in.ProjectID != nil {
			if !security.ValidID(*in.ProjectID) {
				return domain.ErrInvalid
			}
			*in.ProjectID = strings.ToLower(*in.ProjectID)
		}
		sensors, e := domain.NormaliseReportSensors(in.Sensors, security.ValidID)
		if e != nil {
			return e
		}
		if e := reportRanges(reports.Range{Min: in.TempMin, Max: in.TempMax}, reports.Range{Min: in.HumMin, Max: in.HumMax}); e != nil {
			return e
		}
		def := domain.ReportDefinition{ID: id, ProjectID: in.ProjectID, Name: name, Kind: "environment", Sensors: sensors,
			TempMin: in.TempMin, TempMax: in.TempMax, HumMin: in.HumMin, HumMax: in.HumMax}
		if e := reportSchedule(&def, in.Schedule, in.ScheduleWeekday, in.ScheduleTime, in.Channels, time.Now()); e != nil {
			return e
		}
		out, e := s.Repo.SaveReportDefinition(c.Context(), p, def, create)
		if e != nil {
			return e
		}
		if create {
			return c.Status(201).JSON(out)
		}
		return c.JSON(out)
	}
	r.Post("/reports/definitions", func(c fiber.Ctx) error { return save(c, uuid.NewString(), true) })
	r.Post("/reports/definitions/:id/update", func(c fiber.Ctx) error {
		if !security.ValidID(c.Params("id")) {
			return domain.ErrInvalid
		}
		return save(c, c.Params("id"), false)
	})
	// Send a saved report now to its channels, for [from, to) or by default yesterday (Bangkok). The worker delivers
	// it within its next pass; 429 too_many_sends past domain.ReportManualPerHour per workspace and hour.
	r.Post("/reports/definitions/:id/send", func(c fiber.Ctx) error {
		var in struct {
			From string `json:"from"`
			To   string `json:"to"`
		}
		if e := body(c, &in, 1024); e != nil {
			return e
		}
		p, e := writer(c)
		if e != nil {
			return e
		}
		if !security.ValidID(c.Params("id")) {
			return domain.ErrInvalid
		}
		from, to := reports.LastFullDay(time.Now(), reports.Bangkok())
		if in.From != "" || in.To != "" {
			if from, to, e = reportWindow(in.From, in.To); e != nil {
				return e
			}
		}
		run, e := s.Repo.SendReportNow(c.Context(), p, strings.ToLower(c.Params("id")), from, to)
		if e != nil {
			return e
		}
		return c.Status(202).JSON(run)
	})
	r.Get("/reports/definitions/:id/runs", func(c fiber.Ctx) error {
		if !security.ValidID(c.Params("id")) {
			return domain.ErrInvalid
		}
		out, e := s.Repo.ListReportRuns(c.Context(), principal(c), strings.ToLower(c.Params("id")))
		if e != nil {
			return e
		}
		return c.JSON(fiber.Map{"items": out})
	})
	// The channels a report may go to, for the editor: names and kinds only (configs are credentials in practice),
	// and whether this server can send email at all. Readable with the reports module, not only the alerts one.
	r.Get("/reports/channels", func(c fiber.Ctx) error {
		list, e := s.Repo.ListChannels(c.Context(), principal(c))
		if e != nil {
			return e
		}
		items := make([]fiber.Map, 0, len(list))
		for _, ch := range list {
			items = append(items, fiber.Map{"id": ch.ID, "name": ch.Name, "kind": ch.Kind, "enabled": ch.Enabled, "project_id": ch.ProjectID})
		}
		return c.JSON(fiber.Map{"items": items, "email_available": sender.SMTP.Configured()})
	})
	r.Post("/reports/definitions/:id/delete", func(c fiber.Ctx) error {
		p, e := writer(c)
		if e != nil {
			return e
		}
		if !security.ValidID(c.Params("id")) {
			return domain.ErrInvalid
		}
		if e := s.Repo.DeleteReportDefinition(c.Context(), p, c.Params("id")); e != nil {
			return e
		}
		return c.SendStatus(204)
	})
}

// streamReportCSV writes the CSV of an export (reports.WriteCSV) from the repository's rows.
func streamReportCSV(ctx context.Context, s *app.Service, p domain.Principal, infos []domain.ReportSensorInfo, from, to time.Time, resolution string, w *bufio.Writer, refresh func()) error {
	sensors := make([]domain.ReportSensor, len(infos))
	for i, info := range infos {
		sensors[i] = domain.ReportSensor{GatewayID: info.GatewayID, ExternalID: info.ExternalID}
	}
	return reports.WriteCSV(ctx, w, infos, resolution, func(emit func(int, domain.ReportRow) error) error {
		return s.Repo.StreamReport(ctx, p, sensors, from, to, resolution, emit)
	}, refresh)
}

// reportSchedule checks and sets a report's schedule: none, daily at "HH:MM" or weekly on an ISO weekday at "HH:MM"
// (Bangkok), with up to domain.ReportMaxChannels channels; a schedule needs at least one. next_run_at is its next
// occurrence after now. Channels exist, are enabled and fit the report's project: the repository checks that.
func reportSchedule(d *domain.ReportDefinition, kind string, weekday *int, at *string, channels []string, now time.Time) error {
	if kind == "" {
		kind = "none"
	}
	s := reports.Schedule{Kind: kind}
	if weekday != nil {
		s.Weekday = *weekday
	}
	if at != nil {
		s.Time = *at
	}
	if !s.Valid() || (kind == "none" && (weekday != nil || at != nil)) {
		return domain.Because(domain.ErrInvalid, "invalid_schedule")
	}
	if len(channels) > domain.ReportMaxChannels {
		return domain.Because(domain.ErrInvalid, "invalid_channels")
	}
	seen := map[string]bool{}
	d.Channels = []string{}
	for _, ch := range channels {
		ch = strings.ToLower(ch)
		if !security.ValidID(ch) || seen[ch] {
			return domain.Because(domain.ErrInvalid, "invalid_channels")
		}
		seen[ch] = true
		d.Channels = append(d.Channels, ch)
	}
	d.Schedule, d.ScheduleWeekday, d.ScheduleTime, d.NextRunAt = kind, nil, nil, nil
	if kind == "none" {
		return nil
	}
	if len(d.Channels) == 0 {
		return domain.Because(domain.ErrInvalid, "no_channels")
	}
	next := s.Next(now, reports.Bangkok())
	d.ScheduleTime, d.NextRunAt = at, &next
	if kind == "weekly" {
		d.ScheduleWeekday = weekday
	}
	return nil
}
