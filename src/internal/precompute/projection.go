package precompute

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// FilePin is one projected state-file pin.
type FilePin struct {
	TypeKey     string `json:"type_key"`
	Version     int    `json:"version"`
	Hash        string `json:"hash"`
	Placeholder bool   `json:"placeholder,omitempty"`
}

// ProjectedContext describes what the prompt context would be if a valid
// future transition occurs (WBS 11.20). It references a real base
// snapshot and is never a snapshot itself.
type ProjectedContext struct {
	ID uuid.UUID

	BaseSnapshotID   uuid.UUID
	BaseSemanticHash string

	ProjectID uuid.UUID
	PhaseID   *uuid.UUID

	ProjectedWorkUnitID *uuid.UUID
	ProjectedStateKey   string
	ProjectedRoleKey    string

	TransitionPath []string
	Pins           []FilePin

	ProjectionHash string

	CreatedAt time.Time
}

func frame(dst []byte, tag string, payload []byte) []byte {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(tag)+len(payload)))
	dst = append(dst, n[:]...)
	dst = append(dst, tag...)
	return append(dst, payload...)
}

func intBytes(v int) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(v))
	return b[:]
}

// ProjectionHash is the deterministic projection identity (WBS 11.22): base
// snapshot semantic hash, target state, target role, projected unit and
// the projected file versions (sorted by type).
func ProjectionHash(baseSemanticHash, state, roleKey string, unit *uuid.UUID, pins []FilePin) string {
	ps := append([]FilePin(nil), pins...)
	sort.Slice(ps, func(i, j int) bool { return ps[i].TypeKey < ps[j].TypeKey })
	buf := frame(nil, "base", []byte(baseSemanticHash))
	buf = frame(buf, "state", []byte(state))
	buf = frame(buf, "role", []byte(roleKey))
	buf = frame(buf, "unit", []byte(unitStr(unit)))
	for _, p := range ps {
		buf = frame(buf, "pin_type", []byte(p.TypeKey))
		buf = frame(buf, "pin_version", intBytes(p.Version))
		buf = frame(buf, "pin_hash", []byte(p.Hash))
	}
	s := sha256.Sum256(buf)
	return hex.EncodeToString(s[:])
}

// ErrInvalidProjection rejects an invalid projection.
var ErrInvalidProjection = errors.New("precompute: invalid projection")

// NewProjectedContext validates a projection and computes its hash
// (T11.20.1, T11.22.1-T11.22.4).
func NewProjectedContext(p ProjectedContext) (ProjectedContext, error) {
	if p.BaseSnapshotID == uuid.Nil || p.BaseSemanticHash == "" || p.ProjectID == uuid.Nil || strings.TrimSpace(p.ProjectedStateKey) == "" {
		return ProjectedContext{}, ErrInvalidProjection
	}
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	p.Pins = append([]FilePin(nil), p.Pins...)
	p.TransitionPath = append([]string(nil), p.TransitionPath...)
	p.ProjectionHash = ProjectionHash(p.BaseSemanticHash, p.ProjectedStateKey, p.ProjectedRoleKey, p.ProjectedWorkUnitID, p.Pins)
	return p, nil
}

// ValidationInput is the resolved evidence for projection validation (WBS
// 11.21).
type ValidationInput struct {
	Graph       Graph
	StartState  string
	Path        []string
	TargetState string

	RoleAssigned bool
	RoleEnabled  bool

	BasePhaseID      *uuid.UUID
	ProjectedPhaseID *uuid.UUID

	UnitBound      bool
	ProjectedUnit  *uuid.UUID
	UnitEligible   bool
	UnitIsCurrent  bool
	StaticFilesOK  bool
	NextUnitTarget bool
	// NextPhaseTarget: a next-phase planning projection (WBS 11.32/11.34)
	// — the only projection allowed to change the phase.
	NextPhaseTarget bool
}

// ValidateProjection checks a projection before use (T11.21.1-T11.21.4,
// T11.20.2/T11.20.3).
func ValidateProjection(in ValidationInput) error {
	if st, ok := in.Graph.States[in.TargetState]; !ok || !st.Enabled {
		return fmt.Errorf("%w: target state %q not an enabled state of machine v%d", ErrInvalidProjection, in.TargetState, in.Graph.MachineVersion)
	}
	if !in.NextUnitTarget && !in.NextPhaseTarget && !in.Graph.HasPath(in.StartState, in.Path) {
		return fmt.Errorf("%w: no valid transition path %v from %s", ErrInvalidProjection, in.Path, in.StartState)
	}
	if !in.RoleAssigned || !in.RoleEnabled {
		return fmt.Errorf("%w: no enabled role for %s", ErrInvalidProjection, in.TargetState)
	}
	if !in.NextPhaseTarget && unitStr(in.BasePhaseID) != unitStr(in.ProjectedPhaseID) {
		return fmt.Errorf("%w: projection changes the phase", ErrInvalidProjection)
	}
	if in.UnitBound && in.ProjectedUnit == nil {
		return fmt.Errorf("%w: unit-bound state without a work unit", ErrInvalidProjection)
	}
	if in.UnitBound && !in.UnitIsCurrent && !in.UnitEligible {
		return fmt.Errorf("%w: projected work unit is dependency-blocked", ErrInvalidProjection)
	}
	if !in.StaticFilesOK {
		return fmt.Errorf("%w: required static state files not resolvable", ErrInvalidProjection)
	}
	return nil
}
