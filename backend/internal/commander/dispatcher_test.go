package commander

import (
	"aether/backend/internal/domain"
	"context"
	"errors"
	"sort"
	"testing"
	"time"
)

// fakeStore mirrors the real outbox: expiry at sweep time, claim per gateway oldest first.
type fakeStore struct {
	pending   []domain.Command
	status    map[string]string
	failed    map[string]string
	published map[string]time.Time
	timeouts  int
}

func (s *fakeStore) init() {
	if s.status == nil {
		s.status, s.failed, s.published = map[string]string{}, map[string]string{}, map[string]time.Time{}
		for _, c := range s.pending {
			s.status[c.ID] = "pending"
		}
	}
}
func (s *fakeStore) ActiveTenants(context.Context) ([]string, error) { return []string{"t1"}, nil }

func (s *fakeStore) ProcessAutomationRequests(context.Context, string, time.Time) (int, error) {
	return 0, nil
}
func (s *fakeStore) PendingCommandGateways(_ context.Context, _ string, now time.Time) ([]string, error) {
	s.init()
	seen := map[string]bool{}
	out := []string{}
	for _, c := range s.pending {
		if s.status[c.ID] != "pending" {
			continue
		}
		if !c.ExpiresAt.After(now) {
			s.status[c.ID] = "expired"
			continue
		}
		if !seen[c.GatewayID] {
			seen[c.GatewayID] = true
			out = append(out, c.GatewayID)
		}
	}
	sort.Strings(out)
	return out, nil
}
func (s *fakeStore) ClaimCommands(_ context.Context, _, gateway string, now time.Time, limit int) ([]domain.Command, error) {
	out := []domain.Command{}
	for _, c := range s.pending {
		if len(out) == limit {
			break
		}
		if c.GatewayID == gateway && s.status[c.ID] == "pending" && c.ExpiresAt.After(now) {
			s.status[c.ID] = "sent"
			out = append(out, c)
		}
	}
	return out, nil
}
func (s *fakeStore) MarkCommandsPublished(_ context.Context, _ string, ids []string, at time.Time) error {
	for _, id := range ids {
		s.published[id] = at
	}
	return nil
}
func (s *fakeStore) MarkCommandFailed(_ context.Context, _, id, reason string) error {
	s.status[id] = "failed"
	s.failed[id] = reason
	return nil
}
func (s *fakeStore) TimeoutCommands(context.Context, string, time.Time) (int, error) {
	s.timeouts++
	return 0, nil
}

type fakePublisher struct {
	sent   []string
	refuse bool
}

func (p *fakePublisher) Publish(topic string, payload []byte) error {
	if p.refuse {
		return errors.New("not authorised")
	}
	p.sent = append(p.sent, topic+" "+string(payload))
	return nil
}

const gwA, gwB = "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"

func cmd(id, gateway string, expires time.Time) domain.Command {
	return domain.Command{ID: id, GatewayID: gateway, IEEE: "0xa4c1380000000002", Property: "state_left", Value: []byte(`"ON"`), ExpiresAt: expires}
}

func TestDispatcherPublishesClaimedCommands(t *testing.T) {
	now := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	store := &fakeStore{pending: []domain.Command{cmd("a", gwA, now.Add(10*time.Second)), cmd("expired", gwA, now.Add(-time.Second))}}
	pub := &fakePublisher{}
	d := &Dispatcher{Store: store, Publisher: pub, Now: func() time.Time { return now }}
	n, e := d.RunOnce(context.Background())
	if e != nil || n != 1 || store.timeouts != 1 || store.status["expired"] != "expired" {
		t.Fatalf("run: %d %v timeouts=%d %v", n, e, store.timeouts, store.status)
	}
	if len(pub.sent) != 1 || pub.sent[0] != "aether/z2m/"+gwA+`/0xa4c1380000000002/set {"state_left":"ON"}` || !store.published["a"].Equal(now) {
		t.Fatalf("published: %v %v", pub.sent, store.published)
	}
}

func TestDispatcherMarksRefusedAndInvalidCommandsFailed(t *testing.T) {
	now := time.Now()
	bad := cmd("bad", gwA, now.Add(10*time.Second))
	bad.Property = "bright ness"
	store := &fakeStore{pending: []domain.Command{cmd("a", gwA, now.Add(10*time.Second)), bad}}
	d := &Dispatcher{Store: store, Publisher: &fakePublisher{refuse: true}, Now: func() time.Time { return now }}
	if n, _ := d.RunOnce(context.Background()); n != 0 || store.failed["a"] != "broker refused the publish" || store.failed["bad"] != "invalid command payload" || len(store.published) != 0 {
		t.Fatalf("failed: %d %v", n, store.failed)
	}
}

// A burst on gateway A is paced (10 now, the rest left pending) without holding up gateway B; the overflow
// is published by later sweeps as tokens refill, or expires if it waits too long.
func TestDispatcherPacesPerGatewayWithoutBlocking(t *testing.T) {
	now := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	var pending []domain.Command
	for i := 0; i < 30; i++ {
		pending = append(pending, cmd(string(rune('a'+i)), gwA, now.Add(time.Duration(10+i)*time.Second)))
	}
	pending = append(pending, cmd("B", gwB, now.Add(10*time.Second)))
	store := &fakeStore{pending: pending}
	clock := now
	d := &Dispatcher{Store: store, Publisher: &fakePublisher{}, Now: func() time.Time { return clock }}
	if n, _ := d.RunOnce(context.Background()); n != 11 || store.status["B"] != "sent" {
		t.Fatalf("first sweep: %d, B=%s", n, store.status["B"])
	}
	pendingA := 0
	for _, c := range pending {
		if store.status[c.ID] == "pending" {
			pendingA++
		}
	}
	if pendingA != 20 {
		t.Fatalf("overflow left pending: %d", pendingA)
	}
	// One second later the bucket has 5 tokens again.
	clock = now.Add(time.Second)
	if n, _ := d.RunOnce(context.Background()); n != 5 {
		t.Fatalf("refill: %d", n)
	}
	// Much later: what never got a token has expired and is never published.
	clock = now.Add(time.Hour)
	if n, _ := d.RunOnce(context.Background()); n != 0 {
		t.Fatalf("after expiry: %d", n)
	}
	expired := 0
	for _, c := range pending {
		if store.status[c.ID] == "expired" {
			expired++
		}
	}
	if expired != 15 {
		t.Fatalf("expired: %d", expired)
	}
}
