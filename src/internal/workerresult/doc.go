// Package workerresult is the Phase 13 worker response protocol: it turns
// a stored raw worker response into a validated, normalized WorkerResult.
//
// A WorkerResult is a claim made by the worker — what it reports happened.
// It is never verified repository state, a test result or an acceptance
// decision, and it carries no workflow authority: the overseer decides
// state transitions. Authoritative metadata (project, phase, work unit,
// session, state, role) always comes from the Phase 12 dispatch record.
package workerresult
