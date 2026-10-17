package hardening

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// Signature is a function's type contract with every type spelled fully
// qualified by import path (WBS 16.97).
type Signature struct {
	TypeParams []string `json:"type_params,omitempty"`
	Params     []string `json:"params"`
	Results    []string `json:"results"`
	Variadic   bool     `json:"variadic,omitempty"`
}

// String renders the signature canonically.
func (s Signature) String() string {
	var b strings.Builder
	if len(s.TypeParams) > 0 {
		b.WriteString("[" + strings.Join(s.TypeParams, ", ") + "]")
	}
	b.WriteString("(" + strings.Join(s.Params, ", "))
	if s.Variadic {
		b.WriteString("...")
	}
	b.WriteString(") (" + strings.Join(s.Results, ", ") + ")")
	return b.String()
}

// Effects are the observable-state facts of a function body (WBS 16.99).
type Effects struct {
	// GlobalsRead / GlobalsWritten are package-level variables (qualified
	// "importpath.Name").
	GlobalsRead    []string `json:"globals_read,omitempty"`
	GlobalsWritten []string `json:"globals_written,omitempty"`
	// Impure are calls into effectful standard packages (os, io, net,
	// time, math/rand, sync, …), qualified.
	Impure []string `json:"impure,omitempty"`
}

// Key canonicalizes the effect set for comparison.
func (e Effects) Key() string {
	norm := func(x []string) string {
		c := append([]string(nil), x...)
		sort.Strings(c)
		return strings.Join(c, ",")
	}
	return "r:" + norm(e.GlobalsRead) + ";w:" + norm(e.GlobalsWritten) + ";i:" + norm(e.Impure)
}

// FunctionInfo is one top-level function or method in the inventory
// (WBS 16.92), produced by language-aware analysis.
type FunctionInfo struct {
	ImportPath string `json:"import_path"`
	Package    string `json:"package"`
	// Dir and File are module-relative ('/' separators).
	Dir      string `json:"dir"`
	File     string `json:"file"`
	Symbol   string `json:"symbol"`
	Receiver string `json:"receiver,omitempty"`
	// ReceiverUsed: the body refers to the receiver (WBS 16.98).
	ReceiverUsed bool      `json:"receiver_used,omitempty"`
	Signature    Signature `json:"signature"`
	Exported     bool      `json:"exported"`
	Statements   int       `json:"statements"`
	// BodyHash hashes the exact source text; Fingerprint the normalized
	// behaviour (WBS 16.94/16.95).
	BodyHash    string `json:"body_hash"`
	Fingerprint string `json:"fingerprint"`
	// Imports are the import paths the body uses; Calls are the resolved
	// callees ("importpath.Name").
	Imports []string `json:"imports,omitempty"`
	Calls   []string `json:"calls,omitempty"`
	// LocalDeps are package-local declarations the body depends on
	// (functions, types, constants, variables of its own package).
	LocalDeps []string `json:"local_deps,omitempty"`
	Effects   Effects  `json:"effects"`
	// References are module locations referencing the symbol; Tests are
	// test functions exercising it.
	References []Reference `json:"references,omitempty"`
	Tests      []string    `json:"tests,omitempty"`
}

// QualifiedName is "importpath.Symbol" or "importpath.Recv.Symbol".
func (f FunctionInfo) QualifiedName() string {
	if f.Receiver != "" {
		return f.ImportPath + "." + f.Receiver + "." + f.Symbol
	}
	return f.ImportPath + "." + f.Symbol
}

// Reference is one resolved use of a symbol (WBS 16.119).
type Reference struct {
	ImportPath string `json:"import_path"`
	File       string `json:"file"`
	Line       int    `json:"line"`
	Test       bool   `json:"test"`
	// Kind: CALL, VALUE (function value / callback).
	Kind string `json:"kind"`
}

// DuplicateGroup is a set of behaviourally identical functions
// (WBS 16.96).
type DuplicateGroup struct {
	ID          string         `json:"id"`
	Fingerprint string         `json:"fingerprint"`
	Members     []FunctionInfo `json:"members"`
	Packages    []string       `json:"packages"`
	References  []Reference    `json:"references,omitempty"`
	Tests       []string       `json:"tests,omitempty"`
}

// GroupID derives the group's stable identity from its fingerprint.
func GroupID(fingerprint string) string {
	h := sha256.Sum256([]byte(fingerprint))
	return "dup-" + hex.EncodeToString(h[:8])
}

// GroupByFingerprint groups functions sharing a non-empty fingerprint into
// duplicate groups of ≥ 2 members (WBS 16.96). Output is deterministic:
// members sorted by qualified name, groups by ID.
func GroupByFingerprint(fns []FunctionInfo) []DuplicateGroup {
	by := map[string][]FunctionInfo{}
	for _, f := range fns {
		if f.Fingerprint != "" {
			by[f.Fingerprint] = append(by[f.Fingerprint], f)
		}
	}
	var out []DuplicateGroup
	for fp, ms := range by {
		if len(ms) < 2 {
			continue
		}
		sort.Slice(ms, func(i, j int) bool { return ms[i].QualifiedName() < ms[j].QualifiedName() })
		g := DuplicateGroup{ID: GroupID(fp), Fingerprint: fp, Members: ms}
		pk, refs, tests := map[string]bool{}, map[string]bool{}, map[string]bool{}
		for _, m := range ms {
			pk[m.ImportPath] = true
			for _, r := range m.References {
				key := r.File + ":" + itoa(r.Line)
				if !refs[key] {
					refs[key] = true
					g.References = append(g.References, r)
				}
			}
			for _, t := range m.Tests {
				tests[t] = true
			}
		}
		g.Packages = keys(pk)
		g.Tests = keys(tests)
		sort.Slice(g.References, func(i, j int) bool {
			if g.References[i].File != g.References[j].File {
				return g.References[i].File < g.References[j].File
			}
			return g.References[i].Line < g.References[j].Line
		})
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
