package hardening

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// PackageInfo is one module package (from package loading).
type PackageInfo struct {
	ImportPath string   `json:"import_path"`
	Name       string   `json:"name"`
	Dir        string   `json:"dir"`
	Imports    []string `json:"imports,omitempty"`
}

// Destination kinds.
const (
	DestUtility = "UTILITY"
	DestDomain  = "DOMAIN"
)

// Destination is where a consolidated function goes.
type Destination struct {
	ImportPath string `json:"import_path"`
	Name       string `json:"name"`
	// Dir is module-relative.
	Dir string `json:"dir"`
	// Exists: an existing package is reused (WBS 16.100).
	Exists bool   `json:"exists"`
	Kind   string `json:"kind"`
	// Symbol is the exported name in the destination.
	Symbol string `json:"symbol"`
}

// Utility categories (WBS 16.101). Never a generic "util".
const (
	CatString = "stringutil"
	CatPath   = "pathutil"
	CatSlice  = "sliceutil"
	CatHash   = "hashutil"
)

var stringish = map[string]bool{"string": true, "rune": true, "byte": true, "bool": true, "int": true, "[]string": true, "[]byte": true, "[]rune": true}

// UtilityCategory classifies a group by what its members operate on:
// path packages → pathutil, crypto / hash packages → hashutil, string-like
// contracts → stringutil, slice contracts → sliceutil. "" = no narrowly
// scoped category (REQUIRES_REVIEW).
func UtilityCategory(members []FunctionInfo) string {
	if len(members) == 0 {
		return ""
	}
	m := members[0]
	for _, imp := range m.Imports {
		if imp == "path" || imp == "path/filepath" {
			return CatPath
		}
	}
	for _, imp := range m.Imports {
		if strings.HasPrefix(imp, "crypto/") || strings.HasPrefix(imp, "hash") {
			return CatHash
		}
	}
	types := append(append([]string(nil), m.Signature.Params...), m.Signature.Results...)
	if len(types) == 0 {
		return ""
	}
	allString, anySlice := true, false
	for _, t := range types {
		if !stringish[t] {
			allString = false
		}
		if strings.HasPrefix(t, "[]") {
			anySlice = true
		}
	}
	hasString := false
	for _, t := range types {
		if t == "string" || t == "[]string" {
			hasString = true
		}
	}
	switch {
	case allString && hasString:
		return CatString
	case anySlice:
		return CatSlice
	}
	return ""
}

// ChooseUtilityPackage picks the utility destination (WBS 16.100/16.101):
// an existing package named after the category is reused; otherwise a new
// package <module>/<newBase>/<category>. Returns nil and a reason when no
// narrowly scoped category applies.
func ChooseUtilityPackage(members []FunctionInfo, existing []PackageInfo, modulePath, newBase string) (*Destination, string) {
	cat := UtilityCategory(members)
	if cat == "" {
		return nil, "no narrowly scoped utility package fits the group"
	}
	sym := exportName(members[0].Symbol)
	var found []PackageInfo
	for _, p := range existing {
		if p.Name == cat {
			found = append(found, p)
		}
	}
	if len(found) > 0 {
		sort.Slice(found, func(i, j int) bool { return found[i].ImportPath < found[j].ImportPath })
		p := found[0]
		return &Destination{ImportPath: p.ImportPath, Name: p.Name, Dir: p.Dir, Exists: true, Kind: DestUtility, Symbol: sym}, ""
	}
	dir := path.Join(newBase, cat)
	return &Destination{ImportPath: path.Join(modulePath, dir), Name: cat, Dir: dir, Kind: DestUtility, Symbol: sym}, ""
}

// ChooseDomainOwner picks a lower-level domain package that every member
// already depends on (WBS 16.102): domain-specific shared behaviour stays
// in its domain instead of a generic utility. nil = no domain owner.
func ChooseDomainOwner(members []FunctionInfo, packages []PackageInfo, domain []string) *Destination {
	if len(members) == 0 {
		return nil
	}
	isDomain := map[string]bool{}
	for _, d := range domain {
		isDomain[d] = true
	}
	memberPkg := map[string]bool{}
	for _, m := range members {
		memberPkg[m.ImportPath] = true
	}
	common := map[string]int{}
	for _, m := range members {
		seen := map[string]bool{}
		for _, imp := range m.Imports {
			if isDomain[imp] && !memberPkg[imp] && !seen[imp] {
				seen[imp] = true
				common[imp]++
			}
		}
	}
	var cands []string
	for imp, n := range common {
		if n == len(members) {
			cands = append(cands, imp)
		}
	}
	if len(cands) == 0 {
		return nil
	}
	sort.Strings(cands)
	for _, p := range packages {
		if p.ImportPath == cands[0] {
			return &Destination{ImportPath: p.ImportPath, Name: p.Name, Dir: p.Dir, Exists: true, Kind: DestDomain, Symbol: exportName(members[0].Symbol)}
		}
	}
	return nil
}

