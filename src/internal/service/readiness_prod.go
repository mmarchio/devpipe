package service

import (
	"context"
	"sort"
	"time"
)

// Production readiness (WBS 18.207–18.209).

// Readiness check outcomes and overall statuses.
const (
	CheckPass = "PASS"
	CheckWarn = "WARN"
	CheckFail = "FAIL"

	ReadinessReady             = "READY"
	ReadinessReadyWithWarnings = "READY_WITH_WARNINGS"
	ReadinessNotReady          = "NOT_READY"
)

// ReadinessItem is one check's outcome. Details name settings and
// resources, never secret values (WBS 18.208).
type ReadinessItem struct {
	Name   string `json:"name"`
	Area   string `json:"area"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// ProductionCheck evaluates one requirement.
type ProductionCheck func(ctx context.Context) []ReadinessItem

// ProductionReadiness is the report.
type ProductionReadiness struct {
	Status      string          `json:"status"`
	Checks      []ReadinessItem `json:"checks"`
	GeneratedAt time.Time       `json:"generated_at"`
}

// FoldReadiness: any FAIL → NOT_READY, else any WARN →
// READY_WITH_WARNINGS, else READY.
func FoldReadiness(items []ReadinessItem) string {
	st := ReadinessReady
	for _, i := range items {
		switch i.Status {
		case CheckFail:
			return ReadinessNotReady
		case CheckWarn:
			st = ReadinessReadyWithWarnings
		}
	}
	return st
}

// ProductionReadinessService runs every production check (database,
// migrations, backup, providers, repository roots, security, limits,
// audit, recovery, health) and folds them.
type ProductionReadinessService struct {
	checks []ProductionCheck
	now    func() time.Time
}

// NewProductionReadinessService builds the service.
func NewProductionReadinessService(checks ...ProductionCheck) *ProductionReadinessService {
	return &ProductionReadinessService{checks: checks, now: time.Now}
}

// Report evaluates readiness now.
func (s *ProductionReadinessService) Report(ctx context.Context) *ProductionReadiness {
	var items []ReadinessItem
	for _, c := range s.checks {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		items = append(items, c(cctx)...)
		cancel()
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Area != items[j].Area {
			return items[i].Area < items[j].Area
		}
		return items[i].Name < items[j].Name
	})
	if items == nil {
		items = []ReadinessItem{}
	}
	return &ProductionReadiness{Status: FoldReadiness(items), Checks: items, GeneratedAt: s.now().UTC()}
}
