package reportjobs

import (
	"aether/backend/internal/alerts"
	"aether/backend/internal/domain"
	"aether/backend/internal/reports"
	"net/url"
	"strings"
	"testing"
	"time"
)

func f(v float64) *float64 { return &v }

func report(sensors int, name string) Report {
	loc := reports.Bangkok()
	from := time.Date(2026, 10, 12, 0, 0, 0, 0, loc)
	r := Report{Run: domain.ReportRun{ID: "run", ReportID: "11111111-2222-3333-4444-555555555555", PeriodFrom: from, PeriodTo: from.AddDate(0, 0, 7), Trigger: "schedule"},
		Name: "ห้องเย็น <วัคซีน>", Workspace: "โรงพยาบาล", Temp: reports.Range{Min: f(2), Max: f(8)}, Hum: reports.Range{Max: f(70)}, Origin: "https://aether.example.test/"}
	for i := 0; i < sensors; i++ {
		r.Sensors = append(r.Sensors, domain.ReportSensorInfo{GatewayID: "g", ExternalID: "e", Name: name, GatewayName: "GW ชั้น 2"})
		r.Summaries = append(r.Summaries, reports.Summary{Coverage: 98.6, Temperature: &reports.Stats{Min: 3.1, Max: 9.4, Avg: 5.2},
			MinutesOutOfRange: reports.Minutes{Temperature: 75}, Excursions: []reports.Excursion{{}, {}}})
	}
	return r
}

// The LINE text fits one message (5000 characters) however many sensors there are, says how many were left out,
// and always ends with the link to the same days on the reports page.
func TestLineTextCapAndLink(t *testing.T) {
	r := report(50, strings.Repeat("ตู้เย็นเก็บวัคซีน 🧊 ", 8)) // the emoji counts twice for LINE
	text := r.Text()
	if n := alerts.LineLength(text); n > alerts.MaxLineText {
		t.Fatalf("text is %d characters", n)
	}
	lines := strings.Split(text, "\n")
	link := strings.TrimPrefix(lines[len(lines)-1], "ดูรายงาน: ")
	u, e := url.Parse(link)
	if e != nil || u.Host != "aether.example.test" || !strings.HasPrefix(u.RawQuery, "reports&") {
		t.Fatalf("link: %q %v", link, e)
	}
	q := u.Query()
	if q.Get("report") != r.Run.ReportID || q.Get("from") != "2026-10-12" || q.Get("to") != "2026-10-18" {
		t.Fatalf("link query: %v", q)
	}
	if !strings.Contains(text, "…และอีก") || !strings.Contains(text, "12 ต.ค. 2569 – 18 ต.ค. 2569") || !strings.Contains(text, "นอกเกณฑ์ 1 ชม. 15 นาที (2 ครั้ง)") {
		t.Fatalf("text: %s", text)
	}
	// A short report lists every sensor and needs no "and N more".
	short := report(2, "ห้องเย็น 1").Text()
	if strings.Contains(short, "…และอีก") || strings.Count(short, "• ห้องเย็น 1") != 2 || !strings.Contains(short, "เกณฑ์: อุณหภูมิ 2.0–8.0 °C · ความชื้น ไม่เกิน 70.0 %") {
		t.Fatalf("short text: %s", short)
	}
}

// The email HTML escapes names, and the delivery carries the CSV as an attachment named after the period; a CSV over
// the size bound is left out and the HTML says so.
func TestEmailContent(t *testing.T) {
	r := report(1, `<script>x</script>`)
	r.CSV = []byte("\ufeffเวลา\r\n#END,0\r\n")
	d := r.Delivery(time.Now())
	if strings.Contains(d.HTML, "<script>") || !strings.Contains(d.HTML, "&lt;script&gt;") || !strings.Contains(d.HTML, "ห้องเย็น &lt;วัคซีน&gt;") {
		t.Fatalf("html not escaped: %s", d.HTML)
	}
	if d.Attachment == nil || d.Attachment.Name != "aether-report-20261012-20261018-1h.csv" || d.Subject != "รายงาน ห้องเย็น <วัคซีน> · 12 ต.ค. 2569 – 18 ต.ค. 2569" {
		t.Fatalf("delivery: %+v %q", d.Attachment, d.Subject)
	}
	if d.Webhook.(map[string]any)["schema"] != "aether.report.v1" || d.Event != "report" {
		t.Fatalf("webhook: %v", d.Webhook)
	}
	r.CSV = nil
	if d := r.Delivery(time.Now()); d.Attachment != nil || !strings.Contains(d.HTML, "ใหญ่เกินกว่าจะแนบ") {
		t.Fatal("an oversized CSV must be left out and said so")
	}
	// A one-day period reads as one day.
	day := report(1, "x")
	day.Run.PeriodTo = day.Run.PeriodFrom.AddDate(0, 0, 1)
	if !strings.HasSuffix(day.Subject(), "· 12 ต.ค. 2569") {
		t.Fatalf("one day: %s", day.Subject())
	}
}
