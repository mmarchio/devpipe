package templates

import (
	"testing"
	"time"
)

func TestElapsed(t *testing.T) {
	start := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { x := start.Add(d); return &x }
	for _, c := range []struct {
		end  *time.Time
		want string
	}{
		{at(0), "00:00:00"},
		{at(59 * time.Second), "00:00:59"},
		{at(time.Hour + 2*time.Minute + 3*time.Second + 900*time.Millisecond), "01:02:03"},
		{at(123*time.Hour + 45*time.Minute), "123:45:00"},
		{at(-time.Minute), "00:00:00"},
	} {
		if got := elapsed(&start, c.end); got != c.want {
			t.Errorf("elapsed to %v = %q, want %q", c.end, got, c.want)
		}
	}
	if got := elapsed(nil, nil); got != "—" {
		t.Errorf("elapsed(nil) = %q", got)
	}
	running := time.Now().Add(-90 * time.Second)
	if got := elapsed(&running, nil); got != "00:01:30" && got != "00:01:31" {
		t.Errorf("running elapsed = %q", got)
	}
}
