package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"overseer/internal/access"
	"overseer/internal/audit"
	"overseer/internal/persistence"
)

// Restore states (migration 00113 / 00122).
const (
	RestoreValidationPending = "VALIDATION_PENDING"
	RestoreValidated         = "VALIDATED"
	RestoreFailed            = "FAILED"
	RestoreEnabled           = "ENABLED"
)

// GateRestore blocks new work on a restored database until an operator
// enables execution (decision U4).
const GateRestore = "restore"

// RestoreStateStore persists the restore gate.
type RestoreStateStore interface {
	Get(ctx context.Context) (*persistence.RestoreState, error)
	RecordValidation(ctx context.Context, status string, detail map[string]any, at time.Time) error
	Enable(ctx context.Context, by, reason string, at time.Time) error
}

// RestoreCheck is one startup check of a restored database. It returns
// whether the check passed and a JSON-serializable report.
type RestoreCheck struct {
	Name string
	Run  func(ctx context.Context, st *persistence.RestoreState) (ok bool, report any, err error)
}

// RestoreCheckResult is one check's outcome.
type RestoreCheckResult struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
	Report any    `json:"report,omitempty"`
}

// RestoreGateService is the startup gate of a restored database (WBS
// 18.44–18.46, decision U4): until an operator enables execution the
// instance is read-only (mutation routes refused), the scheduler is off
// and no new worker request is sent — even when every check passes. A
// failing check keeps the instance NOT_READY; it is re-checked at the next
// start.
type RestoreGateService struct {
	store  RestoreStateStore
	gate   *WorkGate
	checks []RestoreCheck
	log    *slog.Logger
	now    func() time.Time
	// onEnable runs after execution is enabled (starts the scheduler).
	onEnable func()

	mu     sync.Mutex
	active bool
	state  *persistence.RestoreState
}

// NewRestoreGateService builds the gate.
func NewRestoreGateService(store RestoreStateStore, gate *WorkGate, checks []RestoreCheck, log *slog.Logger) *RestoreGateService {
	if log == nil {
		log = slog.Default()
	}
	return &RestoreGateService{store: store, gate: gate, checks: checks, log: log, now: time.Now}
}

// OnEnable registers the action run once execution is enabled.
func (s *RestoreGateService) OnEnable(fn func()) { s.onEnable = fn }

// Startup runs the checks when the database was restored and not yet
// enabled, records the outcome and closes the gate. It reports whether
// the restore gate is active.
func (s *RestoreGateService) Startup(ctx context.Context) (bool, error) {
	st, err := s.store.Get(ctx)
	if err != nil {
		return false, err
	}
	if st == nil || st.Status == RestoreEnabled {
		s.set(false, st)
		return false, nil
	}
	results := make([]RestoreCheckResult, 0, len(s.checks))
	passed := true
	for _, c := range s.checks {
		res := RestoreCheckResult{Name: c.Name}
		ok, report, err := c.Run(ctx, st)
		res.OK, res.Report = ok && err == nil, report
		if err != nil {
			res.Error = err.Error()
		}
		passed = passed && res.OK
		results = append(results, res)
	}
	status := RestoreValidated
	if !passed {
		status = RestoreFailed
	}
	detail := map[string]any{"checks": results, "checked_at": s.now().UTC()}
	if err := s.store.RecordValidation(ctx, status, detail, s.now()); err != nil {
		return false, err
	}
	if st, err = s.store.Get(ctx); err != nil {
		return false, err
	}
	s.set(true, st)
	if passed {
		s.gate.Block(GateRestore, "restored database: startup checks passed; awaiting operator enablement (POST /api/v1/system/restore/enable)")
		s.log.Warn("restored database validated; execution stays disabled until an operator enables it", "backup_id", st.BackupID.String())
	} else {
		s.gate.Block(GateRestore, "restored database failed its startup checks: "+failedChecks(results)+"; the instance stays read-only and NOT_READY")
		s.log.Error("restored database failed its startup checks", "backup_id", st.BackupID.String(), "failed", failedChecks(results))
	}
	return true, nil
}

