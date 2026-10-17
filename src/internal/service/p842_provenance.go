// P8-42 (WBS 8.37): the prompt provenance read op.
//
// PromptProvenanceService exposes a persisted canonical prompt's source
// components for inspection. The provenance is a pure projection over the
// persisted row's PINNED captures (the P8-34 shape): every entry's identity,
// version, and hash is read from the pinned version rows / snapshot pins at
// the recorded version — a source advance after composition never moves an
// entry. Project isolation mirrors the P8-41 by-id surface (a foreign prompt
// id is ErrNotFound).
package service

import (
	"context"
	"encoding/json"
	"errors"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
	"overseer/internal/prompt"
	"overseer/internal/role"
)

// PromptProvenance is the read model for one prompt's source set (WBS 8.37):
// the prompt's identity + state key plus every source the prompt was
// composed from, in the WBS 8.9 canonical order.
type PromptProvenance struct {
	PromptID uuid.UUID
	StateKey string

	Entries []prompt.ProvenanceEntry
}

// PromptProvenanceService reads a canonical prompt's provenance.
type PromptProvenanceService struct {
	canonical  persistence.CanonicalPromptRepository
	snapshots  persistence.ContextSnapshotRepository
	sysPrompts persistence.SystemPromptRepository
	protocols  persistence.ResponseProtocolRepository
	states     persistence.WorkflowStateRepository
	pool       *pgxpool.Pool
}

// NewPromptProvenanceService wires the provenance read op. The pool carries
// the one direct query (state → protocol reference; the repository
// interfaces expose no list-by-state method — the P8-38 pattern); a nil pool
// simply omits the protocol entry.
func NewPromptProvenanceService(
	canonical persistence.CanonicalPromptRepository,
	snapshots persistence.ContextSnapshotRepository,
	sysPrompts persistence.SystemPromptRepository,
	protocols persistence.ResponseProtocolRepository,
	states persistence.WorkflowStateRepository,
	pool *pgxpool.Pool,
) *PromptProvenanceService {
	return &PromptProvenanceService{
		canonical: canonical, snapshots: snapshots, sysPrompts: sysPrompts,
		protocols: protocols, states: states, pool: pool,
	}
}

// provenanceRank is the WBS 8.9 canonical order projected onto the source
// classes a provenance entry can carry (the same fixed order the composer
// uses — P8-15).
var provenanceRank = map[prompt.PromptComponentType]int{
	prompt.ComponentSystem:           1,
	prompt.ComponentRoleInstruction:  2,
	prompt.ComponentProjectContext:   4,
	prompt.ComponentPhaseContext:     5,
	prompt.ComponentTaskContext:      7,
	prompt.ComponentResponseProtocol: 11,
}

