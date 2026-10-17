package continuation

import (
	"time"

	"github.com/google/uuid"
)

// UnitMetrics are per-work-unit counters.
type UnitMetrics struct {
	Continuations int `json:"continuations"`
	SessionResets int `json:"session_resets"`
}

// Metrics are continuation counters derived from persisted rows (WBS
// 14.88).
type Metrics struct {
	ContinuationsTotal           int                       `json:"continuations_total"`
	SameSessionContinuations     int                       `json:"same_session_continuations"`
	FreshSessionContinuations    int                       `json:"fresh_session_continuations"`
	ContinuationFailures         int                       `json:"continuation_failures"`
	ContinuationBlocks           int                       `json:"continuation_blocks"`
	ContinuationNoProgress       int                       `json:"continuation_no_progress"`
	ContinuationRecoveryRequired int                       `json:"continuation_recovery_required"`
	CompletedDurationTotal       time.Duration             `json:"continuation_duration_total_ns"`
	CompletedDurationAvg         time.Duration             `json:"continuation_duration_avg_ns"`
	PerWorkUnit                  map[uuid.UUID]UnitMetrics `json:"per_work_unit"`
}

// Aggregate computes metrics from continuation rows.
func Aggregate(rows []Continuation) Metrics {
	m := Metrics{PerWorkUnit: map[uuid.UUID]UnitMetrics{}}
	finished := 0
	for _, c := range rows {
		m.ContinuationsTotal++
		u := m.PerWorkUnit[c.WorkUnitID]
		u.Continuations++
		switch c.Strategy {
		case ContinueSameSession:
			m.SameSessionContinuations++
		case ContinueFreshSession:
			m.FreshSessionContinuations++
		}
		if c.NewSessionID != nil && c.SessionConfirmedAt != nil {
			u.SessionResets++
		}
		m.PerWorkUnit[c.WorkUnitID] = u
		switch c.Status {
		case StatusFailed:
			m.ContinuationFailures++
		case StatusBlocked:
			m.ContinuationBlocks++
		case StatusRecoveryRequired:
			m.ContinuationRecoveryRequired++
		}
		if c.RecoveryReason == RecoveryNoProgress {
			m.ContinuationNoProgress++
		}
		if c.CompletedAt != nil {
			m.CompletedDurationTotal += c.CompletedAt.Sub(c.CreatedAt)
			finished++
		}
	}
	if finished > 0 {
		m.CompletedDurationAvg = m.CompletedDurationTotal / time.Duration(finished)
	}
	return m
}
