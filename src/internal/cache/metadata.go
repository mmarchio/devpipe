package cache

import (
	"fmt"
	"sort"
)

// DependencyClass says whether a cache dependency is known ahead of time
// (STATIC) or only after future work (DYNAMIC) — Phase 11 precomputes only
// prompts whose dependencies are all static (WBS 10.44).
type DependencyClass string

const (
	Static  DependencyClass = "STATIC"
	Dynamic DependencyClass = "DYNAMIC"
)

// Named input identifiers (stable; stored in key-source metadata).
const (
	InputContextSnapshot  = "context_snapshot"
	InputRole             = "role"
	InputTemplate         = "template"
	InputResponseProtocol = "response_protocol"
	InputCompression      = "compression_config"
	InputProjectConfig    = "project_config"
	InputStateMachine     = "state_machine"
	InputStateInstruction = "state_instruction"
	InputWorkUnitSpec     = "work_unit_spec"
	InputPhaseWBS         = "phase_wbs"
	InputPlanningConfig   = "planning_config"
	InputContinuation     = "continuation_status"
	InputPolicy           = "preparation_policy"
)

var staticDependencies = map[string]bool{
	InputRole: true, InputTemplate: true, InputResponseProtocol: true, InputCompression: true,
	InputProjectConfig: true, InputStateMachine: true, InputStateInstruction: true,
	InputPlanningConfig: true, InputPolicy: true,
	// Static state files (known ahead of the work that consumes them).
	"file:ARCHITECTURE": true, "file:PROJECT_STATE": true,
}

// Classify returns the dependency class of a named input (T10.44.1-4).
// Anything not known to be static — future worker status, test output,
// generated plans, the next ACTIVE_TASK, unknown inputs — is DYNAMIC.
func Classify(name string) DependencyClass {
	if staticDependencies[name] {
		return Static
	}
	return Dynamic
}

// NamedInput is one semantic input of a cache entry.
type NamedInput struct {
	Name   string          `json:"name"`
	Hash   string          `json:"hash"`
	Detail string          `json:"detail,omitempty"`
	Class  DependencyClass `json:"class"`
}

// FileVersion is one state-file pin of the context snapshot.
type FileVersion struct {
	TypeKey string `json:"type_key"`
	Version int    `json:"version"`
	Hash    string `json:"hash"`
}

// KeySourceMetadata is the stored semantic-input record of a cache entry
// (WBS 10.28/10.40/10.44): every named input hash with a human-readable
// detail, the snapshot's state-file versions, and each input's dependency
// class. It holds no prompt content and no secrets.
type KeySourceMetadata struct {
	Schema int           `json:"schema"`
	Inputs []NamedInput  `json:"inputs"`
	Files  []FileVersion `json:"files"`
}

// Details carries the human-readable detail of each input.
type Details struct {
	Role, Template, Protocol, Compression, StateMachine, StateInstruction, WBS, Policy string
	Files                                                                              []FileVersion
}

// BuildMetadata derives the metadata from a validated input. Files are
// sorted by type key; inputs follow a fixed order.
func BuildMetadata(in PromptCacheInput, d Details) KeySourceMetadata {
	cont := ""
	if in.ContinuationStatusHash != nil {
		cont = *in.ContinuationStatusHash
	}
	add := func(out []NamedInput, name, hash, detail string) []NamedInput {
		return append(out, NamedInput{Name: name, Hash: hash, Detail: detail, Class: Classify(name)})
	}
	var ins []NamedInput
	ins = add(ins, InputContextSnapshot, in.ContextSnapshotHash, "")
	ins = add(ins, InputRole, in.RoleHash, d.Role)
	ins = add(ins, InputTemplate, in.TemplateHash, d.Template)
	ins = add(ins, InputResponseProtocol, in.ResponseProtocolHash, d.Protocol)
	ins = add(ins, InputCompression, in.CompressionConfigHash, d.Compression)
	ins = add(ins, InputProjectConfig, in.ProjectConfigHash, "")
	ins = add(ins, InputStateMachine, fmt.Sprintf("%s@%d/%s", in.StateMachineID, in.StateMachineVersion, in.StateKey), d.StateMachine)
	ins = add(ins, InputStateInstruction, in.StateInstructionHash, d.StateInstruction)
	ins = add(ins, InputWorkUnitSpec, in.WorkUnitSpecHash, "")
	ins = add(ins, InputPhaseWBS, in.PhaseWBSHash, d.WBS)
	ins = add(ins, InputPlanningConfig, in.PlanningConfigHash, "")
	ins = add(ins, InputContinuation, cont, "")
	ins = add(ins, InputPolicy, in.PreparationPolicy, d.Policy)
	files := append([]FileVersion(nil), d.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].TypeKey < files[j].TypeKey })
	return KeySourceMetadata{Schema: CurrentSchema, Inputs: ins, Files: files}
}

