package reports

import (
	"aether/backend/internal/domain"
	"bufio"
	"context"
	"encoding/csv"
	"errors"
	"strconv"
	"strings"
	"time"
)

// The CSV of a report (docs/platform/reports.md, "CSV"): the HTTP export streams it, scheduled delivery attaches it.

const csvTimeLayout = "2006-01-02 15:04:05"

// csvRefreshEvery is the longest WriteCSV goes without calling refresh (a stream's write deadline).
const csvRefreshEvery = 5 * time.Second

// csvFlushRows is how many rows WriteCSV writes between flushes of the CSV writer.
const csvFlushRows = 1000

// CSVCell neutralises formula injection: Excel and Sheets execute a cell that starts with = + - @, and also one that
// starts with a tab or carriage return followed by one of them (OWASP CSV injection).
func CSVCell(s string) string {
	if s != "" && strings.ContainsAny(s[:1], "=+-@\t\r") {
		return "'" + s
	}
	return s
}

// csvNumber formats a CSV number; an absent value is an empty cell. Numbers never go through CSVCell: a negative
// temperature must stay a number, and a cell holding only a number cannot be a formula.
func csvNumber(v *float64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatFloat(*v, 'f', -1, 64)
}

// WriteCSV writes the header, every sensor's rows and a last row that says whether the file is whole:
// `#END,<data rows>` when it is, `#INCOMPLETE,<reason>` when reading stopped part way (reason `timeout` when the
// 30-minute budget ran out, `stream_failed` otherwise). The status was sent with the first byte, so this row is how a
// short file shows itself. refresh moves the write deadline forward before the first row of every sensor and whenever
// more than csvRefreshEvery passed since it last did, so neither a slow query nor a slow reader cuts the file.
// stream hands every row to emit with its sensor's index in infos.
func WriteCSV(ctx context.Context, w *bufio.Writer, infos []domain.ReportSensorInfo, resolution string, stream func(emit func(int, domain.ReportRow) error) error, refresh func()) error {
	last := time.Now()
	touch := func(force bool) {
		if force || time.Since(last) > csvRefreshEvery {
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
	loc, rows, current := Bangkok(), 0, -1
	e := stream(func(i int, row domain.ReportRow) error {
		touch(i != current)
		current = i
		info := infos[i]
		cells := []string{row.At.In(loc).Format(csvTimeLayout), CSVCell(info.Name), CSVCell(info.GatewayName), CSVCell(info.ExternalID)}
		if resolution == "raw" {
			cells = append(cells, csvNumber(row.Temperature), csvNumber(row.Humidity), csvNumber(row.Battery))
		} else {
			cells = append(cells, strconv.Itoa(row.N), csvNumber(row.TAvg), csvNumber(row.TMin), csvNumber(row.TMax), csvNumber(row.HAvg), csvNumber(row.HMin), csvNumber(row.HMax))
		}
		if e := cw.Write(cells); e != nil {
			return e
		}
		if rows++; rows%csvFlushRows == 0 {
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
