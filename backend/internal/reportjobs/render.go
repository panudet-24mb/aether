// Package reportjobs sends saved reports on their schedule (docs/platform/reports.md, "Scheduled delivery"): the
// worker queues due runs, then renders each run once and delivers it to every channel through the alerts Sender.
package reportjobs

import (
	"aether/backend/internal/alerts"
	"aether/backend/internal/domain"
	"aether/backend/internal/reports"
	"fmt"
	"html"
	"net/url"
	"strings"
	"time"
)

// MaxAttachment bounds the CSV attached to an email; a larger one is left out and the message says so. An hourly CSV
// of 50 sensors over a week is about 8,400 rows, under 1 MB.
const MaxAttachment = 4 << 20

// Report is one run's numbers, ready to render.
type Report struct {
	Run       domain.ReportRun
	Name      string
	Workspace string
	Temp, Hum reports.Range
	Sensors   []domain.ReportSensorInfo
	Summaries []reports.Summary
	Omitted   []domain.ReportOmitted
	CSV       []byte
	Origin    string // APP_ORIGIN, for the link to the report
}

var thaiMonths = []string{"ม.ค.", "ก.พ.", "มี.ค.", "เม.ย.", "พ.ค.", "มิ.ย.", "ก.ค.", "ส.ค.", "ก.ย.", "ต.ค.", "พ.ย.", "ธ.ค."}

// thaiDay is a Bangkok calendar day as Thai readers write it: 15 ต.ค. 2569.
func thaiDay(t time.Time) string {
	l := t.In(reports.Bangkok())
	return fmt.Sprintf("%d %s %d", l.Day(), thaiMonths[l.Month()-1], l.Year()+543)
}

// days are the first and last Bangkok calendar day of the run's period.
func (r Report) days() (time.Time, time.Time) {
	return r.Run.PeriodFrom, r.Run.PeriodTo.Add(-time.Second)
}

func (r Report) periodLabel() string {
	first, last := r.days()
	if a, b := thaiDay(first), thaiDay(last); a != b {
		return a + " – " + b
	}
	return thaiDay(first)
}

// Link opens the report on the reports page for the same days (frontend: ?reports&report=&from=&to=).
func (r Report) Link() string {
	first, last := r.days()
	loc := reports.Bangkok()
	q := url.Values{"report": {r.Run.ReportID}, "from": {first.In(loc).Format("2006-01-02")}, "to": {last.In(loc).Format("2006-01-02")}}
	return strings.TrimRight(r.Origin, "/") + "/?reports&" + q.Encode()
}

func num(v float64) string { return fmt.Sprintf("%.1f", v) }

func rangeLabel(r reports.Range, unit string) string {
	switch {
	case r.Min == nil && r.Max == nil:
		return "ไม่กำหนด"
	case r.Min != nil && r.Max != nil:
		return fmt.Sprintf("%s–%s %s", num(*r.Min), num(*r.Max), unit)
	case r.Min != nil:
		return fmt.Sprintf("ไม่ต่ำกว่า %s %s", num(*r.Min), unit)
	}
	return fmt.Sprintf("ไม่เกิน %s %s", num(*r.Max), unit)
}

func stats(s *reports.Stats, unit string) string {
	if s == nil {
		return "ไม่มีข้อมูล"
	}
	return fmt.Sprintf("%s–%s %s (เฉลี่ย %s)", num(s.Min), num(s.Max), unit, num(s.Avg))
}

func minutesLabel(m int) string {
	if m < 60 {
		return fmt.Sprintf("%d นาที", m)
	}
	if m%60 == 0 {
		return fmt.Sprintf("%d ชม.", m/60)
	}
	return fmt.Sprintf("%d ชม. %d นาที", m/60, m%60)
}

