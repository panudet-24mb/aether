// Package reports turns 5-minute buckets (core.sample_rollup) into the numbers of a summary report: overall and
// per-day min/max/avg, coverage, excursions out of an allowed range and an hourly series for the chart. Pure
// functions only; the repository reads the buckets and the HTTP layer sends the result (docs/platform/reports.md).
package reports

import (
	"math"
	"sort"
	"time"
)

// Step is the width of one rollup bucket.
const Step = 5 * time.Minute

// MaxExcursions is how many excursions one sensor lists; minutes out of range still count every bucket.
const MaxExcursions = 500

// settle is how long after a bucket closes the rollup worker (every minute, up to the last bucket closed a minute
// ago) has certainly written it. Younger buckets are not expected yet, so today's report is not short of data.
const settle = 2 * time.Minute

// Bangkok is the zone reports are written in: CSV times and calendar days. It has no daylight saving, so the fixed
// offset is an exact fallback where the zone database is missing.
func Bangkok() *time.Location {
	if loc, e := time.LoadLocation("Asia/Bangkok"); e == nil {
		return loc
	}
	return time.FixedZone("ICT", 7*3600)
}

// Bucket is one 5-minute rollup row of one sensor; nil means no reading of that kind in the bucket.
type Bucket struct {
	At               time.Time
	N                int
	TAvg, TMin, TMax *float64
	HAvg, HMin, HMax *float64
}

// Range is an allowed range; either end may be open.
type Range struct {
	Min *float64 `json:"min"`
	Max *float64 `json:"max"`
}

// Stats are min/max/avg of one metric. Avg is the mean of the 5-minute averages: every 5 minutes weighs the same,
// however many readings the sensor sent in them.
type Stats struct {
	Min float64 `json:"min"`
	Max float64 `json:"max"`
	Avg float64 `json:"avg"`
}

// Day is one calendar day in the report's time zone.
type Day struct {
	Date        string  `json:"date"`
	Buckets     int     `json:"buckets"`
	Expected    int     `json:"expected"`
	Coverage    float64 `json:"coverage_pct"`
	Temperature *Stats  `json:"temperature"`
	Humidity    *Stats  `json:"humidity"`
}

// Excursion is a run of consecutive buckets out of range in one direction. A missing bucket ends the run: the report
// only claims what was measured. End is the end of the last bucket; Peak the most extreme value in the run.
type Excursion struct {
	Metric      string    `json:"metric"`    // temperature | humidity
	Direction   string    `json:"direction"` // high | low
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	DurationMin int       `json:"duration_min"`
	Peak        float64   `json:"peak"`
}

// Minutes is time out of range per metric: distinct out-of-range buckets times five minutes.
type Minutes struct {
	Temperature int `json:"temperature"`
	Humidity    int `json:"humidity"`
}

// Point is one hour of the chart series.
type Point struct {
	At   time.Time `json:"at"`
	TAvg *float64  `json:"t_avg"`
	TMin *float64  `json:"t_min"`
	TMax *float64  `json:"t_max"`
	HAvg *float64  `json:"h_avg"`
	HMin *float64  `json:"h_min"`
	HMax *float64  `json:"h_max"`
}

// Summary is everything a report says about one sensor.
type Summary struct {
	Buckets             int         `json:"buckets"` // with data, of the expected slots
	Expected            int         `json:"expected_buckets"`
	Coverage            float64     `json:"coverage_pct"`
	Temperature         *Stats      `json:"temperature"`
	Humidity            *Stats      `json:"humidity"`
	Days                []Day       `json:"days"`
	Excursions          []Excursion `json:"excursions"`
	ExcursionsTruncated bool        `json:"excursions_truncated"`
	MinutesOutOfRange   Minutes     `json:"minutes_out_of_range"`
	Series              []Point     `json:"series"`
}

// acc accumulates min/max/avg of one metric.
type acc struct {
	n             int
	sum, min, max float64
	minSet        bool
	maxSet        bool
}

func (a *acc) add(avg, lo, hi *float64) {
	if avg != nil {
		a.n++
		a.sum += *avg
	}
	if lo != nil && (!a.minSet || *lo < a.min) {
		a.min, a.minSet = *lo, true
	}
	if hi != nil && (!a.maxSet || *hi > a.max) {
		a.max, a.maxSet = *hi, true
	}
}

func (a *acc) stats() *Stats {
	if a.n == 0 || !a.minSet || !a.maxSet {
		return nil
	}
	return &Stats{Min: round2(a.min), Max: round2(a.max), Avg: round2(a.sum / float64(a.n))}
}

func (a *acc) avg() *float64 {
	if a.n == 0 {
		return nil
	}
	v := round2(a.sum / float64(a.n))
	return &v
}

func (a *acc) lo() *float64 {
	if !a.minSet {
		return nil
	}
	v := round2(a.min)
	return &v
}

