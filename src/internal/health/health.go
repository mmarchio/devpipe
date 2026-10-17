// Package health is the pure liveness / readiness / degraded-health and
// alert model of Phase 18 (WBS 18.115–18.120). Services gather facts; the
// rules here decide.
package health

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Status of a subsystem (WBS 18.117).
type Status string

const (
	Healthy     Status = "HEALTHY"
	Degraded    Status = "DEGRADED"
	Unavailable Status = "UNAVAILABLE"
)

func rank(s Status) int {
	switch s {
	case Unavailable:
		return 2
	case Degraded:
		return 1
	}
	return 0
}

// Subsystem is one component's status.
type Subsystem struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail,omitempty"`
	// StartupCritical subsystems (the database) make the whole service
	// UNAVAILABLE; others only degrade it (WBS 18.5 / 18.117).
	StartupCritical bool `json:"startup_critical"`
}

// Report is the detailed health view.
type Report struct {
	Status     Status      `json:"status"`
	Subsystems []Subsystem `json:"subsystems"`
}

// NewReport sorts subsystems and folds the overall status: an unavailable
// startup-critical subsystem → UNAVAILABLE; any other non-healthy
// subsystem → DEGRADED.
func NewReport(subs []Subsystem) Report {
	out := append([]Subsystem(nil), subs...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	st := Healthy
	for _, s := range out {
		switch {
		case s.Status == Unavailable && s.StartupCritical:
			st = Unavailable
		case s.Status != Healthy && st == Healthy:
			st = Degraded
		}
	}
	return Report{Status: st, Subsystems: out}
}

// ReadinessFacts are the inputs of /ready (WBS 18.116).
type ReadinessFacts struct {
	Initialized      bool
	DatabaseUp       bool
	SchemaCompatible bool
	RestoreValidated bool
	Draining         bool
}

// Ready decides readiness; reasons lists every failing check (sorted).
// Provider outages never affect readiness (they degrade, WBS 18.117).
func Ready(f ReadinessFacts) (bool, []string) {
	var r []string
	if !f.Initialized {
		r = append(r, "application not initialized")
	}
	if !f.DatabaseUp {
		r = append(r, "database unavailable")
	}
	if !f.SchemaCompatible {
		r = append(r, "schema incompatible")
	}
	if !f.RestoreValidated {
		r = append(r, "restore validation pending")
	}
	if f.Draining {
		r = append(r, "shutting down")
	}
	sort.Strings(r)
	return len(r) == 0, r
}

// AlertID names an alert condition (WBS 18.118).
type AlertID string

const (
	AlertRecoveryRequired        AlertID = "RECOVERY_REQUIRED"
	AlertDatabaseUnavailable     AlertID = "DATABASE_UNAVAILABLE"
	AlertWorkerUnavailable       AlertID = "WORKER_UNAVAILABLE"
	AlertDiskThreshold           AlertID = "DISK_THRESHOLD"
	AlertAuditVerification       AlertID = "AUDIT_VERIFICATION_FAILED"
	AlertValidationInfraRepeated AlertID = "VALIDATION_INFRA_REPEATED"
	AlertSchedulerStalled        AlertID = "SCHEDULER_STALLED"
	AlertExecutionDeadlocked     AlertID = "EXECUTION_DEADLOCKED"
	AlertIntegrityViolation      AlertID = "INTEGRITY_VIOLATION"
	// AlertGCFailed: the latest garbage collection failed (decision U1).
	AlertGCFailed AlertID = "GC_FAILED"
)

// AlertIDs lists every alert (stable order).
func AlertIDs() []AlertID {
	return []AlertID{AlertRecoveryRequired, AlertDatabaseUnavailable, AlertWorkerUnavailable, AlertDiskThreshold,
		AlertAuditVerification, AlertValidationInfraRepeated, AlertSchedulerStalled, AlertExecutionDeadlocked, AlertIntegrityViolation, AlertGCFailed}
}

// AlertFacts are the evaluator inputs.
type AlertFacts struct {
	DatabaseUp               bool
	RecoveryRequired         []string // execution ids
	WorkersUnavailable       []string // provider names
	DiskOverThreshold        []string // locations
	AuditStatus              string   // VALID / INVALID / INCOMPLETE / "" (unknown)
	ValidationInfraFailures  int      // within the evaluation window
	ValidationInfraThreshold int
	Stalled                  []string // execution ids (runnable, no progress)
	Deadlocked               []string // execution ids (runnable, nothing eligible)
	IntegrityViolations      int
	GCFailed                 bool
}

// Alert is one evaluated condition.
type Alert struct {
	ID     AlertID   `json:"id"`
	Active bool      `json:"active"`
	Detail string    `json:"detail,omitempty"`
	Since  time.Time `json:"since,omitempty"`
}

func list(ids []string) string {
	s := append([]string(nil), ids...)
	sort.Strings(s)
	return strings.Join(s, ", ")
}

// Evaluate computes every alert's state from facts (pure, deterministic).
func Evaluate(f AlertFacts) []Alert {
	th := f.ValidationInfraThreshold
	if th <= 0 {
		th = 3
	}
	return []Alert{
		{ID: AlertRecoveryRequired, Active: len(f.RecoveryRequired) > 0, Detail: list(f.RecoveryRequired)},
		{ID: AlertDatabaseUnavailable, Active: !f.DatabaseUp},
		{ID: AlertWorkerUnavailable, Active: len(f.WorkersUnavailable) > 0, Detail: list(f.WorkersUnavailable)},
		{ID: AlertDiskThreshold, Active: len(f.DiskOverThreshold) > 0, Detail: list(f.DiskOverThreshold)},
		{ID: AlertAuditVerification, Active: f.AuditStatus == "INVALID" || f.AuditStatus == "INCOMPLETE", Detail: f.AuditStatus},
		{ID: AlertValidationInfraRepeated, Active: f.ValidationInfraFailures >= th, Detail: fmt.Sprintf("%d failures (threshold %d)", f.ValidationInfraFailures, th)},
		{ID: AlertSchedulerStalled, Active: len(f.Stalled) > 0, Detail: list(f.Stalled)},
		{ID: AlertExecutionDeadlocked, Active: len(f.Deadlocked) > 0, Detail: list(f.Deadlocked)},
		{ID: AlertIntegrityViolation, Active: f.IntegrityViolations > 0, Detail: fmt.Sprintf("%d violations", f.IntegrityViolations)},
		{ID: AlertGCFailed, Active: f.GCFailed},
	}
}

// Transition is an activation or clearing.
type Transition struct {
	ID        AlertID
	Activated bool
}

// Tracker keeps alert state across evaluations (activation time) and
// reports transitions (WBS 18.118: activate / clear deterministically).
type Tracker struct {
	since map[AlertID]time.Time
}

// NewTracker builds an empty tracker.
func NewTracker() *Tracker { return &Tracker{since: map[AlertID]time.Time{}} }

// Update applies an evaluation at now, returning current alerts (with
// Since for active ones) and the transitions.
func (t *Tracker) Update(now time.Time, alerts []Alert) ([]Alert, []Transition) {
	var tr []Transition
	out := make([]Alert, len(alerts))
	for i, a := range alerts {
		_, was := t.since[a.ID]
		switch {
		case a.Active && !was:
			t.since[a.ID] = now
			tr = append(tr, Transition{ID: a.ID, Activated: true})
		case !a.Active && was:
			delete(t.since, a.ID)
			tr = append(tr, Transition{ID: a.ID, Activated: false})
		}
		if a.Active {
			a.Since = t.since[a.ID]
		}
		out[i] = a
	}
	return out, tr
}

// DependencyClass classifies what a dependency is needed for (WBS 18.5).
type DependencyClass string

const (
	RequiredForStartup   DependencyClass = "REQUIRED_FOR_STARTUP"
	RequiredForExecution DependencyClass = "REQUIRED_FOR_EXECUTION"
	Optional             DependencyClass = "OPTIONAL"
)

// Dependency is one external dependency of the process.
type Dependency struct {
	Name  string          `json:"name"`
	Class DependencyClass `json:"class"`
}

// Dependencies returns the classification of the overseer's
// dependencies: PostgreSQL (and its schema) gate startup; worker and
// compression providers and project repositories gate execution only, so
// completed projects stay inspectable without them; precomputation is
// optional.
func Dependencies(workerProviders, compressionProviders []string) []Dependency {
	out := []Dependency{{Name: "postgresql", Class: RequiredForStartup}, {Name: "schema", Class: RequiredForStartup},
		{Name: "repository", Class: RequiredForExecution}, {Name: "precompute", Class: Optional}}
	for _, w := range workerProviders {
		out = append(out, Dependency{Name: "worker:" + w, Class: RequiredForExecution})
	}
	for _, c := range compressionProviders {
		out = append(out, Dependency{Name: "compression:" + c, Class: RequiredForExecution})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
