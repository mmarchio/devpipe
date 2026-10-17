package hardening

import (
	"fmt"
	"sort"
)

// OpKind is one consolidation operation (WBS 16.110).
type OpKind string

// Operation kinds, in plan order.
const (
	OpCopyToShared     OpKind = "COPY_TO_SHARED"
	OpTestShared       OpKind = "TEST_SHARED"
	OpUpdateReferences OpKind = "UPDATE_REFERENCES"
	OpTestCallers      OpKind = "TEST_CALLERS"
	OpConsolidateTests OpKind = "CONSOLIDATE_TESTS"
	OpRemoveDuplicates OpKind = "REMOVE_DUPLICATES"
	OpValidate         OpKind = "VALIDATE"
	OpGroupCheckpoint  OpKind = "CHECKPOINT"
)

// OpStatus is an operation's durable state. RUNNING is persisted before
// the edit starts, so a crash leaves RUNNING — never DONE (WBS 16.141).
type OpStatus string

// Operation statuses.
const (
	OpPending OpStatus = "PENDING"
	OpRunning OpStatus = "RUNNING"
	OpDone    OpStatus = "DONE"
	OpFailed  OpStatus = "FAILED"
)

// Operation is one planned step of a group's consolidation.
type Operation struct {
	Seq     int    `json:"seq"`
	Kind    OpKind `json:"kind"`
	Package string `json:"package,omitempty"`
}

// BuildConsolidationPlan orders a SAFE group's operations (WBS 16.110,
// 16.120, 16.169): copy the shared implementation, test it, then per
// caller package (sorted) migrate references and test that package,
// consolidate tests (when enabled), remove the duplicates, validate, and
// checkpoint. Every member package and referencing package appears.
func BuildConsolidationPlan(g DuplicateGroup, consolidateTests bool) []Operation {
	var ops []Operation
	add := func(k OpKind, pkg string) { ops = append(ops, Operation{Seq: len(ops) + 1, Kind: k, Package: pkg}) }
	add(OpCopyToShared, "")
	add(OpTestShared, "")
	for _, p := range CallerPackages(g) {
		add(OpUpdateReferences, p)
		add(OpTestCallers, p)
	}
	if consolidateTests {
		add(OpConsolidateTests, "")
	}
	add(OpRemoveDuplicates, "")
	add(OpValidate, "")
	add(OpGroupCheckpoint, "")
	return ops
}

// GroupState is the persisted + observed progress of one group.
type GroupState struct {
	SharedExists     bool
	SharedTestsPass  bool
	Migrated         map[string]bool
	CallerTestsPass  map[string]bool
	OldReferences    int
	ValidationPass   bool
	CheckpointPolicy string
	CheckpointDone   bool
}

// CanRemoveDuplicates is the removal gate (WBS 16.124/16.127): the shared
// function exists and passes its tests, every caller package migrated and
// tested, and zero references to the old symbols remain.
func CanRemoveDuplicates(st GroupState, callers []string) (bool, string) {
	switch {
	case !st.SharedExists:
		return false, "shared function does not exist"
	case !st.SharedTestsPass:
		return false, "shared tests have not passed"
	}
	pk := append([]string(nil), callers...)
	sort.Strings(pk)
	for _, p := range pk {
		if !st.Migrated[p] {
			return false, fmt.Sprintf("package %s not migrated", p)
		}
		if !st.CallerTestsPass[p] {
			return false, fmt.Sprintf("package %s tests have not passed", p)
		}
	}
	if st.OldReferences != 0 {
		return false, fmt.Sprintf("%d references to the old implementations remain", st.OldReferences)
	}
	return true, ""
}

// CanCompleteGroup is the completion gate (WBS 16.134): validation passed
// and the group checkpoint exists unless the policy makes it optional.
// Reference updates alone are never enough.
func CanCompleteGroup(st GroupState) (bool, string) {
	if !st.ValidationPass {
		return false, "consolidation validation has not passed"
	}
	if st.CheckpointPolicy == "REQUIRED" && !st.CheckpointDone {
		return false, "required group checkpoint missing"
	}
	return true, ""
}

// RoundVerdict is the decision after a round's analysis.
type RoundVerdict string

// Round verdicts.
const (
	RoundConsolidate RoundVerdict = "CONSOLIDATE"
	RoundFixedPoint  RoundVerdict = "FIXED_POINT"
	RoundMaxRounds   RoundVerdict = "MAX_ROUNDS"
)

// AfterAnalysis decides what follows the analysis of `round` (WBS
// 16.135–16.138): no unprocessed SAFE group → FIXED_POINT; SAFE groups
// within the round budget → CONSOLIDATE; SAFE groups after max rounds
// were consolidated → MAX_ROUNDS (they are reported as remaining, never
// claimed absent).
func AfterAnalysis(round, maxRounds, safe int) RoundVerdict {
	switch {
	case safe == 0:
		return RoundFixedPoint
	case round > maxRounds:
		return RoundMaxRounds
	}
	return RoundConsolidate
}

// Facts are source observations used to resume after a crash.
type Facts struct {
	SharedExists bool
	// RemainingRefs maps a package to its references to old symbols.
	RemainingRefs     map[string]int
	SharedTestsExist  bool
	HelperTestsRemain bool
	OldSymbolsPresent bool
}

// ResumeDone reports whether an interrupted (RUNNING) operation's effect is
// already present in the source (WBS 16.139–16.142). Test and validation
// operations are never assumed done: they always rerun. A false result
// means the (idempotent) operation must run again.
func ResumeDone(op Operation, f Facts) bool {
	switch op.Kind {
	case OpCopyToShared:
		return f.SharedExists
	case OpUpdateReferences:
		return f.RemainingRefs[op.Package] == 0
	case OpConsolidateTests:
		return f.SharedTestsExist && !f.HelperTestsRemain
	case OpRemoveDuplicates:
		return !f.OldSymbolsPresent
	}
	return false
}
