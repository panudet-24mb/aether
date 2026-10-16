package reports

import (
	"testing"
	"time"
)

func TestScheduleNextAndLatest(t *testing.T) {
	daily := Schedule{Kind: "daily", Time: "07:30"}
	// 2026-10-16 is a Friday (ISO 5). 00:10 Bangkok on the 16th is still the 15th in UTC.
	at := time.Date(2026, 10, 16, 0, 10, 0, 0, bkk)
	if n := daily.Next(at, bkk); !n.Equal(time.Date(2026, 10, 16, 7, 30, 0, 0, bkk)) {
		t.Fatalf("daily next: %s", n)
	}
	if l := daily.Latest(at, bkk); !l.Equal(time.Date(2026, 10, 15, 7, 30, 0, 0, bkk)) {
		t.Fatalf("daily latest: %s", l)
	}
	// Exactly at the time: that occurrence is due now (latest), the next one is tomorrow.
	exact := time.Date(2026, 10, 16, 7, 30, 0, 0, bkk)
	if !daily.Latest(exact, bkk).Equal(exact) || !daily.Next(exact, bkk).Equal(exact.AddDate(0, 0, 1)) {
		t.Fatalf("at the occurrence: %s %s", daily.Latest(exact, bkk), daily.Next(exact, bkk))
	}
	// Midnight: "00:00" on Monday is the Sunday-to-Monday boundary in Bangkok, 17:00 UTC on Sunday.
	weekly := Schedule{Kind: "weekly", Weekday: 1, Time: "00:00"}
	n := weekly.Next(at, bkk)
	if !n.Equal(time.Date(2026, 10, 19, 0, 0, 0, 0, bkk)) || n.UTC().Hour() != 17 || n.UTC().Weekday() != time.Sunday {
		t.Fatalf("weekly next: %s (%s UTC)", n, n.UTC())
	}
	if l := weekly.Latest(at, bkk); !l.Equal(time.Date(2026, 10, 12, 0, 0, 0, 0, bkk)) {
		t.Fatalf("weekly latest: %s", l)
	}
	sunday := Schedule{Kind: "weekly", Weekday: 7, Time: "23:59"}
	if n := sunday.Next(at, bkk); !n.Equal(time.Date(2026, 10, 18, 23, 59, 0, 0, bkk)) {
		t.Fatalf("sunday next: %s", n)
	}
	// After a week of downtime only the latest occurrence is due.
	down := time.Date(2026, 10, 25, 9, 0, 0, 0, bkk)
	if l := daily.Latest(down, bkk); !l.Equal(time.Date(2026, 10, 25, 7, 30, 0, 0, bkk)) {
		t.Fatalf("after downtime: %s", l)
	}
	for _, bad := range []Schedule{{Kind: "daily", Time: "24:00"}, {Kind: "weekly", Time: "07:00"}, {Kind: "weekly", Weekday: 8, Time: "07:00"}, {Kind: "daily", Weekday: 1, Time: "07:00"}, {Kind: "hourly", Time: "07:00"}, {Kind: "daily", Time: "7:00"}, {Kind: "daily", Time: "+1:00"}, {Kind: "daily", Time: " 1:00"}, {Kind: "daily", Time: "-0:00"}, {Kind: "daily", Time: "07-30"}, {Kind: "daily", Time: "0７:30"}} {
		if bad.Valid() || !bad.Next(at, bkk).IsZero() {
			t.Fatalf("invalid schedule accepted: %+v", bad)
		}
	}
	if !(Schedule{Kind: "none"}).Valid() || !(Schedule{Kind: "none"}).Next(at, bkk).IsZero() {
		t.Fatal("none")
	}
}

func TestSchedulePeriods(t *testing.T) {
	occ := time.Date(2026, 10, 19, 0, 0, 0, 0, bkk) // Monday 00:00
	from, to := (Schedule{Kind: "weekly", Weekday: 1, Time: "00:00"}).Period(occ, bkk)
	if !from.Equal(time.Date(2026, 10, 12, 0, 0, 0, 0, bkk)) || !to.Equal(occ) {
		t.Fatalf("weekly period: %s .. %s", from, to)
	}
	from, to = (Schedule{Kind: "daily", Time: "07:30"}).Period(time.Date(2026, 10, 16, 7, 30, 0, 0, bkk), bkk)
	if !from.Equal(time.Date(2026, 10, 15, 0, 0, 0, 0, bkk)) || !to.Equal(time.Date(2026, 10, 16, 0, 0, 0, 0, bkk)) {
		t.Fatalf("daily period: %s .. %s", from, to)
	}
	// In UTC the Bangkok day starts at 17:00 the day before.
	from, to = LastFullDay(time.Date(2026, 10, 16, 18, 0, 0, 0, time.UTC), bkk) // 01:00 on the 17th in Bangkok
	if from.UTC() != time.Date(2026, 10, 15, 17, 0, 0, 0, time.UTC) || to.UTC() != time.Date(2026, 10, 16, 17, 0, 0, 0, time.UTC) {
		t.Fatalf("last full day: %s .. %s", from.UTC(), to.UTC())
	}
}
