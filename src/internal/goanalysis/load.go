// Package goanalysis is the language-aware Go source analysis used by the
// final hardening pass (WBS 16.92–16.95, 16.103, 16.114–16.119). It loads
// a module with `go list`, type-checks every package from source with
// go/types (dependencies outside the module come from compiler export
// data), and answers inventory, fingerprint, reference and test questions
// from resolved type information — never from regular expressions, so a
// name inside a string or comment is never a reference (WBS 16.93).
package goanalysis

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"overseer/internal/hardening"
)

// listPackage is the subset of `go list -json` output used.
type listPackage struct {
	Dir          string
	ImportPath   string
	Name         string
	Export       string
	ForTest      string
	Standard     bool
	GoFiles      []string
	TestGoFiles  []string
	XTestGoFiles []string
	Imports      []string
	Module       *struct{ Path, Dir string }
	Error        *struct{ Err string }
}

// File is one parsed source file.
type File struct {
	// Path is absolute; Rel is module-relative ('/' separators).
	Path string
	Rel  string
	AST  *ast.File
	// Test: a _test.go file; XTest: in the external test package.
	Test  bool
	XTest bool
	// Excluded (WBS 16.91) and why.
	Excluded       bool
	ExcludedReason string
	// Info is the type information covering this file.
	Info *types.Info
}

// Package is one module package.
type Package struct {
	ImportPath string
	Name       string
	Dir        string
	RelDir     string
	// Imports are the production imports (all, incl. standard library).
	Imports []string
	Files   []*File
	// Types is the production type-checked package.
	Types *types.Package
}

// Module is a loaded, type-checked Go module.
type Module struct {
	Path     string
	Dir      string
	Fset     *token.FileSet
	Packages []*Package
	byPath   map[string]*Package
}

// Package returns a module package by import path.
func (m *Module) Package(path string) *Package { return m.byPath[path] }

// ProductionFiles returns the package's non-test files.
func (p *Package) ProductionFiles() []*File {
	var out []*File
	for _, f := range p.Files {
		if !f.Test {
			out = append(out, f)
		}
	}
	return out
}

// PackageInfos converts the module packages for the hardening domain,
// keeping only module-internal imports.
func (m *Module) PackageInfos() []hardening.PackageInfo {
	var out []hardening.PackageInfo
	for _, p := range m.Packages {
		var imps []string
		for _, i := range p.Imports {
			if m.byPath[i] != nil {
				imps = append(imps, i)
			}
		}
		out = append(out, hardening.PackageInfo{ImportPath: p.ImportPath, Name: p.Name, Dir: p.RelDir, Imports: imps})
	}
	return out
}

// ImportGraph is the module's production package import DAG (WBS 16.103).
func (m *Module) ImportGraph() *hardening.ImportGraph {
	return hardening.NewImportGraph(m.PackageInfos())
}

// Options configure loading.
type Options struct {
	// GoBinary is the go command (default "go").
	GoBinary string
	// Exclusions are extra excluded globs (hardening settings).
	Exclusions []string
}

// ErrLoad reports a module that cannot be listed or type-checked.
var ErrLoad = errors.New("goanalysis: load failed")

func goList(ctx context.Context, bin, dir string) ([]listPackage, error) {
	cmd := exec.CommandContext(ctx, bin, "list", "-e", "-export", "-deps", "-test", "-json", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOWORK=off")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: go list: %v: %s", ErrLoad, err, strings.TrimSpace(stderr.String()))
	}
	dec := json.NewDecoder(&stdout)
	var out []listPackage
	for {
		var p listPackage
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("%w: decode go list: %v", ErrLoad, err)
		}
		out = append(out, p)
	}
	return out, nil
}

