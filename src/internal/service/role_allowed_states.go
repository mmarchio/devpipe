package service

import (
	"context"
	"sort"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
)

// ListAllowedStates returns the role's allowed state keys in stable sorted
// order (WBS 5.25 read path); a missing role is ErrNotFound.
func (s *RoleService) ListAllowedStates(ctx context.Context, roleID string) ([]string, error) {
	if s.allowedStates == nil {
		return nil, ErrInfrastructure
	}
	if _, err := s.roles.Get(ctx, roleID); err != nil {
		return nil, err
	}
	recs, err := s.allowedStates.ListByRole(ctx, mustUUID(roleID))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(recs))
	for _, rec := range recs {
		out = append(out, rec.StateKey)
	}
	return out, nil
}

// SetAllowedStates atomically replaces the role's allowed-state set with the
// given keys (WBS 5.25): the previous assignments are removed and the new set
// assigned in a single transaction (T5.25.2). An empty set is rejected
// (T5.25.4); duplicate keys are normalized to the unique set, order-
// insensitive (T5.25.3). Returns the resulting set in stable sorted order.
func (s *RoleService) SetAllowedStates(ctx context.Context, roleID string, states []string) ([]string, error) {
	if s.allowedStates == nil {
		return nil, ErrInfrastructure
	}
	if len(states) == 0 {
		return nil, ErrInvalidInput
	}
	// Normalize: unique set (duplicates collapsed; the set is order-
	// insensitive so no input order is honored).
	unique := map[string]struct{}{}
	for _, k := range states {
		unique[k] = struct{}{}
	}
	if _, err := s.roles.Get(ctx, roleID); err != nil {
		return nil, err
	}
	uid := mustUUID(roleID)
	if s.txPool == nil {
		// Sequential fallback: delete-then-insert per statement.
		if err := s.replaceAllStates(ctx, nil, uid, unique); err != nil {
			return nil, err
		}
	} else {
		if err := dbrepo.Run(ctx, s.txPool, func(tx persistence.Transaction) error {
			return s.replaceAllStates(ctx, tx, uid, unique)
		}); err != nil {
			return nil, err
		}
	}
	recs, err := s.allowedStates.ListByRole(ctx, uid)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(recs))
	for _, rec := range recs {
		out = append(out, rec.StateKey)
	}
	sort.Strings(out)
	return out, nil
}

// replaceAllStates performs the replace inside the given transaction (nil =
// autocommit): delete every current assignment, then insert each unique key.
func (s *RoleService) replaceAllStates(ctx context.Context, tx persistence.Transaction, roleID uuid.UUID, states map[string]struct{}) error {
	repo := s.allowedStates
	if tx != nil {
		repo = s.allowedStates.WithTx(tx)
	}
	cur, err := repo.ListByRole(ctx, roleID)
	if err != nil {
		return err
	}
	for _, rec := range cur {
		if err := repo.UnassignState(ctx, roleID, rec.StateKey); err != nil {
			return err
		}
	}
	var keys []string
	for k := range states {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, err := repo.AssignState(ctx, roleID, k); err != nil {
			return err
		}
	}
	return nil
}
