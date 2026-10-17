package compression

import (
	"context"
	"strings"
	"time"
)

// maxFeedbackLines bounds the retry feedback (concise, WBS 9.27).
const maxFeedbackLines = 20

// RetryFeedback renders concise validation feedback for a retry (WBS 9.27):
// one line per failure keyed by the canonical item ID / stage. It never
// includes the failed output or any conversation history, so a failed
// output can never become a retry source (T9.27.3).
func RetryFeedback(res ValidationResult) string {
	if len(res.Failures) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("The previous compression attempt was rejected:\n")
	n := 0
	for _, f := range res.Failures {
		if n == maxFeedbackLines {
			b.WriteString("- (further failures omitted)\n")
			break
		}
		b.WriteString("- ")
		switch {
		case f.ItemID != "":
			b.WriteString(f.ItemID + " " + strings.ToLower(f.Code))
		default:
			b.WriteString(strings.ToLower(string(f.Stage)) + " " + strings.ToLower(f.Code))
		}
		if f.Detail != "" && f.ItemID == "" {
			b.WriteString(": " + truncate(f.Detail, 160))
		}
		b.WriteString("\n")
		n++
	}
	b.WriteString("Recompress the original sections, preserving those items exactly. Do not add requirements.")
	return b.String()
}

// Backoff is a bounded exponential backoff (WBS 9.38): the delay before
// retry n (n >= 1) is Base*2^(n-1), capped at Max.
type Backoff struct {
	Base time.Duration
	Max  time.Duration
}

// Delay returns the delay before retry n.
func (b Backoff) Delay(n int) time.Duration {
	if n < 1 || b.Base <= 0 {
		return 0
	}
	d := b.Base
	for i := 1; i < n; i++ {
		d *= 2
		if b.Max > 0 && d >= b.Max {
			return b.Max
		}
	}
	if b.Max > 0 && d > b.Max {
		return b.Max
	}
	return d
}

// Sleeper waits for d or until ctx is done (cancellation interrupts the
// backoff, T9.38.4).
type Sleeper interface {
	Sleep(ctx context.Context, d time.Duration) error
}

// RealSleeper sleeps on a timer.
type RealSleeper struct{}

// Sleep implements Sleeper.
func (RealSleeper) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
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
