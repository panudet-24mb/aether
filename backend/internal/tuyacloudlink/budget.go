package tuyacloudlink

import (
	"aether/backend/internal/adapters/postgres"
	"sync"
)

// Budget is what a cloud project may use in a month. A Tuya trial project has a small monthly allowance of
// Message Service messages and OpenAPI calls; the worker stops spending well before Tuya cuts the project off.
// Zero means unlimited.
type Budget struct {
	EventsMonth   int64
	APICallsMonth int64
}

// GuardAt is the share of the budget from which the trial guard is on: commands are refused ("quota_near") and
// events of devices that are not registered are dropped, so what is left serves the registered devices' state.
const GuardAt = 0.95

// usage is one link's monthly counters: the totals the database last returned plus what this worker counted
// since (flushed by the health loop).
type usage struct {
	mu      sync.Mutex
	budget  Budget
	total   postgres.CloudUsage
	pending postgres.CloudUsage
}

func (u *usage) add(events, apiCalls, dropped int64) {
	u.mu.Lock()
	u.pending.Events += events
	u.pending.APICalls += apiCalls
	u.pending.Dropped += dropped
	u.mu.Unlock()
}

// take returns the counts not yet flushed and forgets them; restore puts them back when the flush failed.
func (u *usage) take() postgres.CloudUsage {
	u.mu.Lock()
	defer u.mu.Unlock()
	p := u.pending
	u.pending = postgres.CloudUsage{}
	return p
}

func (u *usage) restore(p postgres.CloudUsage) {
	u.add(p.Events, p.APICalls, p.Dropped)
}

// settle records the month's totals as the database returned them (which already include what was flushed).
func (u *usage) settle(total postgres.CloudUsage) {
	u.mu.Lock()
	u.total = total
	u.mu.Unlock()
}

// share is the larger of the two budget shares used this month (0 without a budget).
func (u *usage) share() float64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	share := 0.0
	if u.budget.EventsMonth > 0 {
		share = max(share, float64(u.total.Events+u.pending.Events)/float64(u.budget.EventsMonth))
	}
	if u.budget.APICallsMonth > 0 {
		share = max(share, float64(u.total.APICalls+u.pending.APICalls)/float64(u.budget.APICallsMonth))
	}
	return share
}

// guarded reports whether the trial guard is on.
func (u *usage) guarded() bool { return u.share() >= GuardAt }

func (u *usage) snapshot() postgres.CloudUsage {
	u.mu.Lock()
	defer u.mu.Unlock()
	return postgres.CloudUsage{Events: u.total.Events + u.pending.Events, APICalls: u.total.APICalls + u.pending.APICalls, Dropped: u.total.Dropped + u.pending.Dropped}
}
