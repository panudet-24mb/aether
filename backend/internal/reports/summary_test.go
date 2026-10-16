package reports

import (
	"testing"
	"time"
)

var bkk = time.FixedZone("ICT", 7*3600)

func f(v float64) *float64 { return &v }

// env is a bucket with one temperature and humidity reading (avg = min = max).
func env(at time.Time, t, h float64) Bucket {
	return Bucket{At: at, N: 1, TAvg: f(t), TMin: f(t), TMax: f(t), HAvg: f(h), HMin: f(h), HMax: f(h)}
}

func TestSummarizeExcursionsAndCoverage(t *testing.T) {
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, bkk)
	to := from.Add(2 * time.Hour) // 24 expected slots
	now := to.Add(24 * time.Hour)
	temps := []float64{
		4, 4, 9, 10, 8.5, 4, // a three-bucket high run, peak 10
		4, 4, 4, 1, 4, 4, // one low bucket
		4, 9, 4, 4, 4, 4, // one high bucket
		4, 4, 4, 4, 4, 4,
	}
	var buckets []Bucket
	for i, v := range temps {
		if i == 20 || i == 21 { // a two-bucket gap
			continue
		}
		buckets = append(buckets, env(from.Add(time.Duration(i)*Step), v, 50))
	}
	// Two consecutive highs separated by the gap are separate runs: 19 high, 22 high.
	buckets[19].TMax, buckets[19].TAvg = f(12), f(12)
	buckets[20].TMax, buckets[20].TAvg = f(11), f(11) // slot 22 (index shifted by the gap)
	// A bucket out of range in both directions (min below, max above) counts once toward the minutes.
	buckets[16].TMin, buckets[16].TMax = f(0), f(9)
	// Outside the range: ignored.
	buckets = append(buckets, env(to, 99, 50), env(from.Add(-Step), 99, 50))

	s := Summarize(buckets, from, to, now, Range{Min: f(2), Max: f(8)}, Range{Max: f(60)}, bkk)
	if s.Expected != 24 || s.Buckets != 22 || s.Coverage != 91.7 {
		t.Fatalf("coverage: %d/%d %.1f", s.Buckets, s.Expected, s.Coverage)
	}
	type ex struct {
		dir   string
		start int
		dur   int
		peak  float64
	}
	want := []ex{{"high", 2, 15, 10}, {"low", 9, 5, 1}, {"high", 13, 5, 9}, {"high", 16, 5, 9}, {"low", 16, 5, 0}, {"high", 19, 5, 12}, {"high", 22, 5, 11}}
	if len(s.Excursions) != len(want) {
		t.Fatalf("excursions: %+v", s.Excursions)
	}
	for i, w := range want {
		e := s.Excursions[i]
		start := from.Add(time.Duration(w.start) * Step)
		if e.Metric != "temperature" || e.Direction != w.dir || !e.Start.Equal(start) || e.DurationMin != w.dur || !e.End.Equal(start.Add(time.Duration(w.dur)*time.Minute)) || e.Peak != w.peak {
			t.Fatalf("excursion %d: %+v, want %+v", i, e, w)
		}
	}
	// 3 + 1 + 1 + 1 (both directions, once) + 1 + 1 buckets.
	if s.MinutesOutOfRange.Temperature != 40 || s.MinutesOutOfRange.Humidity != 0 {
		t.Fatalf("minutes out of range: %+v", s.MinutesOutOfRange)
	}
	if s.Temperature == nil || s.Temperature.Min != 0 || s.Temperature.Max != 12 || s.Humidity == nil || s.Humidity.Avg != 50 {
		t.Fatalf("overall: %+v %+v", s.Temperature, s.Humidity)
	}
	if len(s.Series) != 2 || !s.Series[0].At.Equal(from) || *s.Series[0].TMax != 10 || *s.Series[1].TMax != 12 {
		t.Fatalf("hourly series: %+v", s.Series)
	}
}

