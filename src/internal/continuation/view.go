package continuation

import (
	"fmt"
	"strconv"
	"strings"

	"overseer/internal/worker"
)

// ProgressView is the continuation progress read model (WBS 14.77-14.80),
// derived only from the persisted continuation, confirmed Phase 12 session
// state and telemetry.
type ProgressView struct {
	ContinuationID string `json:"continuation_id"`
	CurrentTask    string `json:"current_task"`
	TaskStatus     string `json:"task_status"`

	Status       Status `json:"status"`
	Attempt      int    `json:"attempt"`
	MaxAttempts  int    `json:"max_attempts"`
	AttemptLabel string `json:"attempt_label"`
	Strategy     string `json:"strategy"`
	Reason       string `json:"reason"`

	Context ContextView `json:"context"`

	Reset      *ResetView      `json:"reset,omitempty"`
	NoProgress *NoProgressView `json:"no_progress,omitempty"`
	Recovery   string          `json:"recovery,omitempty"`
}

// ContextView renders worker context; unknown is never shown as 0%.
type ContextView struct {
	Known     bool     `json:"known"`
	Percent   *float64 `json:"percent,omitempty"`
	Label     string   `json:"label"`
	Tokens    string   `json:"tokens,omitempty"`
	Bar       string   `json:"bar,omitempty"`
	Estimated bool     `json:"estimated"`
}

// ResetView shows confirmed session-reset states only (WBS 14.78).
type ResetView struct {
	OldSession       string `json:"old_session"`
	NewSession       string `json:"new_session"`
	WorkingDirectory string `json:"working_directory"`
}

// NoProgressView is the structural no-progress indicator (WBS 14.80).
type NoProgressView struct {
	Title  string `json:"title"`
	Detail string `json:"detail"`
}

// ViewInput carries the persisted values the view is derived from.
type ViewInput struct {
	Continuation *Continuation
	Settings     Settings
	TaskLabel    string
	TaskStatus   string
	Reading      *ContextReading
	OldSessionSt worker.SessionStatus
	NewSessionSt worker.SessionStatus
}

const barWidth = 25

// FormatPercent renders a fraction as a percentage with at most two
// decimals ("54%", "51.31%").
func FormatPercent(fraction float64) string {
	s := strconv.FormatFloat(fraction*100, 'f', 2, 64)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	return s + "%"
}

func groupDigits(v int64) string {
	s := strconv.FormatInt(v, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// BuildContextView renders a reading.
func BuildContextView(r *ContextReading) ContextView {
	if r == nil || !r.Known || r.Utilization == nil {
		return ContextView{Label: "Unknown"}
	}
	p := *r.Utilization * 100
	filled := int(*r.Utilization*barWidth + 0.5)
	if filled > barWidth {
		filled = barWidth
	}
	if filled < 0 {
		filled = 0
	}
	return ContextView{Known: true, Percent: &p, Label: FormatPercent(*r.Utilization), Estimated: r.Estimated,
		Tokens: groupDigits(*r.Tokens) + " / " + groupDigits(*r.Window),
		Bar:    strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled)}
}

// ReasonText is the human-readable decision reason.
func ReasonText(d *Decision) string {
	if d == nil {
		return "Not yet decided"
	}
	pct := ""
	if d.Utilization != nil {
		pct = FormatPercent(*d.Utilization)
	}
	thr := FormatPercent(d.Threshold)
	switch d.Reason {
	case ReasonBelowThreshold:
		return fmt.Sprintf("Context %s < %s", pct, thr)
	case ReasonAtThreshold, ReasonAboveThreshold:
		return fmt.Sprintf("Context %s >= %s", pct, thr)
	case ReasonContextUnknown:
		return "Context telemetry unavailable"
	case ReasonContextLimit:
		return "Worker context limit reached"
	case ReasonSessionLost:
		return "Worker session lost"
	case ReasonOperatorOverride:
		return "Operator override"
	}
	return string(d.Reason)
}

// BuildProgressView derives the read model.
func BuildProgressView(in ViewInput) ProgressView {
	c := in.Continuation
	label := fmt.Sprintf("Attempt %d of %d", c.Attempt, in.Settings.MaxContinuationsPerWorkUnit)
	if in.Settings.MaxContinuationsPerWorkUnit == Unlimited {
		label = fmt.Sprintf("Attempt %d (no limit)", c.Attempt)
	}
	v := ProgressView{ContinuationID: c.ID.String(), CurrentTask: in.TaskLabel, TaskStatus: in.TaskStatus, Status: c.Status,
		Attempt: c.Attempt, MaxAttempts: in.Settings.MaxContinuationsPerWorkUnit,
		AttemptLabel: label,
		Strategy:     c.Strategy.Label(), Reason: ReasonText(c.Decision)}
	reading := in.Reading
	if reading == nil && c.Decision != nil {
		reading = &ContextReading{Known: c.Decision.Utilization != nil, Tokens: c.Decision.ContextTokens,
			Window: c.Decision.ContextWindow, Utilization: c.Decision.Utilization, Estimated: c.Decision.Estimated, Source: c.Decision.Source}
	}
	v.Context = BuildContextView(reading)
	if c.Strategy == ContinueFreshSession {
		r := &ResetView{OldSession: "Unknown", NewSession: "Not yet created", WorkingDirectory: "Not yet restored"}
		if c.OldSessionID == nil {
			r.OldSession = "None"
		} else if in.OldSessionSt != "" {
			r.OldSession = string(in.OldSessionSt)
		}
		if c.NewSessionID != nil && c.SessionConfirmedAt != nil {
			r.NewSession = "Unknown"
			if in.NewSessionSt != "" {
				r.NewSession = string(in.NewSessionSt)
			}
		}
		if c.CWDConfirmedAt != nil {
			r.WorkingDirectory = "Restored"
		}
		v.Reset = r
	}
	if in.Settings.MaxNoProgressContinuations > 0 && c.NoProgressStreak >= in.Settings.MaxNoProgressContinuations {
		v.NoProgress = &NoProgressView{Title: "No Progress Detected",
			Detail: fmt.Sprintf("%d consecutive continuation reports contained no meaningful change.", c.NoProgressStreak)}
	}
	if c.RecoveryReason != "" {
		v.Recovery = string(c.RecoveryReason)
	}
	return v
}