// Get returns the prompt's provenance (WBS 8.37). The {prompt} id must
// belong to the URL's project (a foreign id is ErrNotFound — T8.36.4
// semantics, reused).
func (s *PromptProvenanceService) Get(ctx context.Context, projectID, promptID string) (*PromptProvenance, error) {
	pid, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	rid, err := uuid.Parse(promptID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	rec, err := s.canonical.Get(ctx, rid)
	if err != nil {
		return nil, translate(err)
	}
	if rec.ProjectID != pid {
		return nil, ErrNotFound
	}
	snap, err := s.snapshots.Get(ctx, rec.ContextSnapshotID)
	if err != nil {
		return nil, translate(err)
	}
	roleSnap, err := decodeProvenanceRoleSnapshot(snap.RoleSnapshotJSON)
	if err != nil {
		return nil, ErrInfrastructure
	}

	var entries []prompt.ProvenanceEntry

	// The role's system prompt at the PINNED version (the version row's
	// content hash — T8.37.3). A cleared captured version yields no entry
	// (the P8-18 zero-component policy).
	sp := roleSnap.SystemPrompt
	if sp.Version > 0 && sp.Content != "" {
		row, err := s.sysPrompts.GetVersion(ctx, roleSnap.RoleID.String(), sp.Version)
		if err != nil {
			return nil, translate(err)
		}
		entries = append(entries, prompt.ProvenanceEntry{
			ComponentType: prompt.ComponentSystem,
			SourceID:      roleSnap.RoleID,
			SourceVersion: sp.Version,
			SourceHash:    row.ContentHash,
		})
	}

	// The role's assigned instructions at their pinned versions (the
	// snapshot carries the captured hash verbatim — T8.37.3).
	for _, ins := range roleSnap.Instructions {
		if ins.Version <= 0 || ins.Content == "" {
			continue
		}
		entries = append(entries, prompt.ProvenanceEntry{
			ComponentType: prompt.ComponentRoleInstruction,
			SourceID:      roleSnap.RoleID,
			SourceVersion: ins.Version,
			SourceHash:    ins.ContentHash,
		})
	}

	// The pinned state files: the snapshot pins carry the version + hash
	// verbatim (T8.37.2/T8.37.3), in the P8-19 deterministic order (section
	// rank, then type key, then file id).
	fileEntries := make([]prompt.ProvenanceEntry, 0, len(snap.Files))
	for _, f := range snap.Files {
		if f.Version <= 0 || f.Content == "" {
			continue
		}
		fileEntries = append(fileEntries, prompt.ProvenanceEntry{
			ComponentType: provenanceClassificationType(role.Classification(f.Classification)),
			SourceID:      f.StateFileID,
			SourceVersion: f.Version,
			SourceHash:    f.Hash,
		})
	}
	sort.SliceStable(fileEntries, func(i, j int) bool {
		ri, rj := provenanceRank[fileEntries[i].ComponentType], provenanceRank[fileEntries[j].ComponentType]
		if ri != rj {
			return ri < rj
		}
		return fileEntries[i].SourceID.String() < fileEntries[j].SourceID.String()
	})
	entries = append(entries, fileEntries...)

	// The bound work unit: no version series and no content hash (the
	// work_units table carries neither — the P8-14 component shape).
	if rec.WorkUnitID != nil {
		entries = append(entries, prompt.ProvenanceEntry{
			ComponentType: prompt.ComponentTaskContext,
			SourceID:      *rec.WorkUnitID,
			SourceVersion: 0,
			SourceHash:    "",
		})
	}

	// The state's pinned response protocol at its pinned version (the
	// version row's content hash — T8.37.3).
	if protoID, protoVer, err := s.stateProtocolRef(ctx, rec); err == nil {
		ver, err := s.protocols.GetVersion(ctx, protoID, protoVer)
		if err != nil {
			return nil, translate(err)
		}
		entries = append(entries, prompt.ProvenanceEntry{
			ComponentType: prompt.ComponentResponseProtocol,
			SourceID:      protoID,
			SourceVersion: protoVer,
			SourceHash:    ver.ContentHash,
		})
	} else if !isNotFound(err) {
		return nil, err
	}

	// WBS 8.9 canonical order (stable within a type — T8.37.4).
	sort.SliceStable(entries, func(i, j int) bool {
		return provenanceRank[entries[i].ComponentType] < provenanceRank[entries[j].ComponentType]
	})

	return &PromptProvenance{
		PromptID: rec.ID,
		StateKey: rec.StateKey,
		Entries:  entries,
	}, nil
}

// isNotFound reports whether err is a domain/service not-found (the
// protocol reference is optional — a prompt whose state pins no protocol,
// or whose state row is gone, has no protocol entry).
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, persistence.ErrNotFound) || errors.Is(err, ErrNotFound)
}

// stateProtocolRef resolves the prompt's state id (machine id + version +
// state key) and returns its pinned response protocol (id, version). A
// state with no pinned protocol is not an error (the optional case).
func (s *PromptProvenanceService) stateProtocolRef(ctx context.Context, rec *persistence.CanonicalPromptRecord) (uuid.UUID, int, error) {
	if s.pool == nil {
		return uuid.Nil, 0, persistence.ErrNotFound
	}
	states, err := s.states.ListByMachine(ctx, rec.StateMachineID, rec.StateMachineVersion)
	if err != nil {
		return uuid.Nil, 0, translate(err)
	}
	var stateID uuid.UUID
	for _, st := range states {
		if st.Key == rec.StateKey {
			stateID = st.ID
			break
		}
	}
	if stateID == uuid.Nil {
		return uuid.Nil, 0, persistence.ErrNotFound
	}
	var protoID uuid.UUID
	var protoVer int32
	err = s.pool.QueryRow(ctx,
		`SELECT protocol_id, protocol_version FROM state_protocol_references WHERE state_id = $1`, stateID).
		Scan(&protoID, &protoVer)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, 0, persistence.ErrNotFound
		}
		return uuid.Nil, 0, translate(err)
	}
	return protoID, int(protoVer), nil
}

// provenanceClassificationType maps a snapshot pin's WBS 7.36 classification
// to its component type (the P8-19 mapping).
func provenanceClassificationType(c role.Classification) prompt.PromptComponentType {
	switch c {
	case role.ClassificationProjectContext:
		return prompt.ComponentProjectContext
	case role.ClassificationPhaseContext:
		return prompt.ComponentPhaseContext
	case role.ClassificationTaskContext:
		return prompt.ComponentTaskContext
	case role.ClassificationTestContext:
		return prompt.ComponentTestContext
	}
	return prompt.PromptComponentType(c)
}

// decodeProvenanceRoleSnapshot deserializes the snapshot's role-snapshot
// JSONB blob.
func decodeProvenanceRoleSnapshot(raw []byte) (role.RoleSnapshot, error) {
	var out role.RoleSnapshot
	if len(raw) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return role.RoleSnapshot{}, err
	}
	return out, nil
}
