package recovery

import (
	"fmt"
	"strings"
)

// Crash matrix (WBS 18.222): for every critical operation, a crash at each
// of nine locations has one documented, deterministic recovery.

// CrashOperation is a critical operation.
type CrashOperation string

// Critical operations.
const (
	OpDispatch     CrashOperation = "worker dispatch"
	OpValidation   CrashOperation = "unit validation"
	OpCheckpoint   CrashOperation = "unit checkpoint"
	OpTransition   CrashOperation = "workflow transition"
	OpPhaseAdvance CrashOperation = "phase advancement"
	OpCompletion   CrashOperation = "project completion"
)

// CrashOperations lists the operations in matrix order.
func CrashOperations() []CrashOperation {
	return []CrashOperation{OpDispatch, OpValidation, OpCheckpoint, OpTransition, OpPhaseAdvance, OpCompletion}
}

// CrashLocation is where the process dies.
type CrashLocation string

// The nine locations of WBS 18.222.
const (
	BeforeIntent            CrashLocation = "before intent"
	AfterIntent             CrashLocation = "after intent"
	BeforeExternalAction    CrashLocation = "before external action"
	DuringExternalAction    CrashLocation = "during external action"
	AfterExternalAction     CrashLocation = "after external action"
	BeforeResultPersistence CrashLocation = "before result persistence"
	AfterResultPersistence  CrashLocation = "after result persistence"
	BeforeTransitionCrash   CrashLocation = "before transition"
	AfterTransitionCrash    CrashLocation = "after transition"
)

// CrashLocations lists the locations in operation order.
func CrashLocations() []CrashLocation {
	return []CrashLocation{BeforeIntent, AfterIntent, BeforeExternalAction, DuringExternalAction, AfterExternalAction,
		BeforeResultPersistence, AfterResultPersistence, BeforeTransitionCrash, AfterTransitionCrash}
}

// CrashOutcome is the recovery class after restart.
type CrashOutcome string

const (
	// ConvergesOnce: recovery continues automatically; the execution
	// completes with every side effect performed exactly once (a
	// validation run may be ABANDONED and rerun — it has no external
	// effect).
	ConvergesOnce CrashOutcome = "CONVERGES_ONCE"
	// StopsDeliveryUnknown: the worker request may have been processed;
	// the execution stops in recovery (DELIVERY_UNKNOWN /
	// RESPONSE_UNPERSISTED) and the request is never resent automatically.
	StopsDeliveryUnknown CrashOutcome = "STOPS_DELIVERY_UNKNOWN"
)

// CrashCase is one cell of the matrix.
type CrashCase struct {
	Operation CrashOperation
	Location  CrashLocation
	Outcome   CrashOutcome
	Recovery  string
}

