package service

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/role"
	"overseer/internal/statefile"
)

// ErrContinuationBinding reports a continuation snapshot whose work unit,
// phase or runtime state no longer matches the authoritative runtime
// position (WBS 14.66-14.68).
var ErrContinuationBinding = errors.New("service: continuation binding no longer matches the runtime position")

// CreateContinuationSnapshot creates a new execution-context snapshot for
// a continuation (P14-41, WBS 14.27, 14.70-14.72): same project, phase,
// work unit and runtime state as the source snapshot, the ORIGINAL role
// snapshot of the source (the continuation chain is not silently changed
// by role edits), and the state files at their CURRENT versions (derived
// files may legitimately advance between attempts). The source snapshot
// is never modified.
func (s *ExecutionSnapshotService) CreateContinuationSnapshot(ctx context.Context, projectID, sourceSnapshotID uuid.UUID) (*statefile.ExecutionContextSnapshot, error) {
	if s.snapshots == nil || s.fileVersions == nil || s.roleSvc == nil || s.scopeRes == nil {
		return nil, ErrInfrastructure
	}
	src, err := s.snapshots.Get(ctx, sourceSnapshotID)
	if err != nil {
		return nil, translate(err)
	}
	if src.ProjectID != projectID {
		return nil, ErrNotFound
	}
	var roleSnap role.RoleSnapshot
	if err := json.Unmarshal(src.RoleSnapshotJSON, &roleSnap); err != nil {
		return nil, ErrInfrastructure
	}
	var out *statefile.ExecutionContextSnapshot
	run := func(tx persistence.Transaction) error {
		rt := s.runtime
		if tx != nil {
			rt = s.runtime.WithTx(tx)
		}
		rtRec, err := rt.Get(ctx, projectID)
		if err != nil {
			return translate(err)
		}
		if rtRec.ID != src.RuntimeStateID || !sameUUID(rtRec.PhaseID, src.PhaseID) || !sameUUID(rtRec.WorkUnitID, src.WorkUnitID) {
			return ErrContinuationBinding
		}
		snap, err := s.assemble(ctx, tx, projectID, rtRec)
		if err != nil {
			return err
		}
		snap.RoleSnapshot = roleSnap
		snap.StateKey = src.StateKey
		snap.StateMachineID, snap.StateMachineVersion = src.StateMachineID, src.StateMachineVersion
		if err := s.persist(ctx, tx, snap); err != nil {
			return err
		}
		out = snap
		return nil
	}
	if s.txPool == nil {
		err = run(nil)
	} else {
		err = dbrepo.Run(ctx, s.txPool, run)
	}
	return out, err
}

func sameUUID(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
