package service

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/invariants"
	"overseer/internal/persistence"
)

// CommitChecker verifies checkpoint commits in a repository (WBS 18.203).
type CommitChecker interface {
	CommitExists(ctx context.Context, dir, commit string) (bool, error)
}

// IntegrityService runs the read-only invariant scan (WBS 18.39 / 18.40 /
// 18.171 / 18.181 / 18.203): database invariants plus the existence of
// every created checkpoint's commit. It reports; it never repairs.
type IntegrityService struct {
	scanner  *dbrepo.IntegrityScanner
	settings persistence.SettingsRepository
	commits  CommitChecker
}

// NewIntegrityService wires the service (commits nil = skip the repository
// object check).
func NewIntegrityService(scanner *dbrepo.IntegrityScanner, settings persistence.SettingsRepository, commits CommitChecker) *IntegrityService {
	return &IntegrityService{scanner: scanner, settings: settings, commits: commits}
}

// Scan scans the system, one project, or one execution.
func (s *IntegrityService) Scan(ctx context.Context, projectID, executionID *uuid.UUID) (invariants.Report, error) {
	scope := "system"
	switch {
	case executionID != nil:
		scope = "execution:" + executionID.String()
	case projectID != nil:
		scope = "project:" + projectID.String()
	}
	sc := dbrepo.IntegrityScope{ProjectID: projectID, ExecutionID: executionID}
	checked, vs, err := s.scanner.Scan(ctx, sc)
	if err != nil {
		return invariants.Report{}, translate(err)
	}
	if s.commits != nil {
		more, err := s.missingCommits(ctx, sc)
		if err != nil {
			return invariants.Report{}, err
		}
		vs = append(vs, more...)
	}
	return invariants.NewReport(scope, checked, vs), nil
}

// missingCommits reports created checkpoints whose commit object is gone
// from the project's repository (a DONE invariant violation, WBS 18.203).
// A repository that cannot be read is not judged here (the repository
// guard reports it before any side effect).
func (s *IntegrityService) missingCommits(ctx context.Context, sc dbrepo.IntegrityScope) ([]invariants.Violation, error) {
	cps, err := s.scanner.CreatedCheckpoints(ctx, sc)
	if err != nil {
		return nil, translate(err)
	}
	dirs := map[string]string{}
	var out []invariants.Violation
	for _, c := range cps {
		dir, ok := dirs[c[1]]
		if !ok {
			if ps, err := s.settings.Get(ctx, c[1]); err == nil {
				dir = ps.WorkingDirectory
			}
			dirs[c[1]] = dir
		}
		if dir == "" {
			continue
		}
		exists, err := s.commits.CommitExists(ctx, dir, c[3])
		if err != nil || exists {
			continue
		}
		out = append(out, invariants.Violation{Invariant: invariants.CheckpointConsistent, ProjectID: c[1], ExecutionID: c[2],
			EntityIDs: []string{c[0]}, Detail: fmt.Sprintf("checkpoint commit %s is missing from the repository", c[3])})
	}
	return out, nil
}
