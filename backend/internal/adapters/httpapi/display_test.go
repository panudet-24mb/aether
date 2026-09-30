package httpapi

import (
	"testing"
	"time"
)

// Failed pairings are capped across all clients, not only per address, and the budget comes back each minute.
func TestPairFailureBudget(t *testing.T) {
	f := &pairFailures{}
	now := time.Now()
	for i := 0; i < maxPairFailuresPerMinute; i++ {
		if f.blocked(now) {
			t.Fatalf("blocked after %d failures", i)
		}
		f.fail(now)
	}
	if !f.blocked(now.Add(30 * time.Second)) {
		t.Fatal("not blocked at the budget")
	}
	if f.blocked(now.Add(61 * time.Second)) {
		t.Fatal("still blocked in the next minute")
	}
}

// The pairing limiter keys IPv6 clients by their /64, so rotating addresses inside one network does not reset it.
func TestPairLimiterKey(t *testing.T) {
	cases := map[string]string{
		"203.0.113.9":                "203.0.113.9",
		"::ffff:203.0.113.9":         "203.0.113.9",
		"2001:db8:1:2:aaaa::1":       "2001:db8:1:2::/64",
		"2001:db8:1:2:ffff:ffff:0:9": "2001:db8:1:2::/64",
		"2001:db8:1:3::1":            "2001:db8:1:3::/64",
	}
	for in, want := range cases {
		if got := ipKey(in); got != want {
			t.Fatalf("%s: %s, want %s", in, got, want)
		}
	}
}