func TestSummarizeDaysFollowLocalCalendar(t *testing.T) {
	// 23:50 to 00:10 Bangkok: two slots on each local day, though all four share one UTC day.
	from := time.Date(2026, 10, 1, 23, 50, 0, 0, bkk)
	to := from.Add(20 * time.Minute)
	buckets := []Bucket{env(from, 20, 40), env(from.Add(Step), 22, 40), env(from.Add(2*Step), 30, 40)}
	s := Summarize(buckets, from, to, to.Add(time.Hour), Range{}, Range{}, bkk)
	if len(s.Days) != 2 || s.Days[0].Date != "2026-10-01" || s.Days[1].Date != "2026-10-02" {
		t.Fatalf("days: %+v", s.Days)
	}
	if d := s.Days[0]; d.Expected != 2 || d.Buckets != 2 || d.Coverage != 100 || d.Temperature.Avg != 21 || d.Temperature.Max != 22 {
		t.Fatalf("first day: %+v %+v", d, d.Temperature)
	}
	if d := s.Days[1]; d.Expected != 2 || d.Buckets != 1 || d.Coverage != 50 || d.Temperature.Min != 30 {
		t.Fatalf("second day: %+v", d)
	}
	if len(s.Excursions) != 0 || s.MinutesOutOfRange.Temperature != 0 {
		t.Fatalf("no limits, no excursions: %+v", s.Excursions)
	}
}

func TestSummarizeExpectsOnlySettledBuckets(t *testing.T) {
	from := time.Date(2026, 10, 2, 0, 0, 0, 0, bkk)
	to := from.Add(24 * time.Hour)
	now := from.Add(time.Hour + 6*time.Minute) // settled up to 01:00 (01:04 truncated)
	var buckets []Bucket
	for i := 0; i < 13; i++ { // 00:00 .. 01:00, the last one written early
		buckets = append(buckets, env(from.Add(time.Duration(i)*Step), 5, 50))
	}
	s := Summarize(buckets, from, to, now, Range{}, Range{}, bkk)
	if s.Expected != 12 || s.Buckets != 12 || s.Coverage != 100 {
		t.Fatalf("today's report: %d/%d %.1f", s.Buckets, s.Expected, s.Coverage)
	}
	// No data at all: zero coverage, no stats, the day still listed.
	empty := Summarize(nil, from, to, now, Range{Min: f(0)}, Range{}, bkk)
	if empty.Coverage != 0 || empty.Temperature != nil || len(empty.Days) != 1 || empty.Days[0].Temperature != nil || len(empty.Series) != 0 {
		t.Fatalf("empty: %+v", empty)
	}
}

func TestSummarizeCapsExcursions(t *testing.T) {
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	var buckets []Bucket
	for i := 0; i < 2*(MaxExcursions+10); i++ { // alternating out / in: one excursion every other bucket
		v := 4.0
		if i%2 == 0 {
			v = 9
		}
		buckets = append(buckets, env(from.Add(time.Duration(i)*Step), v, 50))
	}
	to := from.Add(time.Duration(len(buckets)) * Step)
	s := Summarize(buckets, from, to, to, Range{Max: f(8)}, Range{}, time.UTC)
	if len(s.Excursions) != MaxExcursions || !s.ExcursionsTruncated || s.MinutesOutOfRange.Temperature != (MaxExcursions+10)*5 {
		t.Fatalf("cap: %d %v %d", len(s.Excursions), s.ExcursionsTruncated, s.MinutesOutOfRange.Temperature)
	}
	// Humidity is out of range in every bucket, as one long run that closes last: it is among the earliest, so the cap
	// keeps it (cutting in closing order would have dropped it).
	h := Summarize(buckets, from, to, to, Range{Max: f(8)}, Range{Max: f(40)}, time.UTC)
	if len(h.Excursions) != MaxExcursions || h.Excursions[0].Metric != "humidity" && h.Excursions[1].Metric != "humidity" {
		t.Fatalf("cap keeps the earliest: %+v %+v", h.Excursions[0], h.Excursions[1])
	}
	for i := 1; i < len(h.Excursions); i++ {
		if h.Excursions[i].Start.Before(h.Excursions[i-1].Start) {
			t.Fatalf("not in time order at %d", i)
		}
	}
}