// Classes returns the dependency class of every input and file.
func (m KeySourceMetadata) Classes() map[string]DependencyClass {
	out := map[string]DependencyClass{}
	for _, in := range m.Inputs {
		out[in.Name] = in.Class
	}
	for _, f := range m.Files {
		out["file:"+f.TypeKey] = Classify("file:" + f.TypeKey)
	}
	return out
}

// Change is one changed input between two metadata records.
type Change struct {
	Name   string `json:"name"`
	Before string `json:"before"`
	After  string `json:"after"`
}

// MetadataDiff is the comparison of a stored entry's inputs with the
// current inputs (WBS 10.40): changed inputs (with version detail where
// known, e.g. "ACTIVE_TASK v4 → v5") and unchanged inputs.
type MetadataDiff struct {
	Changed   []Change `json:"changed"`
	Unchanged []string `json:"unchanged"`
}

// Equal reports whether no semantic input differs.
func (d MetadataDiff) Equal() bool { return len(d.Changed) == 0 }

// Diff compares stored (a) and current (b) metadata input-by-input and
// file-by-file. It reads only the stored semantic metadata — no prompt
// text is compared (WBS 10.61).
func Diff(a, b KeySourceMetadata) MetadataDiff {
	var d MetadataDiff
	bi := map[string]NamedInput{}
	for _, in := range b.Inputs {
		bi[in.Name] = in
	}
	for _, x := range a.Inputs {
		y, ok := bi[x.Name]
		switch {
		case !ok:
			d.Changed = append(d.Changed, Change{Name: x.Name, Before: describe(x), After: "(absent)"})
		case x.Hash != y.Hash:
			d.Changed = append(d.Changed, Change{Name: x.Name, Before: describe(x), After: describe(y)})
		default:
			d.Unchanged = append(d.Unchanged, x.Name)
		}
		delete(bi, x.Name)
	}
	for _, y := range b.Inputs {
		if _, ok := bi[y.Name]; ok {
			d.Changed = append(d.Changed, Change{Name: y.Name, Before: "(absent)", After: describe(y)})
		}
	}
	af := map[string]FileVersion{}
	for _, f := range a.Files {
		af[f.TypeKey] = f
	}
	bf := map[string]FileVersion{}
	for _, f := range b.Files {
		bf[f.TypeKey] = f
	}
	keys := map[string]bool{}
	for k := range af {
		keys[k] = true
	}
	for k := range bf {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	for _, k := range sorted {
		x, okA := af[k]
		y, okB := bf[k]
		name := "file:" + k
		switch {
		case !okA:
			d.Changed = append(d.Changed, Change{Name: name, Before: "(absent)", After: fmt.Sprintf("%s v%d", k, y.Version)})
		case !okB:
			d.Changed = append(d.Changed, Change{Name: name, Before: fmt.Sprintf("%s v%d", k, x.Version), After: "(absent)"})
		case x.Version != y.Version || x.Hash != y.Hash:
			d.Changed = append(d.Changed, Change{Name: name, Before: fmt.Sprintf("%s v%d", k, x.Version), After: fmt.Sprintf("%s v%d", k, y.Version)})
		default:
			d.Unchanged = append(d.Unchanged, name)
		}
	}
	return d
}

func describe(n NamedInput) string {
	if n.Detail != "" {
		return n.Detail
	}
	if len(n.Hash) > 12 {
		return n.Hash[:12]
	}
	if n.Hash == "" {
		return "(none)"
	}
	return n.Hash
}
