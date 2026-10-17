// Package clock is the time seam for Phase 18 code (WBS 18.194 / 18.195):
// timestamps are persisted in UTC, durations are measured with the
// monotonic clock, and ordering uses database sequences, never wall time.
package clock

import "time"

// Clock provides time.
type Clock interface {
	// Now returns the current time in UTC (persisted timestamps).
	Now() time.Time
	// Since returns the elapsed duration since t using the monotonic
	// reading when t carries one.
	Since(t time.Time) time.Duration
}

// System is the real clock.
type System struct{}

// Now returns time.Now() — with its monotonic reading kept for Since —
// converted to UTC for persistence.
func (System) Now() time.Time { return time.Now().UTC() }

// Since measures elapsed time (monotonic when available).
func (System) Since(t time.Time) time.Duration { return time.Since(t) }

// Fake is a manually advanced clock (tests).
type Fake struct{ T time.Time }

// Now returns the fake time.
func (f *Fake) Now() time.Time { return f.T.UTC() }

// Since returns the fake elapsed time.
func (f *Fake) Since(t time.Time) time.Duration { return f.T.Sub(t) }

// Advance moves the fake clock.
func (f *Fake) Advance(d time.Duration) { f.T = f.T.Add(d) }

// Elapsed returns end-start using the monotonic readings of both when
// present (time.Sub does this), so a wall-clock jump (DST, NTP step,
// time-zone change) between them does not corrupt the duration.
func Elapsed(start, end time.Time) time.Duration { return end.Sub(start) }