// LoadModule lists, parses and type-checks the module rooted at dir
// (WBS 16.92/16.103). Production files of every package are checked in
// import order; test files are checked in a test variant (internal tests)
// or as the external test package. Objects are matched across variants by
// ObjectKey, never by pointer.
func LoadModule(ctx context.Context, dir string, opt Options) (*Module, error) {
	bin := opt.GoBinary
	if bin == "" {
		bin = "go"
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	list, err := goList(ctx, bin, abs)
	if err != nil {
		return nil, err
	}
	m := &Module{Dir: abs, Fset: token.NewFileSet(), byPath: map[string]*Package{}}
	export := map[string]string{}
	var mods []listPackage
	for _, lp := range list {
		if lp.ForTest != "" || strings.HasSuffix(lp.ImportPath, ".test") || strings.Contains(lp.ImportPath, " [") {
			continue
		}
		if lp.Module != nil && !lp.Standard && sameDir(lp.Module.Dir, abs) {
			if m.Path == "" {
				m.Path = lp.Module.Path
			}
			if lp.Error != nil {
				return nil, fmt.Errorf("%w: %s: %s", ErrLoad, lp.ImportPath, lp.Error.Err)
			}
			mods = append(mods, lp)
			continue
		}
		if lp.Export != "" {
			export[lp.ImportPath] = lp.Export
		}
	}
	sort.Slice(mods, func(i, j int) bool { return mods[i].ImportPath < mods[j].ImportPath })
	gc := importer.ForCompiler(m.Fset, "gc", func(path string) (io.ReadCloser, error) {
		f, ok := export[path]
		if !ok {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(f)
	}).(types.ImporterFrom)
	for _, lp := range mods {
		p := &Package{ImportPath: lp.ImportPath, Name: lp.Name, Dir: lp.Dir, Imports: lp.Imports}
		p.RelDir = rel(abs, lp.Dir)
		for _, set := range []struct {
			names       []string
			test, xtest bool
		}{{lp.GoFiles, false, false}, {lp.TestGoFiles, true, false}, {lp.XTestGoFiles, true, true}} {
			for _, n := range set.names {
				path := filepath.Join(lp.Dir, n)
				af, err := parser.ParseFile(m.Fset, path, nil, parser.ParseComments)
				if err != nil {
					return nil, fmt.Errorf("%w: parse %s: %v", ErrLoad, path, err)
				}
				f := &File{Path: path, Rel: rel(abs, path), AST: af, Test: set.test, XTest: set.xtest}
				f.Excluded, f.ExcludedReason = hardening.IsExcluded(f.Rel, header(af), opt.Exclusions)
				p.Files = append(p.Files, f)
			}
		}
		m.Packages = append(m.Packages, p)
		m.byPath[p.ImportPath] = p
	}
	ld := &loader{m: m, gc: gc, state: map[string]int{}}
	for _, p := range m.Packages {
		if err := ld.checkProduction(p); err != nil {
			return nil, err
		}
	}
	for _, p := range m.Packages {
		if err := ld.checkTests(p); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func sameDir(a, b string) bool {
	ea, err1 := filepath.EvalSymlinks(a)
	eb, err2 := filepath.EvalSymlinks(b)
	if err1 == nil && err2 == nil {
		return ea == eb
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

func rel(root, p string) string {
	r, err := filepath.Rel(root, p)
	if err != nil {
		return filepath.ToSlash(p)
	}
	if ea, e1 := filepath.EvalSymlinks(root); e1 == nil {
		if ep, e2 := filepath.EvalSymlinks(p); e2 == nil {
			if r2, e3 := filepath.Rel(ea, ep); e3 == nil {
				r = r2
			}
		}
	}
	return filepath.ToSlash(r)
}

// header returns the comment text before the package clause.
func header(f *ast.File) string {
	var b strings.Builder
	for _, cg := range f.Comments {
		if cg.Pos() >= f.Package {
			break
		}
		for _, c := range cg.List {
			b.WriteString(c.Text + "\n")
		}
	}
	return b.String()
}

func newInfo() *types.Info {
	return &types.Info{
		Types:      map[ast.Expr]types.TypeAndValue{},
		Defs:       map[*ast.Ident]types.Object{},
		Uses:       map[*ast.Ident]types.Object{},
		Implicits:  map[ast.Node]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
		Scopes:     map[ast.Node]*types.Scope{},
	}
}

type loader struct {
	m     *Module
	gc    types.ImporterFrom
	state map[string]int // 1 = checking, 2 = done
}

// importerFor resolves module packages to their production check and
// everything else to export data.
type importerFor struct{ ld *loader }

func (i importerFor) Import(path string) (*types.Package, error) {
	return i.ImportFrom(path, "", 0)
}

func (i importerFor) ImportFrom(path, dir string, mode types.ImportMode) (*types.Package, error) {
	if path == "unsafe" {
		return types.Unsafe, nil
	}
	if p := i.ld.m.byPath[path]; p != nil {
		if err := i.ld.checkProduction(p); err != nil {
			return nil, err
		}
		return p.Types, nil
	}
	return i.ld.gc.ImportFrom(path, dir, mode)
}

func (ld *loader) conf() *types.Config {
	return &types.Config{Importer: importerFor{ld}, Error: func(error) {}}
}

func (ld *loader) checkProduction(p *Package) error {
	switch ld.state[p.ImportPath] {
	case 2:
		return nil
	case 1:
		return fmt.Errorf("%w: import cycle through %s", ErrLoad, p.ImportPath)
	}
	ld.state[p.ImportPath] = 1
	var files []*ast.File
	for _, f := range p.ProductionFiles() {
		files = append(files, f.AST)
	}
	info := newInfo()
	var firstErr error
	cfg := ld.conf()
	cfg.Error = func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	tp, _ := cfg.Check(p.ImportPath, ld.m.Fset, files, info)
	if firstErr != nil {
		return fmt.Errorf("%w: type-check %s: %v", ErrLoad, p.ImportPath, firstErr)
	}
	p.Types = tp
	for _, f := range p.ProductionFiles() {
		f.Info = info
	}
	ld.state[p.ImportPath] = 2
	return nil
}

func (ld *loader) checkTests(p *Package) error {
	var internal, external []*File
	for _, f := range p.Files {
		switch {
		case f.XTest:
			external = append(external, f)
		case f.Test:
			internal = append(internal, f)
		}
	}
	if len(internal) > 0 {
		var files []*ast.File
		for _, f := range p.ProductionFiles() {
			files = append(files, f.AST)
		}
		for _, f := range internal {
			files = append(files, f.AST)
		}
		info := newInfo()
		var firstErr error
		cfg := ld.conf()
		cfg.Error = func(err error) {
			if firstErr == nil {
				firstErr = err
			}
		}
		_, _ = cfg.Check(p.ImportPath, ld.m.Fset, files, info)
		if firstErr != nil {
			return fmt.Errorf("%w: type-check %s tests: %v", ErrLoad, p.ImportPath, firstErr)
		}
		for _, f := range internal {
			f.Info = info
		}
	}
	if len(external) > 0 {
		var files []*ast.File
		for _, f := range external {
			files = append(files, f.AST)
		}
		info := newInfo()
		var firstErr error
		cfg := ld.conf()
		cfg.Error = func(err error) {
			if firstErr == nil {
				firstErr = err
			}
		}
		_, _ = cfg.Check(p.ImportPath+"_test", ld.m.Fset, files, info)
		if firstErr != nil {
			return fmt.Errorf("%w: type-check %s external tests: %v", ErrLoad, p.ImportPath, firstErr)
		}
		for _, f := range external {
			f.Info = info
		}
	}
	return nil
}

// ObjectKey is the cross-variant identity of a package-level object:
// "importpath.Name" or "importpath.Recv.Name" for methods; "" for locals,
// fields and builtins.
func ObjectKey(obj types.Object) string {
	if obj == nil || obj.Pkg() == nil {
		return ""
	}
	path := strings.TrimSuffix(obj.Pkg().Path(), "_test")
	if fn, ok := obj.(*types.Func); ok {
		if sig, ok := fn.Type().(*types.Signature); ok && sig.Recv() != nil {
			return path + "." + recvName(sig.Recv().Type()) + "." + fn.Name()
		}
		return path + "." + fn.Name()
	}
	if obj.Parent() == obj.Pkg().Scope() {
		return path + "." + obj.Name()
	}
	return ""
}

func recvName(t types.Type) string {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	if n, ok := t.(*types.Named); ok {
		return n.Obj().Name()
	}
	return types.TypeString(t, nil)
}

// Qualifier spells types fully qualified by import path.
func Qualifier(p *types.Package) string { return p.Path() }
