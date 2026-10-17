package readmodel

// UnitStage is the display stage of a work unit (WBS 17.168). Worker
// reports and overseer decisions are kept apart (17.85): only the
// persisted unit status DONE yields StageDone.
type UnitStage string

const (
	StageReady      UnitStage = "READY"
	StagePending    UnitStage = "PENDING"
	StageExecuting  UnitStage = "EXECUTING"
	StageIncomplete UnitStage = "INCOMPLETE"
	StageContinuing UnitStage = "CONTINUING"
	// StageComplete: the worker reported COMPLETE; the overseer has not yet
	// validated.
	StageComplete   UnitStage = "COMPLETE"
	StageValidating UnitStage = "VALIDATING"
	StageCheckpoint UnitStage = "CHECKPOINT"
	StageDone       UnitStage = "DONE"
	StageFailed     UnitStage = "FAILED"
	StageBlocked    UnitStage = "BLOCKED"
)

// UnitFacts are the persisted facts a stage derives from.
type UnitFacts struct {
	// Status is the persisted work-unit status (wbs.WorkUnitStatus).
	Status string
	// WorkerResult is the latest structured worker result status
	// (advisory; "" when none).
	WorkerResult string
	// ValidationOutcome is the latest validation run status for the unit
	// ("" when none; RUNNING while in flight).
	ValidationOutcome string
	// CheckpointPending: validation passed and the unit checkpoint is not
	// yet recorded.
	CheckpointPending bool
	// ContinuationInFlight: a Phase 14 continuation is active for the unit.
	ContinuationInFlight bool
}

// StageOf derives the display stage.
func StageOf(f UnitFacts) UnitStage {
	switch f.Status {
	case "DONE":
		return StageDone
	case "FAILED":
		return StageFailed
	case "BLOCKED":
		return StageBlocked
	case "PENDING":
		return StagePending
	case "READY":
		return StageReady
	case "INCOMPLETE":
		if f.ContinuationInFlight {
			return StageContinuing
		}
		return StageIncomplete
	case "EXECUTING":
		if f.ContinuationInFlight {
			return StageContinuing
		}
		return StageExecuting
	case "VALIDATING":
		switch {
		case f.CheckpointPending:
			return StageCheckpoint
		case f.ValidationOutcome == "" && f.WorkerResult == "COMPLETE":
			return StageComplete
		}
		return StageValidating
	}
	return UnitStage(f.Status)
}

// Report pairs the worker's advisory report with the overseer's
// authoritative state (WBS 17.85): "Worker: COMPLETE / Overseer:
// VALIDATING".
type Report struct {
	Worker   string `json:"worker"`
	Overseer string `json:"overseer"`
}

// ReportOf builds the pair; Overseer is always the persisted unit status.
func ReportOf(f UnitFacts) Report {
	w := f.WorkerResult
	if w == "" {
		w = "NONE"
	}
	return Report{Worker: w, Overseer: f.Status}
}
