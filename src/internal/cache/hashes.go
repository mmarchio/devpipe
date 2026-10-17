package cache

import (
	"sort"
	"strconv"

	"github.com/google/uuid"

	"overseer/internal/project"
	"overseer/internal/prompt"
	"overseer/internal/role"
)

// ProjectConfigHash hashes only the prompt-relevant project configuration
// (WBS 10.5): the worker provider/model and context configuration. Display
// and operational fields (working directory, auto-continue, retries, worker
// timeout, timestamps) are excluded, as are compression settings (they are
// the compression configuration hash) and planning settings (they enter the
// planning configuration hash, which applies only to planning prompts —
// T10.24.3). Settings carry no secret values.
func ProjectConfigHash(s project.Settings) string {
	buf := frame(nil, "worker_provider", []byte(s.WorkerProvider))
	buf = frame(buf, "worker_model", []byte(s.WorkerModel))
	buf = frame(buf, "context_window", intBytes(s.ContextWindow))
	buf = frame(buf, "context_threshold", []byte(strconv.FormatFloat(s.ContextThreshold, 'g', -1, 64)))
	return sum(buf)
}

// PlanningConfigHash hashes the planning policy and granularity (WBS 10.24).
func PlanningConfigHash(policy, granularity string) string {
	buf := frame(nil, "planning_policy", []byte(policy))
	buf = frame(buf, "planning_granularity", []byte(granularity))
	return sum(buf)
}

// listField frames a list preserving its order (order is semantic).
func listField(buf []byte, tag string, items []string) []byte {
	buf = frame(buf, tag+"_count", intBytes(len(items)))
	for _, it := range items {
		buf = frame(buf, tag, []byte(it))
	}
	return buf
}

// ContinuationStatusHash hashes a captured worker status (WBS 10.12): work
// unit, summary, completed / remaining / blocking (captured order) and next
// action.
func ContinuationStatusHash(st prompt.ContinuationStatusInput) string {
	buf := frame(nil, "work_unit_id", []byte(st.WorkUnitID.String()))
	buf = frame(buf, "summary", []byte(st.Summary))
	buf = listField(buf, "completed", st.Completed)
	buf = listField(buf, "remaining", st.Remaining)
	buf = listField(buf, "blocking", st.Blocking)
	buf = frame(buf, "next_action", []byte(st.NextAction))
	return sum(buf)
}

// WorkUnitSpecHash hashes the bound work unit's captured specification
// (WBS 10.21): identity, name, kind, objective, acceptance criteria /
// required tests / constraints in captured order, dependencies sorted (the
// composer renders them sorted).
func WorkUnitSpecHash(u prompt.WorkUnitContext) string {
	deps := append([]string(nil), u.Dependencies...)
	sort.Strings(deps)
	buf := frame(nil, "id", []byte(u.ID.String()))
	buf = frame(buf, "name", []byte(u.Name))
	buf = frame(buf, "kind", []byte(u.Kind))
	buf = frame(buf, "objective", []byte(u.Objective))
	buf = listField(buf, "dependencies", deps)
	buf = listField(buf, "acceptance", u.Acceptance)
	buf = listField(buf, "tests", u.RequiredTests)
	buf = listField(buf, "constraints", u.Constraints)
	return sum(buf)
}

// VersionedHash hashes a pinned, versioned source: its kind, identity,
// version and exact content (WBS 10.8/10.9). A new version misses even when
// its text coincidentally matches an older one.
func VersionedHash(kind string, id uuid.UUID, version int, content string) string {
	buf := frame(nil, "kind", []byte(kind))
	buf = frame(buf, "id", []byte(id.String()))
	buf = frame(buf, "version", intBytes(version))
	buf = frame(buf, "content", []byte(content))
	return sum(buf)
}

// RoleHash is the semantic role-configuration hash (WBS 10.6): the Phase 5
// configuration hash over role key + version, system prompt version/hash,
// ordered instruction versions/hashes, allowed states and state-file
// requirements. Display name and description are not inputs (T10.6.5).
func RoleHash(r role.RoleSnapshot) string { return r.ConfigurationHash() }
