package resilience

import (
	"testing"
	"time"
)

// T18.27 / T18.28 — repeated failures open the circuit (no more traffic);
// after the cooldown one probe is admitted; success closes it.
func TestT1827Breaker(t *testing.T) {
	now := time.Unix(1000, 0)
	b := NewBreaker(BreakerConfig{Failures: 3, Window: time.Minute, Cooldown: 10 * time.Second, Successes: 1})
	for i := 0; i < 3; i++ {
		if !b.Allow(now).Allowed {
			t.Fatal("closed breaker refused")
		}
		b.Failure(now, "connection refused")
	}
	if a, st, _ := b.Status(); a != Unavailable || st != BreakerOpen {
		t.Fatalf("not open: %s %s", a, st)
	}
	allowed := 0
	for i := 0; i < 100; i++ {
		if b.Allow(now.Add(time.Second)).Allowed {
			allowed++
		}
	}
	if allowed != 0 {
		t.Fatalf("open breaker admitted %d requests", allowed)
	}
	p := b.Allow(now.Add(11 * time.Second))
	if !p.Allowed || !p.Probe || b.Allow(now.Add(11*time.Second)).Allowed {
		t.Fatal("half-open must admit exactly one probe")
	}
	b.Failure(now.Add(12*time.Second), "still down")
	if b.Allow(now.Add(13 * time.Second)).Allowed {
		t.Fatal("failed probe did not re-open")
	}
	p = b.Allow(now.Add(30 * time.Second))
	if !p.Probe {
		t.Fatal("no probe after second cooldown")
	}
	b.Success(now.Add(30 * time.Second))
	if a, st, _ := b.Status(); a != Healthy || st != BreakerClosed || !b.Allow(now.Add(31*time.Second)).Allowed {
		t.Fatal("successful probe did not close")
	}
	// Failures spread beyond the window do not open it.
	c := NewBreaker(BreakerConfig{Failures: 2, Window: time.Second, Cooldown: time.Second})
	c.Failure(now, "x")
	c.Failure(now.Add(2*time.Second), "x")
	if a, _, _ := c.Status(); a != Degraded {
		t.Fatalf("windowed failures: %s", a)
	}
}
