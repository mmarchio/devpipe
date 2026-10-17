package service

import (
	"context"
	"sort"
	"time"

	"overseer/internal/persistence"
	"overseer/internal/recovery"
)

// StartupRecoveryScan discovers every unfinished durable operation and
// classifies it (WBS 18.6 / 18.7). It is read-only.
type StartupRecoveryScan struct {
	repo     persistence.RecoveryScanRepository
	now      func() time.Time
	instance string
}

// NewStartupRecoveryScan builds the scan; instance identifies this
// process (its own leases / precompute jobs are not orphans).
func NewStartupRecoveryScan(repo persistence.RecoveryScanRepository, instance string, now func() time.Time) *StartupRecoveryScan {
	if now == nil {
		now = time.Now
	}
	return &StartupRecoveryScan{repo: repo, now: now, instance: instance}
}

// ScanResult is the classified scan.
type ScanResult struct {
	Facts []recovery.Facts
	// LiveLeases are executions driven by another live instance (left
	// alone, WBS 18.13).
	LiveLeases []string
}

// Scan reads and classifies.
func (s *StartupRecoveryScan) Scan(ctx context.Context) (*ScanResult, error) {
	d, err := s.repo.Scan(ctx)
	if err != nil {
		return nil, translate(err)
	}
	return Classify(d, s.now(), s.instance), nil
}

// Classify maps scan data to recovery facts (pure, deterministic).
func Classify(d *persistence.RecoveryScanData, now time.Time, instance string) *ScanResult {
	res := &ScanResult{}
	add := func(exec string, c recovery.Condition, ev map[string]any) {
		res.Facts = append(res.Facts, recovery.Facts{ExecutionID: exec, Condition: c, Evidence: ev})
	}
	execOf := map[string]string{} // project → execution
	for _, e := range d.Executions {
		execOf[e.ProjectID.String()] = e.ID.String()
		id := e.ID.String()
		switch {
		case e.Status == "RECOVERY_REQUIRED":
			add(id, recovery.ConditionForReason(e.RecoveryReason, e.Evidence), map[string]any{"reason": e.RecoveryReason})
		case e.DriverOwner != "" && e.DriverOwner != instance && e.DriverExpiresAt != nil && e.DriverExpiresAt.After(now):
			res.LiveLeases = append(res.LiveLeases, id)
		case e.DriverOwner != "" && e.DriverExpiresAt != nil && !e.DriverExpiresAt.After(now):
			add(id, recovery.ExpiredLease, map[string]any{"owner": e.DriverOwner, "expired_at": e.DriverExpiresAt.UTC()})
		case e.Status == "RUNNING" || e.Status == "WAITING":
			add(id, recovery.Resumable, map[string]any{"status": e.Status})
		default:
			add(id, recovery.Resumable, map[string]any{"status": e.Status, "bootstrap": true})
		}
	}
	execFor := func(projectID string, exec *string) string {
		if exec != nil {
			return *exec
		}
		return execOf[projectID]
	}
	for _, x := range d.Dispatches {
		var ex *string
		if x.ExecutionID != nil {
			v := x.ExecutionID.String()
			ex = &v
		}
		ph := recovery.DispatchPhaseOf(x.Status, x.Delivery)
		add(execFor(x.ProjectID.String(), ex), recovery.ConditionForDispatch(ph), map[string]any{"dispatch_id": x.ID.String(),
			"dispatch_phase": string(ph), "status": x.Status, "delivery": x.Delivery, "state": x.StateKey})
	}
	for _, it := range d.Items {
		var ex *string
		if it.ExecutionID != nil {
			v := it.ExecutionID.String()
			ex = &v
		}
		exec := execFor(it.ProjectID.String(), ex)
		ev := map[string]any{"id": it.ID.String(), "kind": it.Kind}
		for k, v := range it.Detail {
			ev[k] = v
		}
		switch it.Kind {
		case "RUNNING_VALIDATION":
			add(exec, recovery.RunningValidation, ev)
		case "PENDING_CHECKPOINT":
			add(exec, recovery.PendingCheckpoint, ev)
		case "INTENT_STEP":
			// A dispatch intent without a dispatch row never reached the
			// worker (durable intent precedes the send).
			if it.Detail["action"] == "DISPATCH" || it.Detail["action"] == "PLAN_DISPATCH" {
				if it.Detail["dispatch_id"] == nil {
					add(exec, recovery.DispatchNotSent, ev)
				}
				continue
			}
			add(exec, recovery.Resumable, ev)
		case "ACTIVE_CONTINUATION":
			add(exec, recovery.Resumable, ev)
		case "PRECOMPUTE_JOB":
			if owner, _ := it.Detail["owner"].(string); owner != instance {
				add(exec, recovery.OrphanPrecompute, ev)
			}
		}
	}
	sort.SliceStable(res.Facts, func(i, j int) bool {
		a, b := res.Facts[i], res.Facts[j]
		if a.ExecutionID != b.ExecutionID {
			return a.ExecutionID < b.ExecutionID
		}
		if a.Condition != b.Condition {
			return a.Condition < b.Condition
		}
		ia, _ := a.Evidence["id"].(string)
		ib, _ := b.Evidence["id"].(string)
		if ia == "" {
			ia, _ = a.Evidence["dispatch_id"].(string)
		}
		if ib == "" {
			ib, _ = b.Evidence["dispatch_id"].(string)
		}
		return ia < ib
	})
	sort.Strings(res.LiveLeases)
	return res
}
