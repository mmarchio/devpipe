package service

import (
	"context"

	"overseer/internal/persistence"
)

// DefaultRequiredRoles are the five default workflow roles required by a
// project that uses the initial default workflow (WBS 5.20). They are the
// project-configurable required set, not a hard-coded global mandate.
var DefaultRequiredRoles = []string{
	"ARCHITECT", "PLANNER", "ENGINEER", "TESTER", "PROJECT_MANAGER",
}

// RoleReadinessReport reports whether a project's required roles are all
// present, enabled, and valid (WBS 5.20).
type RoleReadinessReport struct {
	Ready    bool
	Failures []RoleReadinessFailure
}

// RoleReadinessFailure is one role readiness problem.
type RoleReadinessFailure struct {
	RoleKey string
	Reason  string
}

// CheckRoleReadiness verifies the project's required roles are all present,
// enabled, and valid (WBS 5.20). requiredKeys come from project configuration
// (DefaultRequiredRoles for the initial default workflow). A required role
// that is missing, disabled, or invalid fails readiness (T5.20.2–T5.20.4); a
// disabled OPTIONAL role does not fail readiness (T5.20.5).
func (s *RoleService) CheckRoleReadiness(ctx context.Context, projectID string, requiredKeys []string) (*RoleReadinessReport, error) {
	rep := &RoleReadinessReport{}
	pid := parseRoleID(projectID)
	roles, err := s.roles.ListByScope(ctx, &pid)
	if err != nil {
		return nil, err
	}
	byKey := make(map[string]*persistence.RoleRecord, len(roles))
	for _, r := range roles {
		byKey[r.Key] = r
	}
	for _, key := range requiredKeys {
		r, ok := byKey[key]
		if !ok {
			rep.Failures = append(rep.Failures, RoleReadinessFailure{RoleKey: key, Reason: "missing required role"})
			continue
		}
		if !r.Enabled {
			rep.Failures = append(rep.Failures, RoleReadinessFailure{RoleKey: key, Reason: "disabled required role"})
			continue
		}
		if problems := s.ValidateRole(ctx, r.ID.String()); len(problems) > 0 {
			rep.Failures = append(rep.Failures, RoleReadinessFailure{RoleKey: key, Reason: "invalid required role"})
		}
	}
	rep.Ready = len(rep.Failures) == 0
	return rep, nil
}
