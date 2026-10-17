// Package hardening is the pure domain of the Phase 16 final hardening pass
// (WBS 16.87–16.149): pass lifecycle, settings and exclusions, the
// language-neutral function / test inventory model, duplicate grouping,
// consolidation safety checks (signature, receiver, side effects, import
// cycles, architecture, value), classification, the ordered consolidation
// plan with its gates, round control, crash-resume reasoning and the report.
//
// It never touches the filesystem, git, the Go toolchain or the database:
// inventories are produced by internal/goanalysis and edits are applied by
// internal/gorefactor. The overseer, not the model, decides eligibility.
package hardening