// CrashMatrix returns every cell with its documented recovery.
func CrashMatrix() []CrashCase {
	conv := func(op CrashOperation, loc CrashLocation, why string) CrashCase {
		return CrashCase{Operation: op, Location: loc, Outcome: ConvergesOnce, Recovery: why}
	}
	unknown := func(op CrashOperation, loc CrashLocation, why string) CrashCase {
		return CrashCase{Operation: op, Location: loc, Outcome: StopsDeliveryUnknown, Recovery: why}
	}
	return []CrashCase{
		conv(OpDispatch, BeforeIntent, "the previous step is applied; the dispatch step starts normally"),
		conv(OpDispatch, AfterIntent, "the INTENT step is resumed; the prompt is prepared and sent once"),
		conv(OpDispatch, BeforeExternalAction, "the PENDING dispatch was never sent: reconciled as not sent, then sent once"),
		unknown(OpDispatch, DuringExternalAction, "transmitted, outcome unknown: DELIVERY_UNKNOWN, operator decides; never resent"),
		unknown(OpDispatch, AfterExternalAction, "response received but not stored: reconciled through the provider, else DELIVERY_UNKNOWN; never resent"),
		unknown(OpDispatch, BeforeResultPersistence, "same as after external action: the response exists only in the dead process"),
		conv(OpDispatch, AfterResultPersistence, "the stored response is evaluated; nothing is resent"),
		conv(OpDispatch, BeforeTransitionCrash, "the step re-runs, finds its completed dispatch and applies the transition"),
		conv(OpDispatch, AfterTransitionCrash, "the transition is durable; the next step starts"),
		conv(OpValidation, BeforeIntent, "the previous step is applied; validation starts normally"),
		conv(OpValidation, AfterIntent, "the INTENT step is resumed and starts its run"),
		conv(OpValidation, BeforeExternalAction, "the RUNNING run is ABANDONED and rerun"),
		conv(OpValidation, DuringExternalAction, "the RUNNING run is ABANDONED (never PASS / FAIL) and rerun"),
		conv(OpValidation, AfterExternalAction, "results were not recorded: the run is ABANDONED and rerun"),
		conv(OpValidation, BeforeResultPersistence, "same as after external action"),
		conv(OpValidation, AfterResultPersistence, "the recorded result is reused; no rerun"),
		conv(OpValidation, BeforeTransitionCrash, "the recorded result is reused and the transition applied"),
		conv(OpValidation, AfterTransitionCrash, "the transition is durable; the next step starts"),
		conv(OpCheckpoint, BeforeIntent, "the previous step is applied; the checkpoint step starts normally"),
		conv(OpCheckpoint, AfterIntent, "the INTENT step is resumed"),
		conv(OpCheckpoint, BeforeExternalAction, "no commit exists (trailer lookup): committed once"),
		conv(OpCheckpoint, DuringExternalAction, "staged but not committed: no trailer found, committed once"),
		conv(OpCheckpoint, AfterExternalAction, "the commit exists: adopted by trailer, parent and tree; no second commit"),
		conv(OpCheckpoint, BeforeResultPersistence, "same as after external action"),
		conv(OpCheckpoint, AfterResultPersistence, "the recorded checkpoint is reused"),
		conv(OpCheckpoint, BeforeTransitionCrash, "the recorded checkpoint is reused and the unit marked DONE"),
		conv(OpCheckpoint, AfterTransitionCrash, "the unit is DONE; the next unit is selected"),
		conv(OpTransition, BeforeIntent, "the previous step is applied"),
		conv(OpTransition, AfterIntent, "the INTENT step is resumed"),
		conv(OpTransition, BeforeExternalAction, "no external action: as after intent"),
		conv(OpTransition, DuringExternalAction, "no external action: as after intent"),
		conv(OpTransition, AfterExternalAction, "no external action: as after intent"),
		conv(OpTransition, BeforeResultPersistence, "no external action: as after intent"),
		conv(OpTransition, AfterResultPersistence, "no external action: as before transition"),
		conv(OpTransition, BeforeTransitionCrash, "the transaction never committed: the step re-runs"),
		conv(OpTransition, AfterTransitionCrash, "the transition is durable"),
		conv(OpPhaseAdvance, BeforeIntent, "the previous step is applied"),
		conv(OpPhaseAdvance, AfterIntent, "the INTENT step is resumed"),
		conv(OpPhaseAdvance, BeforeExternalAction, "the phase validation run is ABANDONED and rerun"),
		conv(OpPhaseAdvance, DuringExternalAction, "the phase validation run is ABANDONED and rerun"),
		conv(OpPhaseAdvance, AfterExternalAction, "results not recorded: rerun"),
		conv(OpPhaseAdvance, BeforeResultPersistence, "same as after external action"),
		conv(OpPhaseAdvance, AfterResultPersistence, "the advancement transaction rolled back: re-applied once; one current phase"),
		conv(OpPhaseAdvance, BeforeTransitionCrash, "as after result persistence"),
		conv(OpPhaseAdvance, AfterTransitionCrash, "the phase is DONE; the next phase starts"),
		conv(OpCompletion, BeforeIntent, "the previous step is applied"),
		conv(OpCompletion, AfterIntent, "the INTENT step is resumed"),
		conv(OpCompletion, BeforeExternalAction, "no final commit exists: committed once"),
		conv(OpCompletion, DuringExternalAction, "staged but not committed: committed once"),
		conv(OpCompletion, AfterExternalAction, "the final commit exists: adopted by trailer"),
		conv(OpCompletion, BeforeResultPersistence, "same as after external action"),
		conv(OpCompletion, AfterResultPersistence, "the completion transaction rolled back: re-applied once"),
		conv(OpCompletion, BeforeTransitionCrash, "as after result persistence"),
		conv(OpCompletion, AfterTransitionCrash, "the project is COMPLETED (terminal)"),
	}
}

// CrashMatrixMarkdown renders the matrix (docs, WBS 18.222).
func CrashMatrixMarkdown() string {
	var b strings.Builder
	b.WriteString("| Operation | Crash location | Outcome | Recovery |\n| --- | --- | --- | --- |\n")
	for _, c := range CrashMatrix() {
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", c.Operation, c.Location, c.Outcome, c.Recovery)
	}
	return b.String()
}
