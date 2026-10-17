package execution

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/google/uuid"
)

// CheckpointStage is the logical checkpoint name (WBS 15.149).
const CheckpointStage = "BOOTSTRAP_COMPLETE"

// FileRef pins one state-file version.
type FileRef struct {
	Type        string    `json:"type"`
	VersionID   uuid.UUID `json:"version_id"`
	ContentHash string    `json:"content_hash"`
}

// BootstrapCheckpoint references and hashes every bootstrap artifact
// (WBS 15.149). It is built from immutable artifacts so it stays
// verifiable after later phases move the runtime (PLAN Decision 19).
type BootstrapCheckpoint struct {
	ExecutionID           uuid.UUID `json:"execution_id"`
	ConfigurationHash     string    `json:"configuration_hash"`
	RuntimeStateID        uuid.UUID `json:"runtime_state_id"`
	BootstrapHistoryHash  string    `json:"bootstrap_history_hash"`
	BootstrapHistoryCount int       `json:"bootstrap_history_count"`
	StateFiles            []FileRef `json:"state_files"`
	ContextSnapshotID     uuid.UUID `json:"context_snapshot_id"`
	ContextSnapshotHash   string    `json:"context_snapshot_hash"`
	BaselineID            uuid.UUID `json:"baseline_id"`
	BaselineHash          string    `json:"baseline_hash"`
	WorkerPromptID        uuid.UUID `json:"worker_prompt_id"`
	WorkerPromptHash      string    `json:"worker_prompt_hash"`
	CacheKey              string    `json:"cache_key"`
	CanonicalKey          string    `json:"canonical_key"`
	FirstActionableState  string    `json:"first_actionable_state"`
	PlanHash              string    `json:"plan_hash"`
}

// Normalize orders file refs.
func (c *BootstrapCheckpoint) Normalize() {
	sort.SliceStable(c.StateFiles, func(i, j int) bool {
		if c.StateFiles[i].Type != c.StateFiles[j].Type {
			return c.StateFiles[i].Type < c.StateFiles[j].Type
		}
		return c.StateFiles[i].VersionID.String() < c.StateFiles[j].VersionID.String()
	})
	if c.StateFiles == nil {
		c.StateFiles = []FileRef{}
	}
}

// Hash is SHA-256 over the canonical checkpoint.
func (c BootstrapCheckpoint) Hash() string {
	c.Normalize()
	b, _ := json.Marshal(c)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// VerifyCheckpoint compares the stored checkpoint with one recomputed from
// current DB artifacts (WBS 15.150) and lists every mismatch.
func VerifyCheckpoint(stored, observed BootstrapCheckpoint) []string {
	stored.Normalize()
	observed.Normalize()
	var out []string
	cmp := func(name string, a, b any) {
		if fmt.Sprint(a) != fmt.Sprint(b) {
			out = append(out, fmt.Sprintf("%s: expected %v, found %v", name, a, b))
		}
	}
	cmp("execution", stored.ExecutionID, observed.ExecutionID)
	cmp("configuration_hash", stored.ConfigurationHash, observed.ConfigurationHash)
	cmp("runtime_state", stored.RuntimeStateID, observed.RuntimeStateID)
	cmp("bootstrap_history_hash", stored.BootstrapHistoryHash, observed.BootstrapHistoryHash)
	cmp("bootstrap_history_count", stored.BootstrapHistoryCount, observed.BootstrapHistoryCount)
	cmp("context_snapshot", stored.ContextSnapshotID, observed.ContextSnapshotID)
	cmp("context_snapshot_hash", stored.ContextSnapshotHash, observed.ContextSnapshotHash)
	cmp("baseline", stored.BaselineID, observed.BaselineID)
	cmp("baseline_hash", stored.BaselineHash, observed.BaselineHash)
	cmp("worker_prompt", stored.WorkerPromptID, observed.WorkerPromptID)
	cmp("worker_prompt_hash", stored.WorkerPromptHash, observed.WorkerPromptHash)
	cmp("cache_key", stored.CacheKey, observed.CacheKey)
	cmp("canonical_key", stored.CanonicalKey, observed.CanonicalKey)
	cmp("first_actionable_state", stored.FirstActionableState, observed.FirstActionableState)
	cmp("plan_hash", stored.PlanHash, observed.PlanHash)
	obs := map[uuid.UUID]FileRef{}
	for _, f := range observed.StateFiles {
		obs[f.VersionID] = f
	}
	for _, f := range stored.StateFiles {
		o, ok := obs[f.VersionID]
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("state_file %s version %s: missing", f.Type, f.VersionID))
		case o.ContentHash != f.ContentHash:
			out = append(out, fmt.Sprintf("state_file %s version %s: content changed", f.Type, f.VersionID))
		}
	}
	return out
}

// ActivationInput carries the facts ValidateActivation checks
// (WBS 15.153).
type ActivationInput struct {
	ExecutionStatus      ExecutionStatus
	CheckpointVerified   bool
	CheckpointProblems   []string
	ProjectStatus        string
	RuntimeExists        bool
	RuntimeBound         bool
	FirstActionableKnown bool
	ContextSnapshotKnown bool
}

// ErrAlreadyRunning signals an idempotent re-activation (WBS 15.154).
var ErrAlreadyRunning = fmt.Errorf("execution: already running")

// ValidateActivation returns every unmet activation precondition
// (WBS 15.151–15.153). An execution already RUNNING returns
// ErrAlreadyRunning and no problems.
func ValidateActivation(in ActivationInput) ([]string, error) {
	if in.ExecutionStatus == ExecutionRunning {
		return nil, ErrAlreadyRunning
	}
	var p []string
	if in.ExecutionStatus != ExecutionReady {
		p = append(p, fmt.Sprintf("execution is %s, not READY", in.ExecutionStatus))
	}
	if !in.CheckpointVerified {
		p = append(p, "bootstrap checkpoint is missing or not verified")
		p = append(p, in.CheckpointProblems...)
	}
	if in.ProjectStatus != "READY" {
		p = append(p, fmt.Sprintf("project is %s, not READY", in.ProjectStatus))
	}
	if !in.RuntimeExists {
		p = append(p, "runtime state does not exist")
	} else if !in.RuntimeBound {
		p = append(p, "runtime state is not bound to the execution")
	}
	if !in.FirstActionableKnown {
		p = append(p, "first actionable state is not resolved")
	}
	if !in.ContextSnapshotKnown {
		p = append(p, "initial context is not prepared")
	}
	return p, nil
}
