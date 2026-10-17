package compression

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/prompt"
)

// Shared fixture: a small implementation prompt shaped like the Phase 8
// composition (system, role instruction, state files, task context with
// acceptance criteria/tests/constraints, response protocol).

var (
	fxUnitID  = uuid.MustParse("11111111-1111-1111-1111-111111111111")
	fxProtoID = uuid.MustParse("22222222-2222-2222-2222-222222222222")
)

const (
	fxAC1 = "Exact preservation validator rejects a missing acceptance criterion"
	fxAC2 = "Validator is deterministic"
	fxAC3 = "Modified command fails validation"
	fxT1  = "go test ./internal/compression/..."
	fxT2  = "go test ./..."
	fxC1  = "Do not modify public API."
	fxC2  = "Do not add external dependencies."
)

const fxProtocol = `Response protocol v2 (required response schema; allowed status values as defined by the schema's status field):
{
  "status": "complete|incomplete|blocked|failed",
  "summary": "...",
  "completed": [],
  "remaining": [],
  "blocking": [],
  "next_action": "..."
}
The worker reports status; the overseer decides transitions.`

func fxTask() string {
	return "ID: " + fxUnitID.String() + "\nname: 9.17\nkind: IMPLEMENTATION\nobjective: Implement the exact preservation validator\n" +
		"dependencies: 9.10; 9.16\nacceptance criteria: " + strings.Join([]string{fxAC1, fxAC2, fxAC3}, "; ") +
		"\nrequired tests: " + fxT1 + "; " + fxT2 + "\nconstraints: " + fxC1 + "; " + fxC2 + "\n"
}

func fxPromptComponents(t *testing.T) prompt.Components {
	t.Helper()
	mk := func(typ prompt.PromptComponentType, src prompt.PromptSource, order int, content string) prompt.PromptComponent {
		c, err := prompt.NewPromptComponent(typ, src, order, content, uuid.New(), order+1, "hash-"+itoa(order))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	sys := mk(prompt.ComponentSystem, prompt.SourceRoleSystemPrompt, 0, "You are the ENGINEER. Implement only the active task.\n\nNever modify unrelated files.")
	role := mk(prompt.ComponentRoleInstruction, prompt.SourceRoleInstruction, 1, "Follow test-driven development.\nDo not skip failing tests.")
	proj := mk(prompt.ComponentProjectContext, prompt.SourceStateFile, 2,
		"PROJECT_STATE: the overseer project history.\nPhase 1 built the skeleton.\nPhase 2 added PostgreSQL persistence.\nPhase 3 added project management.\nPhase 4 added WBS management.\nPhase 5 added roles.\nPhase 6 added the state machine.\nPhase 7 added state files.\nPhase 8 added canonical prompts.")
	arch := mk(prompt.ComponentProjectContext, prompt.SourceStateFile, 3,
		"ARCHITECTURE: layered.\nThe web layer calls services.\nServices call the domain.\nThe domain never imports infrastructure.\nRepositories translate database errors.")
	task := mk(prompt.ComponentTaskContext, prompt.SourceWorkUnit, 4, fxTask())
	task.SourceID = fxUnitID
	proto := mk(prompt.ComponentResponseProtocol, prompt.SourceResponseProtocol, 5, fxProtocol)
	proto.SourceID = fxProtoID
	return prompt.Components{sys, role, proj, arch, task, proto}
}

func fxComponents(t *testing.T) []CompressionComponent {
	t.Helper()
	cs, err := FromPromptComponents(fxPromptComponents(t))
	if err != nil {
		t.Fatal(err)
	}
	return ApplyEffectivePolicies(cs)
}

func fxManifest(t *testing.T, comps []CompressionComponent) Manifest {
	t.Helper()
	return BuildManifest(ManifestInput{
		WorkUnitID: fxUnitID.String(), WorkUnitName: "9.17",
		Dependencies: []string{"9.10", "9.16"},
		Acceptance:   []string{fxAC1, fxAC2, fxAC3},
		Tests:        []string{fxT1, fxT2},
		Constraints:  []string{fxC1, fxC2},
		StateKey:     "IMPLEMENT", RoleKey: "ENGINEER",
	}, comps)
}

// fxGoodOutput is a valid compression of the fixture: exact components
// verbatim, semantic components lightly shortened, narrative compressed.
func fxGoodOutput() string {
	return strings.Join([]string{
		"You are the ENGINEER. Implement only the active task.\nNever modify unrelated files.",
		"Follow test-driven development.\nDo not skip failing tests.",
		"PROJECT_STATE: the overseer project history.",
		"ARCHITECTURE: layered.",
		fxTask(),
		fxProtocol,
	}, "\n")
}

func fxInput(t *testing.T, compressed string) ValidationInput {
	t.Helper()
	comps := fxComponents(t)
	si := RenderStructuredInput(comps)
	canonTokens := ComponentTokens(comps) * 2
	return ValidationInput{
		Components: comps, Canonical: comps, Manifest: fxManifest(t, comps), Boundary: si.Boundary,
		Compressed: compressed, CanonicalTokens: canonTokens, TargetTokens: canonTokens, MinimumSavings: 0.15,
	}
}
