package tuyacloudlink

import (
	"container/list"
	"context"
	"math"
	"math/rand/v2"
	"sync"
	"time"
)

// bucket is a token bucket: rate tokens a second, at most burst held.
type bucket struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	at     time.Time
	now    func() time.Time
}

func newBucket(rate, burst float64, now func() time.Time) *bucket {
	return &bucket{rate: rate, burst: burst, tokens: burst, at: now(), now: now}
}

// reserve takes a token and says how long to wait before using it (0 when one was available).
func (b *bucket) reserve() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	b.tokens = math.Min(b.burst, b.tokens+now.Sub(b.at).Seconds()*b.rate)
	b.at = now
	b.tokens--
	if b.tokens >= 0 {
		return 0
	}
	return time.Duration(-b.tokens / b.rate * float64(time.Second))
}

// wait blocks until a token is available: a flood of messages slows the reader down instead of being dropped.
func (b *bucket) wait(ctx context.Context) error {
	d := b.reserve()
	if d == 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// seenIDs remembers the last n message ids, so a message Pulsar redelivers after it was stored (the ack was lost)
// is acknowledged again without being applied twice.
type seenIDs struct {
	n     int
	order *list.List
	index map[string]*list.Element
}

func newSeenIDs(n int) *seenIDs {
	return &seenIDs{n: n, order: list.New(), index: map[string]*list.Element{}}
}

func (s *seenIDs) has(id string) bool {
	_, ok := s.index[id]
	return ok
}

func (s *seenIDs) add(id string) {
	if el, ok := s.index[id]; ok {
		s.order.MoveToFront(el)
		return
	}
	s.index[id] = s.order.PushFront(id)
	for s.order.Len() > s.n {
		last := s.order.Back()
		s.order.Remove(last)
		delete(s.index, last.Value.(string))
	}
}

// backoff is the reconnect delay after the n-th consecutive failure: doubling from Min to Max, each drawn at
// random from its upper half so many links failing together do not reconnect in step.
func backoff(n int, min, max time.Duration) time.Duration {
	d := min
	for i := 0; i < n && d < max; i++ {
		d *= 2
	}
	if d > max {
		d = max
	}
	half := d / 2
	return half + time.Duration(rand.Int64N(int64(half)+1))
}