func exportName(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// ImportGraph is a package import DAG (WBS 16.103): edges from a package
// to the module packages it imports.
type ImportGraph struct {
	edges map[string]map[string]bool
}

// NewImportGraph builds a graph from packages (imports outside the given
// set are kept as nodes; they cannot close a cycle back into the module).
func NewImportGraph(pkgs []PackageInfo) *ImportGraph {
	g := &ImportGraph{edges: map[string]map[string]bool{}}
	for _, p := range pkgs {
		g.node(p.ImportPath)
		for _, imp := range p.Imports {
			g.AddEdge(p.ImportPath, imp)
		}
	}
	return g
}

func (g *ImportGraph) node(p string) {
	if g.edges[p] == nil {
		g.edges[p] = map[string]bool{}
	}
}

// AddEdge records from → to.
func (g *ImportGraph) AddEdge(from, to string) {
	g.node(from)
	g.node(to)
	g.edges[from][to] = true
}

// Imports lists a package's direct imports, sorted.
func (g *ImportGraph) Imports(p string) []string { return keys(g.edges[p]) }

// Clone deep-copies the graph.
func (g *ImportGraph) Clone() *ImportGraph {
	c := &ImportGraph{edges: map[string]map[string]bool{}}
	for f, ts := range g.edges {
		c.node(f)
		for t := range ts {
			c.edges[f][t] = true
		}
	}
	return c
}

// Prospective simulates the consolidation (WBS 16.104): the destination
// package importing destImports, and every migrated package importing the
// destination.
func (g *ImportGraph) Prospective(dest string, destImports, migrated []string) *ImportGraph {
	c := g.Clone()
	c.node(dest)
	for _, imp := range destImports {
		if imp != dest {
			c.AddEdge(dest, imp)
		}
	}
	for _, m := range migrated {
		if m != dest {
			c.AddEdge(m, dest)
		}
	}
	return c
}

// FindCycle returns one import cycle as a path whose last element repeats
// the first (direct or transitive, WBS 16.105), or nil. Deterministic.
func (g *ImportGraph) FindCycle() []string {
	color := map[string]int{}
	var stack, found []string
	var visit func(n string) bool
	visit = func(n string) bool {
		color[n] = 1
		stack = append(stack, n)
		for _, t := range keys(g.edges[n]) {
			switch color[t] {
			case 0:
				if visit(t) {
					return true
				}
			case 1:
				for i, s := range stack {
					if s == t {
						found = append(append([]string(nil), stack[i:]...), t)
						return true
					}
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[n] = 2
		return false
	}
	for _, n := range keys(boolKeys(g.edges)) {
		if color[n] == 0 && visit(n) {
			return found
		}
	}
	return nil
}

func boolKeys(m map[string]map[string]bool) map[string]bool {
	out := map[string]bool{}
	for k := range m {
		out[k] = true
	}
	return out
}

// Layers is the project architecture used for placement (WBS 16.106):
// domain packages must stay infrastructure-independent; infra packages
// are the infrastructure layer.
type Layers struct {
	Domain []string `json:"domain"`
	Infra  []string `json:"infra"`
}

func inSet(p string, set []string) bool {
	for _, s := range set {
		if p == s || strings.HasPrefix(p, s+"/") {
			return true
		}
	}
	return false
}

// CheckLayer validates placement against the architecture (WBS 16.106): a
// helper whose members all live in domain packages is never moved into an
// infrastructure package, and a domain destination never gains an
// infrastructure import. "" = allowed.
func CheckLayer(dest Destination, members []FunctionInfo, destImports []string, l Layers) string {
	allDomain := len(members) > 0
	for _, m := range members {
		if !inSet(m.ImportPath, l.Domain) {
			allDomain = false
		}
	}
	if allDomain && inSet(dest.ImportPath, l.Infra) {
		return fmt.Sprintf("domain helper would move into infrastructure package %s", dest.ImportPath)
	}
	if inSet(dest.ImportPath, l.Domain) {
		for _, imp := range destImports {
			if inSet(imp, l.Infra) {
				return fmt.Sprintf("domain package %s would import infrastructure package %s", dest.ImportPath, imp)
			}
		}
	}
	return ""
}

// AssessValue applies the consolidation value policy (WBS 16.107): a
// group whose functions are trivial (fewer than MinStatements statements)
// is retained as LOW_VALUE — the shared dependency costs more than the
// duplication. "" = worth consolidating.
func AssessValue(members []FunctionInfo, s Settings) string {
	max := 0
	for _, m := range members {
		if m.Statements > max {
			max = m.Statements
		}
	}
	if max < s.MinStatements {
		return fmt.Sprintf("trivial duplication (%d statements < %d)", max, s.MinStatements)
	}
	return ""
}
