package statefile

// DefaultFileType is one of the GLOBAL default state-file type definitions
// seeded by the service layer (WBS 7.2). The definitions are data, not
// branching: the seed op creates one global row per definition, and the
// DefaultFilename is a preferred materialization name, never an
// authoritative path (WBS 7.1).
type DefaultFileType struct {
	Key             string
	Name            string
	DefaultFilename string
	Scope           StateFileScope
}

// DefaultFileTypes returns the six initial default state-file types (WBS
// 7.2, T7.2.1-T7.2.2). The order is stable (declaration order) so seeding
// and tests are deterministic. The scope of each default follows the WBS 7.3
// examples (PROJECT_STATE/ARCHITECTURE/TASK_ARCHIVE -> PROJECT, PHASE_STATE
// -> PHASE, ACTIVE_TASK -> WORK_UNIT; TEST_STATE is legal at PHASE or
// WORK_UNIT and the global default pins PHASE).
func DefaultFileTypes() []DefaultFileType {
	return []DefaultFileType{
		{Key: "PROJECT_STATE", Name: "Project State", DefaultFilename: "PROJECT_STATE.md", Scope: ScopeProject},
		{Key: "PHASE_STATE", Name: "Phase State", DefaultFilename: "PHASE_STATE.md", Scope: ScopePhase},
		{Key: "ACTIVE_TASK", Name: "Active Task", DefaultFilename: "ACTIVE_TASK.md", Scope: ScopeWorkUnit},
		{Key: "TASK_ARCHIVE", Name: "Task Archive", DefaultFilename: "TASK_ARCHIVE.md", Scope: ScopeProject},
		{Key: "ARCHITECTURE", Name: "Architecture", DefaultFilename: "ARCHITECTURE.md", Scope: ScopeProject},
		{Key: "TEST_STATE", Name: "Test State", DefaultFilename: "TEST_STATE.md", Scope: ScopePhase},
	}
}