// sensorLine is one sensor in a text message.
func (r Report) sensorLine(i int) string {
	s, info := r.Summaries[i], r.Sensors[i]
	out := s.MinutesOutOfRange.Temperature + s.MinutesOutOfRange.Humidity
	status := "อยู่ในเกณฑ์"
	if out > 0 {
		status = fmt.Sprintf("นอกเกณฑ์ %s (%d ครั้ง)", minutesLabel(out), len(s.Excursions))
	}
	return fmt.Sprintf("• %s (%s): %s · %s · ครอบคลุม %s%% · %s", info.Name, info.GatewayName, stats(s.Temperature, "°C"), stats(s.Humidity, "%"), num(s.Coverage), status)
}

func (r Report) Subject() string {
	return fmt.Sprintf("รายงาน %s · %s", r.Name, r.periodLabel())
}

// Text is the LINE message and the plain-text part of the email: at most alerts.MaxLineText as LINE counts it
// (UTF-16 code units, alerts.LineLength), sensors
// left out from the end when they do not fit, the link always last.
func (r Report) Text() string {
	head := []string{
		"รายงาน: " + r.Name,
		"ช่วง: " + r.periodLabel() + " (เวลาไทย)",
		"เกณฑ์: อุณหภูมิ " + rangeLabel(r.Temp, "°C") + " · ความชื้น " + rangeLabel(r.Hum, "%"),
	}
	tail := []string{}
	if len(r.Omitted) > 0 {
		tail = append(tail, fmt.Sprintf("ไม่รวม %d เซนเซอร์ (ถอนการลงทะเบียนหรือเป็นข้อมูลส่วนบุคคล)", len(r.Omitted)))
	}
	tail = append(tail, "ดูรายงาน: "+r.Link())
	budget := alerts.MaxLineText
	for _, l := range append(append([]string{}, head...), tail...) {
		budget -= alerts.LineLength(l) + 1
	}
	lines := append([]string{}, head...)
	for i := range r.Sensors {
		line := r.sensorLine(i)
		rest := len(r.Sensors) - i - 1
		more := ""
		if rest > 0 {
			more = fmt.Sprintf("…และอีก %d เซนเซอร์ (ดูในรายงาน)", rest)
		}
		if alerts.LineLength(line)+1+alerts.LineLength(more)+1 > budget {
			lines = append(lines, fmt.Sprintf("…และอีก %d เซนเซอร์ (ดูในรายงาน)", len(r.Sensors)-i))
			break
		}
		lines = append(lines, line)
		budget -= alerts.LineLength(line) + 1
	}
	return strings.Join(append(lines, tail...), "\n")
}

