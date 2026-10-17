package prompt

import (
	"errors"
	"fmt"

	"overseer/internal/role"
)

// ErrMissingInstructionVersion reports a composition failure: the
// RoleSnapshot carries an assigned instruction with no usable captured
// version (WBS 8.11; the per-instruction analogue of the 8.10
// missing-version contract).
var ErrMissingInstructionVersion = errors.New("prompt: snapshot carries an instruction with no captured version")

// ComposeRoleInstructions renders the RoleSnapshot's ordered instruction
// set (WBS 8.11): every assigned instruction becomes a ROLE_INSTRUCTION
// component at its snapshot-captured version and content, with the
// snapshot's source hash retained as SourceHash. Assignment order is
// preserved verbatim — the snapshot IS the ordered Phase 5 read model
// (WBS 5.14) and is never re-sorted here (canonical placement across
// types is the P8-15 CanonicalOrder step). Because resolution reads only
// the snapshot, a later instruction edit has no input to the composition
// and cannot affect a historical composition. A cleared captured
// version/content is a composition failure (ErrMissingInstructionVersion).
func ComposeRoleInstructions(snap role.RoleSnapshot) (Components, error) {
	out := make(Components, 0, len(snap.Instructions))
	for _, ins := range snap.Instructions {
		if ins.Version <= 0 || ins.Content == "" {
			return nil, fmt.Errorf("role %s: instruction %s: %w", snap.RoleKey, ins.Key, ErrMissingInstructionVersion)
		}
		c, err := NewPromptComponent(
			ComponentRoleInstruction,
			SourceRoleInstruction,
			0,
			ins.Content,
			snap.RoleID,
			ins.Version,
			ins.ContentHash,
		)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}
