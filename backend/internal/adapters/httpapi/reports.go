package httpapi

import (
	"aether/backend/internal/app"
	"aether/backend/internal/domain"
	"aether/backend/internal/reports"
	"aether/backend/internal/security"
	"bufio"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

// "รายงาน" (docs/platform/reports.md): CSV export of fixed sensors and summary reports with excursions. Export and
// summary are reads sent as POST (the sensor list is a body; CORS allows GET and POST only), so a member with read
// access to the reports module may run them (accessGate, readPost). Saved reports are written by owner / admin /
// operator. Wearables and roaming tags are refused by the repository (personal_sensor): they belong to the privacy export.

const reportTimeLayout = "2006-01-02 15:04:05"

// streamRefreshEvery is the longest a streaming export goes without moving its write deadline (streamWriteWindow).
const streamRefreshEvery = 5 * time.Second

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

// reportNumber formats a CSV number; an absent value is an empty cell. Numbers never go through csvCell: a negative
// temperature must stay a number, and a cell holding only a number cannot be a formula.
func reportNumber(v *float64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatFloat(*v, 'f', -1, 64)
}

type reportSummaryItem struct {
	domain.ReportSensorInfo
	reports.Summary
}

type reportOmitted struct {
	domain.ReportSensor
	Reason string `json:"reason"`
}

func reportRoutes(r fiber.Router, s *app.Service, reads *readLog) {
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
		omitted := []reportOmitted{}
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
				case e == nil:
					infos = append(infos, got[0])
				case errors.As(e, &reason):
					omitted = append(omitted, reportOmitted{ReportSensor: sensor, Reason: reason.Reason})
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
		out, e := s.Repo.SaveReportDefinition(c.Context(), p, domain.ReportDefinition{ID: id, ProjectID: in.ProjectID, Name: name, Kind: "environment", Sensors: sensors,
			TempMin: in.TempMin, TempMax: in.TempMax, HumMin: in.HumMin, HumMax: in.HumMax}, create)
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

// streamReportCSV writes the header, every sensor's rows and a last row that says whether the file is whole:
// `#END,<data rows>` when it is, `#INCOMPLETE,<reason>` when reading stopped part way (reason `timeout` when the
// 30-minute budget ran out, `stream_failed` otherwise). The status was sent with the first byte, so this row is how a
// short file shows itself. refresh moves the write deadline forward before the first row of every sensor and whenever
// more than streamRefreshEvery passed since it last did, so neither a slow query nor a slow reader cuts the file.
func streamReportCSV(ctx context.Context, s *app.Service, p domain.Principal, infos []domain.ReportSensorInfo, from, to time.Time, resolution string, w *bufio.Writer, refresh func()) error {
	last := time.Now()
	touch := func(force bool) {
		if force || time.Since(last) > streamRefreshEvery {
			refresh()
			last = time.Now()
		}
	}
	touch(true)
	if _, e := w.WriteString("\ufeff"); e != nil {
		return e
	}
	cw := csv.NewWriter(w)
	head := []string{"เวลา", "เซนเซอร์", "gateway", "external_id"}
	if resolution == "raw" {
		head = append(head, "temperature", "humidity", "battery")
	} else {
		head = append(head, "n", "t_avg", "t_min", "t_max", "h_avg", "h_min", "h_max")
	}
	if e := cw.Write(head); e != nil {
		return e
	}
	sensors := make([]domain.ReportSensor, len(infos))
	for i, info := range infos {
		sensors[i] = domain.ReportSensor{GatewayID: info.GatewayID, ExternalID: info.ExternalID}
	}
	loc, rows, current := reports.Bangkok(), 0, -1
	e := s.Repo.StreamReport(ctx, p, sensors, from, to, resolution, func(i int, row domain.ReportRow) error {
		touch(i != current)
		current = i
		info := infos[i]
		cells := []string{row.At.In(loc).Format(reportTimeLayout), csvCell(info.Name), csvCell(info.GatewayName), csvCell(info.ExternalID)}
		if resolution == "raw" {
			cells = append(cells, reportNumber(row.Temperature), reportNumber(row.Humidity), reportNumber(row.Battery))
		} else {
			cells = append(cells, strconv.Itoa(row.N), reportNumber(row.TAvg), reportNumber(row.TMin), reportNumber(row.TMax), reportNumber(row.HAvg), reportNumber(row.HMin), reportNumber(row.HMax))
		}
		if e := cw.Write(cells); e != nil {
			return e
		}
		if rows++; rows%refreshRows == 0 {
			cw.Flush()
			return cw.Error()
		}
		return nil
	})
	touch(true)
	marker := []string{"#END", strconv.Itoa(rows)}
	if e != nil {
		reason := "stream_failed"
		if errors.Is(e, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			reason = "timeout"
		}
		marker = []string{"#INCOMPLETE", reason}
	}
	// A broken connection fails these writes too; the error that matters is the stream's.
	werr := cw.Write(marker)
	cw.Flush()
	if werr == nil {
		werr = cw.Error()
	}
	if ferr := w.Flush(); werr == nil {
		werr = ferr
	}
	if e != nil {
		return e
	}
	return werr
}
