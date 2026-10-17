package statefile

import "overseer/internal/role"

// ClassifySnapshotFile assigns the WBS 7.36 source classification of a
// snapshot state-file pin. The classification is a property of the CAPTURED
// component (persisted with the snapshot), not of the live state — it must
// survive reconstruction without re-derivation, so it is a pure function of
// the type key + scope captured at snapshot time.
//
// The canonical mapping (T7.36.1-T7.36.4): PROJECT_STATE ->
// PROJECT_CONTEXT, PHASE_STATE -> PHASE_CONTEXT, ACTIVE_TASK ->
// TASK_CONTEXT, TEST_STATE -> TEST_CONTEXT.
//
// Task-level choice (stated so TESTER/PM can verify; the WBS pins the
// four canonical classes and the class SET, not the non-canonical keys):
// a non-canonical type key falls back to the scope-based class — PROJECT
// scope -> PROJECT_CONTEXT, PHASE scope -> PHASE_CONTEXT, WORK_UNIT and
// EXECUTION scope -> TASK_CONTEXT.
func ClassifySnapshotFile(typeKey string, scope StateFileScope) role.Classification {
	switch typeKey {
	case "PROJECT_STATE":
		return role.ClassificationProjectContext
	case "PHASE_STATE":
		return role.ClassificationPhaseContext
	case "ACTIVE_TASK":
		return role.ClassificationTaskContext
	case "TEST_STATE":
		return role.ClassificationTestContext
	}
	switch scope {
	case ScopeProject:
		return role.ClassificationProjectContext
	case ScopePhase:
		return role.ClassificationPhaseContext
	default:
		return role.ClassificationTaskContext
	}
}
