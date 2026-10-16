package reports

import "time"

// Schedule is when a saved report is sent: daily, or weekly on an ISO weekday (1 = Monday .. 7 = Sunday), at a local
// time in Bangkok ("HH:MM").
type Schedule struct {
	Kind    string // none | daily | weekly
	Weekday int
	Time    string
}

// clock parses "HH:MM", exactly the form the table's CHECK accepts (two digits, a colon, two digits).
func (s Schedule) clock() (int, int, bool) {
	t := s.Time
	if len(t) != 5 || t[2] != ':' {
		return 0, 0, false
	}
	for _, i := range []int{0, 1, 3, 4} {
		if t[i] < '0' || t[i] > '9' {
			return 0, 0, false
		}
	}
	h, m := int(t[0]-'0')*10+int(t[1]-'0'), int(t[3]-'0')*10+int(t[4]-'0')
	return h, m, h <= 23 && m <= 59
}

// Valid says whether the schedule can run: none, daily at a time, or weekly on a weekday at a time.
func (s Schedule) Valid() bool {
	_, _, ok := s.clock()
	switch s.Kind {
	case "none":
		return true
	case "daily":
		return ok && s.Weekday == 0
	case "weekly":
		return ok && s.Weekday >= 1 && s.Weekday <= 7
	}
	return false
}

// at is the occurrence on the local calendar day of t (whatever its weekday).
func (s Schedule) at(t time.Time, loc *time.Location) time.Time {
	h, m, _ := s.clock()
	l := t.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day(), h, m, 0, 0, loc)
}

func isoWeekday(t time.Time) int {
	if w := int(t.Weekday()); w != 0 {
		return w
	}
	return 7
}

func (s Schedule) onDay(t time.Time, loc *time.Location) bool {
	return s.Kind == "daily" || isoWeekday(t.In(loc)) == s.Weekday
}

// Next is the first occurrence strictly after t; the zero time for "none" or an invalid schedule.
func (s Schedule) Next(t time.Time, loc *time.Location) time.Time {
	if s.Kind == "none" || !s.Valid() {
		return time.Time{}
	}
	l := t.In(loc)
	for i := 0; i <= 8; i++ {
		day := time.Date(l.Year(), l.Month(), l.Day()+i, 12, 0, 0, 0, loc)
		if occ := s.at(day, loc); s.onDay(day, loc) && occ.After(t) {
			return occ
		}
	}
	return time.Time{}
}

// Latest is the last occurrence at or before t; the zero time for "none" or an invalid schedule. After downtime the
// scheduler sends only this one: the periods missed in between are not caught up.
func (s Schedule) Latest(t time.Time, loc *time.Location) time.Time {
	if s.Kind == "none" || !s.Valid() {
		return time.Time{}
	}
	l := t.In(loc)
	for i := 0; i <= 8; i++ {
		day := time.Date(l.Year(), l.Month(), l.Day()-i, 12, 0, 0, 0, loc)
		if occ := s.at(day, loc); s.onDay(day, loc) && !occ.After(t) {
			return occ
		}
	}
	return time.Time{}
}

// Period is what an occurrence reports on: the local calendar day before it (daily), or the seven local days ending
// where its day begins (weekly).
func (s Schedule) Period(occurrence time.Time, loc *time.Location) (time.Time, time.Time) {
	l := occurrence.In(loc)
	to := time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, loc)
	days := 1
	if s.Kind == "weekly" {
		days = 7
	}
	return time.Date(l.Year(), l.Month(), l.Day()-days, 0, 0, 0, 0, loc), to
}

// LastFullDay is the default period of a report sent by hand: yesterday, as a local calendar day.
func LastFullDay(now time.Time, loc *time.Location) (time.Time, time.Time) {
	return Schedule{Kind: "daily"}.Period(now, loc)
}
