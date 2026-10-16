package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"strings"
	"testing"
	"time"

	"aether/backend/internal/app"
	"aether/backend/internal/domain"
	"aether/backend/internal/ports"
)

func TestCSVCellNeutralisesFormulas(t *testing.T) {
	for in, want := range map[string]string{
		"=SUM(A1)": "'=SUM(A1)", "+1": "'+1", "-1": "'-1", "@x": "'@x", "\t=1": "'\t=1", "\r=1": "'\r=1",
		"Fridge A": "Fridge A", "": "", " =1": " =1",
	} {
		if got := csvCell(in); got != want {
			t.Fatalf("%q: %q, want %q", in, got, want)
		}
	}
}

// streamRepo answers StreamReport with fixed rows, then an error when fail is set; every other method is unused.
type streamRepo struct {
	ports.Repository
	rows int
	fail error
}

func (r streamRepo) StreamReport(_ context.Context, _ domain.Principal, sensors []domain.ReportSensor, _, _ time.Time, _ string, emit func(int, domain.ReportRow) error) error {
	v := 4.5
	for i := 0; i < r.rows; i++ {
		if e := emit(i%len(sensors), domain.ReportRow{At: time.Unix(1_800_000_000, 0), Temperature: &v}); e != nil {
			return e
		}
	}
	return r.fail
}

func streamed(t *testing.T, repo streamRepo) ([][]string, int, error) {
	t.Helper()
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)
	refreshes := 0
	infos := []domain.ReportSensorInfo{{GatewayID: "g", ExternalID: "a", Name: "A"}, {GatewayID: "g", ExternalID: "b", Name: "B"}}
	e := streamReportCSV(context.Background(), &app.Service{Repo: repo}, domain.Principal{}, infos, time.Now(), time.Now(), "raw", w, func() { refreshes++ })
	r := csv.NewReader(strings.NewReader(strings.TrimPrefix(buf.String(), "\ufeff")))
	r.FieldsPerRecord = -1
	rows, perr := r.ReadAll()
	if perr != nil {
		t.Fatal(perr)
	}
	return rows, refreshes, e
}

// A whole file ends with #END and its row count; a stream that failed part way ends with #INCOMPLETE, so a short
// file never looks complete. The deadline moves at the start, at every change of sensor and at the end.
func TestReportCSVMarksCompleteAndShortFiles(t *testing.T) {
	rows, refreshes, e := streamed(t, streamRepo{rows: 3})
	if e != nil || len(rows) != 5 || strings.Join(rows[4], ",") != "#END,3" {
		t.Fatalf("whole file: %v %v", rows, e)
	}
	if refreshes != 5 { // start, sensor a, b, a, end
		t.Fatalf("deadline moves: %d", refreshes)
	}
	failed := errors.New("connection reset")
	rows, _, e = streamed(t, streamRepo{rows: 2, fail: failed})
	if !errors.Is(e, failed) || strings.Join(rows[len(rows)-1], ",") != "#INCOMPLETE,stream_failed" || len(rows) != 4 {
		t.Fatalf("short file: %v %v", rows, e)
	}
	rows, _, _ = streamed(t, streamRepo{rows: 1, fail: context.DeadlineExceeded})
	if strings.Join(rows[len(rows)-1], ",") != "#INCOMPLETE,timeout" {
		t.Fatalf("timed out: %v", rows)
	}
}

func TestRawExportBudget(t *testing.T) {
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		sensors int
		span    time.Duration
		ok      bool
	}{{4, 30 * 24 * time.Hour, true}, {4, 31 * 24 * time.Hour, false}, {50, 2 * 24 * time.Hour, true}, {50, 2*24*time.Hour + time.Minute, false}, {120, time.Hour, true}} {
		e := domain.ValidRawBudget(c.sensors, from, from.Add(c.span))
		if (e == nil) != c.ok {
			t.Fatalf("%d sensors over %s: %v", c.sensors, c.span, e)
		}
	}
}
