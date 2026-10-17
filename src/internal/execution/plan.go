package execution

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"sort"

	"github.com/google/uuid"
)

// SourceVersions is the version vector of every mutable source a bootstrap
// reads (WBS 15.90–15.94/15.159). Two vectors are equal exactly when no
// relevant source changed.
type SourceVersions struct {
	ProjectVersion    int               `json:"project_version"`
	SettingsHash      string            `json:"settings_hash"`
	PhaseListHash     string            `json:"phase_list_hash"`
	WBSVersion        int               `json:"wbs_version"`
	WBSHash           string            `json:"wbs_hash"`
	PlanID            string            `json:"plan_id"`
	PlanVersion       int               `json:"plan_version"`
	PlanStatus        string            `json:"plan_status"`
	UnitStatusHash    string            `json:"unit_status_hash"`
	MachineID         string            `json:"machine_id"`
	MachineVersion    int               `json:"machine_version"`
	MachineHash       string            `json:"machine_hash"`
	RoleHashes        map[string]string `json:"role_hashes"`
	TemplateVersions  map[string]int    `json:"template_versions"`
	CompressionHash   string            `json:"compression_hash"`
	WorkerHash        string            `json:"worker_hash"`
	ProtocolHash      string            `json:"protocol_hash"`
	ContinuationHash  string            `json:"continuation_hash"`
	ExecutionSettings string            `json:"execution_settings"`
}

// Equal reports identical vectors.
func (v SourceVersions) Equal(o SourceVersions) bool { return len(v.Diff(o)) == 0 }

// Diff names the members that differ, sorted (WBS 15.159).
func (v SourceVersions) Diff(o SourceVersions) []string {
	var out []string
	a, b := reflect.ValueOf(v), reflect.ValueOf(o)
	t := a.Type()
	for i := 0; i < t.NumField(); i++ {
		fa, fb := a.Field(i).Interface(), b.Field(i).Interface()
		if isEmptyMap(fa) && isEmptyMap(fb) {
			continue
		}
		if !reflect.DeepEqual(fa, fb) {
			out = append(out, t.Field(i).Tag.Get("json"))
		}
	}
	sort.Strings(out)
	return out
}

func isEmptyMap(x any) bool {
	rv := reflect.ValueOf(x)
	return rv.Kind() == reflect.Map && rv.Len() == 0
}

// BootstrapPlan is the deterministic, pre-mutation bootstrap plan
// (WBS 15.157).
type BootstrapPlan struct {
	ProjectID        uuid.UUID  `json:"project_id"`
	PhaseID          uuid.UUID  `json:"phase_id"`
	PhaseOrder       int        `json:"phase_order"`
	PhaseName        string     `json:"phase_name"`
	WBSVersion       int        `json:"wbs_version"`
	WBSHash          string     `json:"wbs_hash"`
	PlanState        PlanState  `json:"plan_state"`
	PlanID           *uuid.UUID `json:"plan_id"`
	PlanVersion      int        `json:"plan_version"`
	MachineID        uuid.UUID  `json:"machine_id"`
	MachineVersion   int        `json:"machine_version"`
	MachineHash      string     `json:"machine_hash"`
	InitialState     string     `json:"initial_state"`
	ControlPath      []string   `json:"control_path"`
	FirstActionable  string     `json:"first_actionable_state"`
	Role             string     `json:"role"`
	WorkUnitID       *uuid.UUID `json:"work_unit_id"`
	TemplateKey      string     `json:"template_key"`
	TemplateID       *uuid.UUID `json:"template_id"`
	TemplateVersion  int        `json:"template_version"`
	WorkingDirectory string     `json:"working_directory"`

	Sources SourceVersions `json:"sources"`
}

// Steps returns the ordered bootstrap steps (WBS 15.157).
func (p BootstrapPlan) Steps() []string {
	return []string{
		"Validate project",
		"Resolve phase",
		"Bind WBS v" + itoa(p.WBSVersion),
		"Bind workflow v" + itoa(p.MachineVersion),
		"Snapshot configuration",
		"Initialize state files",
		"Initialize runtime",
		"Prepare context",
		"Prepare initial prompt",
		"Verify checkpoint",
		"Activate",
	}
}

// Hash is SHA-256 over the canonical plan (WBS 15.158).
func (p BootstrapPlan) Hash() string {
	b, _ := json.Marshal(p)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
