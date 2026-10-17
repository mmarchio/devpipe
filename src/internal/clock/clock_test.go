package clock

import (
	"testing"
	"time"
)

// T18.194 — persisted timestamps are UTC; durations ignore time-zone /
// DST representation changes.
func TestT18194Clock(t *testing.T) {
	var c Clock = System{}
	now := c.Now()
	if now.Location() != time.UTC {
		t.Fatal("not UTC")
	}
	start := time.Now()
	// The same instant in another zone (and across a DST boundary) must
	// not change a duration.
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("tzdata unavailable")
	}
	before := time.Date(2026, 3, 8, 1, 30, 0, 0, ny) // 30 min before the DST jump
	after := before.Add(time.Hour)                   // wall clock shows 03:30
	if Elapsed(before, after) != time.Hour || Elapsed(before.UTC(), after.In(time.FixedZone("x", 5*3600))) != time.Hour {
		t.Fatal("DST corrupted duration")
	}
	if c.Since(start) < 0 {
		t.Fatal("negative monotonic duration")
	}
	f := &Fake{T: time.Unix(0, 0)}
	s := f.Now()
	f.Advance(3 * time.Second)
	if f.Since(s) != 3*time.Second {
		t.Fatal("fake")
	}
}