func failedChecks(results []RestoreCheckResult) string {
	var out []string
	for _, r := range results {
		if !r.OK {
			out = append(out, r.Name)
		}
	}
	return strings.Join(out, ", ")
}

func (s *RestoreGateService) set(active bool, st *persistence.RestoreState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active, s.state = active, st
}

// Active reports whether the restore gate holds the instance.
func (s *RestoreGateService) Active() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

// Ready reports whether the instance may report ready: false while a
// restore failed its checks (decision U4).
func (s *RestoreGateService) Ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !(s.active && s.state != nil && s.state.Status == RestoreFailed)
}

// RestoreStatus is the restore gate as the API reports it.
type RestoreStatus struct {
	Restored bool                      `json:"restored"`
	Active   bool                      `json:"active"`
	State    *persistence.RestoreState `json:"state,omitempty"`
}

// Status returns the current gate.
func (s *RestoreGateService) Status(ctx context.Context) (*RestoreStatus, error) {
	st, err := s.store.Get(ctx)
	if err != nil {
		return nil, translate(err)
	}
	return &RestoreStatus{Restored: st != nil, Active: s.Active(), State: st}, nil
}

// ErrReadOnly: the instance refuses mutations while the restore gate is
// active.
var ErrReadOnly = errors.New("the instance is read-only after a restore: review GET /api/v1/system/restore and enable execution (POST /api/v1/system/restore/enable)")

// WriteGuard refuses every mutation except enabling execution and
// acknowledging an audit failure while the gate is active.
func (s *RestoreGateService) WriteGuard(action access.Action) error {
	if !s.Active() || action == access.ActionRestoreEnable || action == access.ActionAcknowledgeAuditFailure {
		return nil
	}
	return ErrReadOnly
}

// EnableRequest is the operator's decision to enable execution.
type EnableRequest struct {
	Reason string `json:"reason"`
	By     string `json:"-"`
}

// Enable enables execution on a VALIDATED restore: the gate opens, writes
// are accepted again and the scheduler starts. A FAILED restore cannot be
// enabled (fix the cause and restart to re-check).
func (s *RestoreGateService) Enable(ctx context.Context, req EnableRequest) (*RestoreStatus, error) {
	req.Reason = strings.TrimSpace(req.Reason)
	if req.Reason == "" || len(req.Reason) > audit.MaxReason {
		return nil, fmt.Errorf("%w: a reason (1–%d characters) is required", ErrInvalidInput, audit.MaxReason)
	}
	if req.By == "" {
		return nil, fmt.Errorf("%w: enabling actor unknown", ErrInvalidInput)
	}
	st, err := s.store.Get(ctx)
	if err != nil {
		return nil, translate(err)
	}
	switch {
	case st == nil:
		return nil, fmt.Errorf("%w: this database was not restored", ErrConflict)
	case st.Status == RestoreEnabled:
		return nil, fmt.Errorf("%w: execution is already enabled", ErrConflict)
	case st.Status != RestoreValidated:
		return nil, fmt.Errorf("%w: the restore is %s; only a VALIDATED restore can be enabled (fix the failing checks and restart)", ErrConflict, st.Status)
	}
	if err := s.store.Enable(ctx, req.By, req.Reason, s.now()); err != nil {
		return nil, translate(err)
	}
	if st, err = s.store.Get(ctx); err != nil {
		return nil, translate(err)
	}
	s.set(false, st)
	s.gate.Clear(GateRestore)
	s.log.Warn("execution enabled on the restored database", "backup_id", st.BackupID.String(), "by", req.By, "reason", req.Reason)
	if s.onEnable != nil {
		s.onEnable()
	}
	return &RestoreStatus{Restored: true, Active: false, State: st}, nil
}