// HTML is the email's summary table.
func (r Report) HTML() string {
	var b strings.Builder
	e := html.EscapeString
	td := func(content string, style string) string {
		return `<td style="padding:6px 10px;border-bottom:1px solid #ddd;text-align:left;` + style + `">` + content + `</td>`
	}
	fmt.Fprintf(&b, `<!doctype html><html><body style="font-family:sans-serif;color:#111"><p style="color:#555;margin:0">AETHER · %s</p><h2 style="margin:4px 0 8px">%s</h2>`, e(r.Workspace), e(r.Name))
	fmt.Fprintf(&b, `<p style="margin:0 0 12px">ช่วง %s (เวลาไทย) · เกณฑ์อุณหภูมิ %s · เกณฑ์ความชื้น %s</p>`, e(r.periodLabel()), e(rangeLabel(r.Temp, "°C")), e(rangeLabel(r.Hum, "%")))
	b.WriteString(`<table style="border-collapse:collapse;font-size:13px"><tr>`)
	for _, h := range []string{"เซนเซอร์", "ครอบคลุม", "อุณหภูมิ ต่ำสุด / สูงสุด / เฉลี่ย (°C)", "ความชื้น ต่ำสุด / สูงสุด / เฉลี่ย (%)", "นอกเกณฑ์", "ครั้ง"} {
		b.WriteString(`<th style="padding:6px 10px;border-bottom:1px solid #999;text-align:left">` + e(h) + `</th>`)
	}
	b.WriteString(`</tr>`)
	triple := func(s *reports.Stats) string {
		if s == nil {
			return "—"
		}
		return num(s.Min) + " / " + num(s.Max) + " / " + num(s.Avg)
	}
	for i, s := range r.Summaries {
		info := r.Sensors[i]
		out := s.MinutesOutOfRange.Temperature + s.MinutesOutOfRange.Humidity
		color := "color:#111"
		if out > 0 {
			color = "color:#b42318"
		}
		b.WriteString(`<tr>` + td(`<strong>`+e(info.Name)+`</strong><br><span style="color:#555">`+e(info.GatewayName)+`</span>`, "") +
			td(num(s.Coverage)+"%", "") + td(triple(s.Temperature), "") + td(triple(s.Humidity), "") + td(e(minutesLabel(out)), color) +
			td(fmt.Sprint(len(s.Excursions)), "") + `</tr>`)
	}
	b.WriteString(`</table>`)
	if len(r.Omitted) > 0 {
		fmt.Fprintf(&b, `<p style="color:#9a6700">ไม่รวม %d เซนเซอร์ (ถอนการลงทะเบียนหรือเป็นข้อมูลส่วนบุคคล)</p>`, len(r.Omitted))
	}
	if r.CSV == nil {
		b.WriteString(`<p style="color:#555">ไฟล์ CSV ใหญ่เกินกว่าจะแนบ ดาวน์โหลดได้จากหน้ารายงาน</p>`)
	} else {
		b.WriteString(`<p style="color:#555">แนบไฟล์ CSV ค่ารายชั่วโมงของช่วงนี้ (บรรทัดสุดท้าย #END คือไฟล์ครบ)</p>`)
	}
	fmt.Fprintf(&b, `<p><a href="%s">ดูรายงานฉบับเต็ม</a></p></body></html>`, e(r.Link()))
	return b.String()
}

type webhookSensor struct {
	domain.ReportSensorInfo
	Coverage          float64         `json:"coverage_pct"`
	Temperature       *reports.Stats  `json:"temperature"`
	Humidity          *reports.Stats  `json:"humidity"`
	MinutesOutOfRange reports.Minutes `json:"minutes_out_of_range"`
	Excursions        int             `json:"excursions"`
}

// Webhook is the JSON a webhook channel receives (schema aether.report.v1).
func (r Report) Webhook(now time.Time) map[string]any {
	sensors := make([]webhookSensor, len(r.Sensors))
	for i, s := range r.Summaries {
		sensors[i] = webhookSensor{ReportSensorInfo: r.Sensors[i], Coverage: s.Coverage, Temperature: s.Temperature, Humidity: s.Humidity, MinutesOutOfRange: s.MinutesOutOfRange, Excursions: len(s.Excursions)}
	}
	return map[string]any{
		"schema": "aether.report.v1", "sent_at": now.UTC(), "workspace": r.Workspace,
		"report": map[string]any{"id": r.Run.ReportID, "name": r.Name, "project_id": r.Run.ProjectID},
		"run":    map[string]any{"id": r.Run.ID, "trigger": r.Run.Trigger, "period_from": r.Run.PeriodFrom.UTC(), "period_to": r.Run.PeriodTo.UTC()},
		"temp":   r.Temp, "hum": r.Hum, "sensors": sensors, "omitted": r.Omitted, "link": r.Link(),
	}
}

// Delivery is the message for every channel kind.
func (r Report) Delivery(now time.Time) alerts.Delivery {
	d := alerts.Delivery{Event: "report", Subject: r.Subject(), Text: r.Text(), HTML: r.HTML(), Webhook: r.Webhook(now)}
	if r.CSV != nil {
		first, last := r.days()
		loc := reports.Bangkok()
		d.Attachment = &alerts.Attachment{Name: fmt.Sprintf("aether-report-%s-%s-1h.csv", first.In(loc).Format("20060102"), last.In(loc).Format("20060102")), ContentType: "text/csv; charset=utf-8", Data: r.CSV}
	}
	return d
}
