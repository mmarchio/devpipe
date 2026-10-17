package archtest

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Module is the module path of this repository (see go.mod).
const Module = "overseer"

// domainPackages are the packages that must stay infrastructure-independent.
var domainPackages = []string{
	"project", "phase", "wbs", "state", "role", "prompt",
	"compression", "worker", "execution", "orchestration",
	"cache", "persistence", "statefile", "precompute", "workerresult", "continuation",
	"hardening", "readmodel", "access", "audit",
	"resilience", "faults", "recovery", "invariants", "metrics", "clock", "health", "retention",
}

// infraImports are import paths prohibited in any domain package.
var infraImports = []string{
	Module + "/internal/database",
	Module + "/internal/web",
	Module + "/internal/gateway",
	Module + "/internal/compressionprovider",
	// Phase 16 infrastructure: process execution, git writes, Go source
	// analysis and rewriting stay out of the domain (PLAN P16 Rules).
	Module + "/internal/repoinspect",
	Module + "/internal/valrun",
	Module + "/internal/gitcheckpoint",
	Module + "/internal/goanalysis",
	Module + "/internal/gorefactor",
	"net/http",
	"os/exec",
}

// concreteCompressionProviders are concrete prompt-compression provider
// implementations that internal/compression must not import (T1.14.4).
var concreteCompressionProviders = []string{
	Module + "/internal/gateway",
	Module + "/internal/compressionprovider",
}

// compressionImportAllowed is the closed import set of the compression
// domain (T9.1.3: no provider-specific type can leak into the domain): the
// standard library (no net/*), uuid, other domain packages, and its own
// subpackages.
func compressionImportAllowed(imp string) bool {
	if strings.HasPrefix(imp, Module+"/internal/") {
		rest := strings.TrimPrefix(imp, Module+"/internal/")
		return isDomain(strings.SplitN(rest, "/", 2)[0])
	}
	switch {
	case imp == "github.com/google/uuid":
		return true
	case strings.HasPrefix(imp, "net"):
		return false
	case !strings.Contains(strings.SplitN(imp, "/", 2)[0], "."):
		return !strings.HasPrefix(imp, Module+"/")
	}
	return false
}

// workerAdapters are concrete worker adapter packages (WBS 12.90): only
// the composition root (internal/app) and the gateway tree itself may
// import them; domain, orchestration, execution, service and web code
// speak the generic internal/worker contract.
var workerAdapters = []string{Module + "/internal/gateway/hermes"}

// adapterImporters are the internal directories allowed to import a
// concrete worker adapter.
var adapterImporters = []string{"app", "gateway"}

// CheckAdapterImports applies the worker-adapter boundary to one
// non-test file. dir is the package directory relative to internal/.
func CheckAdapterImports(dir string, imports []string) []Violation {
	top := strings.SplitN(dir, "/", 2)[0]
	for _, ok := range adapterImporters {
		if top == ok {
			return nil
		}
	}
	var out []Violation
	for _, imp := range imports {
		for _, a := range workerAdapters {
			if imp == a || strings.HasPrefix(imp, a+"/") {
				out = append(out, Violation{Package: dir, Import: imp, Rule: "worker-adapter-boundary"})
			}
		}
	}
	return out
}

// Violation is a single detected architectural dependency.
type Violation struct {
	Package string
	Import  string
	File    string
	Rule    string
}

func (v Violation) String() string {
	return fmt.Sprintf("%s imports %s in %s [%s]", v.Package, v.Import, v.File, v.Rule)
}

// isDomain reports whether the package name is a domain package.
func isDomain(pkg string) bool {
	for _, d := range domainPackages {
		if pkg == d {
			return true
		}
	}
	return false
}

// CheckImports applies the Phase 1 dependency rules to one package's import
// set. packageDir is the package name (e.g. "project"); imports are full
// import paths. It returns one violation per prohibited import.
func CheckImports(packageDir string, imports []string) []Violation {
	var out []Violation
	if !isDomain(packageDir) {
		return nil
	}
	for _, imp := range imports {
		for _, infra := range infraImports {
			if imp == infra || strings.HasPrefix(imp, infra+"/") {
				out = append(out, Violation{Package: packageDir, Import: imp, Rule: "domain-infra"})
				break
			}
		}
		if packageDir == "compression" {
			if !compressionImportAllowed(imp) {
				out = append(out, Violation{Package: packageDir, Import: imp, Rule: "compression-allowlist"})
			}
			for _, prov := range concreteCompressionProviders {
				if imp == prov || strings.HasPrefix(imp, prov+"/") {
					out = append(out, Violation{Package: packageDir, Import: imp, Rule: "compression-provider"})
					break
				}
			}
		}
	}
	return out
}

// WalkModule scans all .go files under root/internal and applies CheckImports
// to every domain package it finds. It returns all violations found.
func WalkModule(root string) ([]Violation, error) {
	var out []Violation
	internal := filepath.Join(root, "internal")
	err := filepath.WalkDir(internal, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		pkg := f.Name.Name
		var imports []string
		for _, imp := range f.Imports {
			imports = append(imports, strings.Trim(imp.Path.Value, `"`))
		}
		rel, _ := filepath.Rel(internal, filepath.Dir(path))
		for _, v := range CheckAdapterImports(filepath.ToSlash(rel), imports) {
			v.File = path
			out = append(out, v)
		}
		for _, v := range CheckWebImports(filepath.ToSlash(rel), imports) {
			v.File = path
			out = append(out, v)
		}
		for _, v := range CheckWebRuntimeWrites(filepath.ToSlash(rel), f) {
			v.File = path
			out = append(out, v)
		}
		if !isDomain(pkg) {
			return nil
		}
		for _, v := range CheckImports(pkg, imports) {
			v.File = path
			out = append(out, v)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Import < out[j].Import
	})
	return out, nil
}

// Main is the standalone entrypoint for `go run ./internal/archtest`.
// It exits non-zero when violations are found.
func Main() int {
	root, err := moduleRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "archtest:", err)
		return 2
	}
	v, err := WalkModule(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "archtest:", err)
		return 2
	}
	if len(v) == 0 {
		return 0
	}
	for _, x := range v {
		fmt.Fprintln(os.Stderr, x)
	}
	return 1
}

func moduleRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for d := wd; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d, nil
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", fmt.Errorf("no go.mod found above %s", wd)
		}
	}
}
