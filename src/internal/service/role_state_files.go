package service

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
)

// SetStateFiles atomically replaces the role's required state files with the
// given ordered list (WBS 5.26). Unlike allowed states (an order-insensitive
// set, P5-25), state-file order IS meaningful: the list is stored in the exact
// input order (T5.26.2). A duplicate key in the input is rejected (T5.26.3) —
// not normalized — because order is significant; an empty list is rejected as
// invalid input, and a missing role is ErrNotFound (T5.26.4). The previous
// requirements are removed and the new ordered list assigned in a single
// transaction. Returns the stored list in its stored (position) order.
func (s *RoleService) SetStateFiles(ctx context.Context, roleID string, stateFiles []string) ([]string, error) {
	if s.stateFiles == nil {
		return nil, ErrInfrastructure
	}
	if len(stateFiles) == 0 {
		return nil, ErrInvalidInput
	}
	seen := make(map[string]struct{}, len(stateFiles))
	for _, k := range stateFiles {
		if _, ok := seen[k]; ok {
			return nil, fmt.Errorf("duplicate state file %q: %w", k, ErrInvalidInput)
		}
		seen[k] = struct{}{}
	}
	if _, err := s.roles.Get(ctx, roleID); err != nil {
		return nil, err
	}
	uid := mustUUID(roleID)
	if s.txPool == nil {
		// Sequential fallback: delete-then-insert per statement.
		if err := s.replaceAllStateFiles(ctx, nil, uid, stateFiles); err != nil {
			return nil, err
		}
	} else {
		if err := dbrepo.Run(ctx, s.txPool, func(tx persistence.Transaction) error {
			return s.replaceAllStateFiles(ctx, tx, uid, stateFiles)
		}); err != nil {
			return nil, err
		}
	}
	re, err := s.stateFiles.ListByRole(ctx, uid)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(re))
	for _, r := range re {
		out = append(out, r.StateFileKey)
	}
	return out, nil
}

// replaceAllStateFiles performs the replace inside the given transaction (nil
// = autocommit): delete every current requirement, then require each key in
// the input order so stored position matches input order.
func (s *RoleService) replaceAllStateFiles(ctx context.Context, tx persistence.Transaction, roleID uuid.UUID, stateFiles []string) error {
	repo := s.stateFiles
	if tx != nil {
		repo = s.stateFiles.WithTx(tx)
	}
	cur, err := repo.ListByRole(ctx, roleID)
	if err != nil {
		return err
	}
	for _, rec := range cur {
		if err := repo.Remove(ctx, roleID, rec.StateFileKey); err != nil {
			return err
		}
	}
	for _, k := range stateFiles {
		if _, err := repo.Require(ctx, roleID, k); err != nil {
			return err
		}
	}
	return nil
}