func (a *acc) hi() *float64 {
	if !a.maxSet {
		return nil
	}
	v := round2(a.max)
	return &v
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func pct(n, of int) float64 {
	if of <= 0 {
		return 0
	}
	return math.Min(100, math.Round(float64(n)*1000/float64(of))/10)
}

// hasData: a bucket counts toward coverage when it holds a temperature or a humidity.
func hasData(b Bucket) bool { return b.TAvg != nil || b.HAvg != nil }

// run is an excursion being built.
type run struct {
	start, last time.Time
	count       int
	peak        float64
}

// Summarize computes one sensor's summary for [from, to). Buckets may come in any order; ones outside the range
// are ignored. A 5-minute slot is expected when it starts in the range and closed long enough ago for the rollup to
// have written it (now). Days are calendar days in loc.
func Summarize(buckets []Bucket, from, to, now time.Time, temp, hum Range, loc *time.Location) Summary {
	start := from.Truncate(Step)
	end := to
	if settled := now.Add(-settle).Truncate(Step); settled.Before(end) {
		end = settled
	}
	in := make([]Bucket, 0, len(buckets))
	for _, b := range buckets {
		if !b.At.Before(start) && b.At.Before(to) {
			in = append(in, b)
		}
	}
	sort.Slice(in, func(i, j int) bool { return in[i].At.Before(in[j].At) })

	out := Summary{Days: []Day{}, Excursions: []Excursion{}, Series: []Point{}}
	dayIndex := map[string]int{}
	day := func(at time.Time) *Day {
		key := at.In(loc).Format("2006-01-02")
		i, ok := dayIndex[key]
		if !ok {
			i = len(out.Days)
			dayIndex[key] = i
			out.Days = append(out.Days, Day{Date: key})
		}
		return &out.Days[i]
	}
	for slot := start; slot.Before(end); slot = slot.Add(Step) {
		out.Expected++
		day(slot).Expected++
	}

	var t, h acc
	dayT, dayH := map[string]*acc{}, map[string]*acc{}
	type hour struct {
		at   time.Time
		t, h acc
	}
	hours := []*hour{}
	open := map[string]*run{} // metric/direction -> the run that the previous bucket extended
	closeRun := func(key, metric, direction string) {
		r := open[key]
		if r == nil {
			return
		}
		delete(open, key)
		out.Excursions = append(out.Excursions, Excursion{Metric: metric, Direction: direction, Start: r.start, End: r.last.Add(Step), DurationMin: r.count * int(Step/time.Minute), Peak: round2(r.peak)})
	}
	extend := func(key, metric, direction string, at time.Time, value float64, outside bool) {
		r := open[key]
		if !outside {
			closeRun(key, metric, direction)
			return
		}
		if r != nil && r.last.Add(Step).Equal(at) {
			r.last, r.count = at, r.count+1
			if (direction == "high" && value > r.peak) || (direction == "low" && value < r.peak) {
				r.peak = value
			}
			return
		}
		closeRun(key, metric, direction)
		open[key] = &run{start: at, last: at, count: 1, peak: value}
	}
	check := func(metric string, limits Range, at time.Time, lo, hi *float64) bool {
		high := limits.Max != nil && hi != nil && *hi > *limits.Max
		low := limits.Min != nil && lo != nil && *lo < *limits.Min
		var hv, lv float64
		if hi != nil {
			hv = *hi
		}
		if lo != nil {
			lv = *lo
		}
		extend(metric+"/high", metric, "high", at, hv, high)
		extend(metric+"/low", metric, "low", at, lv, low)
		return high || low
	}

	for _, b := range in {
		// Only buckets of expected slots count, so a bucket the rollup wrote early never lifts coverage past 100%.
		if hasData(b) && b.At.Before(end) {
			out.Buckets++
			day(b.At).Buckets++
		}
		t.add(b.TAvg, b.TMin, b.TMax)
		h.add(b.HAvg, b.HMin, b.HMax)
		key := b.At.In(loc).Format("2006-01-02")
		if dayT[key] == nil {
			dayT[key], dayH[key] = &acc{}, &acc{}
		}
		dayT[key].add(b.TAvg, b.TMin, b.TMax)
		dayH[key].add(b.HAvg, b.HMin, b.HMax)
		at := b.At.Truncate(time.Hour)
		if len(hours) == 0 || !hours[len(hours)-1].at.Equal(at) {
			hours = append(hours, &hour{at: at})
		}
		hours[len(hours)-1].t.add(b.TAvg, b.TMin, b.TMax)
		hours[len(hours)-1].h.add(b.HAvg, b.HMin, b.HMax)
		if check("temperature", temp, b.At, b.TMin, b.TMax) {
			out.MinutesOutOfRange.Temperature += int(Step / time.Minute)
		}
		if check("humidity", hum, b.At, b.HMin, b.HMax) {
			out.MinutesOutOfRange.Humidity += int(Step / time.Minute)
		}
	}
	for _, metric := range []string{"temperature", "humidity"} {
		for _, direction := range []string{"high", "low"} {
			closeRun(metric+"/"+direction, metric, direction)
		}
	}
	// The first MaxExcursions in time, whatever metric or direction they are.
	sort.SliceStable(out.Excursions, func(i, j int) bool { return out.Excursions[i].Start.Before(out.Excursions[j].Start) })
	if len(out.Excursions) > MaxExcursions {
		out.Excursions, out.ExcursionsTruncated = out.Excursions[:MaxExcursions], true
	}
	out.Coverage = pct(out.Buckets, out.Expected)
	out.Temperature, out.Humidity = t.stats(), h.stats()
	for i := range out.Days {
		d := &out.Days[i]
		d.Coverage = pct(d.Buckets, d.Expected)
		if a := dayT[d.Date]; a != nil {
			d.Temperature, d.Humidity = a.stats(), dayH[d.Date].stats()
		}
	}
	// Days with data but no expected slot (the rollup ran ahead of `settle`) still get their numbers.
	for key, a := range dayT {
		if _, ok := dayIndex[key]; !ok {
			out.Days = append(out.Days, Day{Date: key, Temperature: a.stats(), Humidity: dayH[key].stats()})
		}
	}
	sort.SliceStable(out.Days, func(i, j int) bool { return out.Days[i].Date < out.Days[j].Date })
	for _, hr := range hours {
		out.Series = append(out.Series, Point{At: hr.at, TAvg: hr.t.avg(), TMin: hr.t.lo(), TMax: hr.t.hi(), HAvg: hr.h.avg(), HMin: hr.h.lo(), HMax: hr.h.hi()})
	}
	return out
}
