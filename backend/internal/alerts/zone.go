package alerts

import "time"

// Zone decisions for roaming wearables. A person's body, walls and doors move RSSI by 10 dB or more, so the
// strongest raw reading flips between neighbouring gateways. The zone therefore changes only when another
// gateway's smoothed RSSI beats the current one by ZoneMarginDB for at least ZoneDwell.
const (
	ZoneMarginDB  = 6.0
	ZoneDwell     = 10 * time.Second
	ZoneFresh     = 60 * time.Second
	rssiSmoothing = 0.35 // weight of the newest sample
)

type ZoneState struct {
	Gateway        string
	Since          time.Time
	Candidate      string
	CandidateSince time.Time
	CandidateSeen  time.Time // last uplink in which the candidate was clearly stronger
}

// ZoneCandidateGap is how long a candidate may go unheard before its dwell time starts over.
const ZoneCandidateGap = 15 * time.Second

// SmoothRSSI is an exponential moving average that restarts after a gap (the old average says nothing then).
func SmoothRSSI(prev *float64, prevAt time.Time, rssi int, at time.Time) float64 {
	if prev == nil || at.Sub(prevAt) > ZoneFresh {
		return float64(rssi)
	}
	return rssiSmoothing*float64(rssi) + (1-rssiSmoothing)**prev
}

// DecideZone is called when `gateway` hears the tag with smoothed RSSI `avg`. currentAvg is the smoothed RSSI
// of the current zone's gateway, or nil when that gateway has not heard the tag within ZoneFresh.
func DecideZone(prev ZoneState, gateway string, avg float64, currentAvg *float64, at time.Time) (ZoneState, bool) {
	next := prev
	clear := func() { next.Candidate, next.CandidateSince, next.CandidateSeen = "", time.Time{}, time.Time{} }
	switch {
	case prev.Gateway == "" || (prev.Gateway != gateway && currentAvg == nil):
		// First sighting, or the old zone lost the tag: no reason to wait.
		return ZoneState{Gateway: gateway, Since: at}, true
	case prev.Gateway == gateway:
		// The current gateway still hears the tag. That must not reset a candidate's dwell time (both gateways
		// report every few seconds); only a candidate that has gone quiet is dropped.
		if prev.Candidate != "" && at.Sub(prev.CandidateSeen) > ZoneCandidateGap {
			clear()
		}
		return next, false
	case avg >= *currentAvg+ZoneMarginDB:
		if prev.Candidate != gateway || at.Sub(prev.CandidateSeen) > ZoneCandidateGap {
			next.Candidate, next.CandidateSince, next.CandidateSeen = gateway, at, at
			return next, false
		}
		next.CandidateSeen = at
		if at.Sub(prev.CandidateSince) >= ZoneDwell {
			return ZoneState{Gateway: gateway, Since: at}, true
		}
		return next, false
	default:
		if prev.Candidate == gateway {
			clear()
		}
		return next, false
	}
}
