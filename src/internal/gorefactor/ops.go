package gorefactor

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go/types"

	"overseer/internal/goanalysis"
	"overseer/internal/hardening"
)

func isPkgName(o types.Object) bool {
	_, ok := o.(*types.PkgName)
	return ok
}

// declText renders a function declaration (without its doc comment) with
// a new name. Qualifiers naming the destination package itself (the body
// already used it, e.g. a domain owner's types or a function consolidated
// in an earlier round) are dropped: the copy lives in that package.
func declText(m *goanalysis.Module, f *goanalysis.File, src []byte, fd *ast.FuncDecl, newName, destPath string) string {
	start, end := offset(m, fd.Pos()), offset(m, fd.End())
	edits := []edit{{offset(m, fd.Name.Pos()) - start, offset(m, fd.Name.End()) - start, newName}}
	if f.Info != nil {
		ast.Inspect(fd, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if pn, ok := f.Info.Uses[id].(*types.PkgName); ok && pn.Imported().Path() == destPath {
				edits = append(edits, edit{offset(m, sel.Pos()) - start, offset(m, sel.Sel.Pos()) - start, ""})
			}
			return true
		})
	}
	return string(apply(src[start:end], edits))
}

// normalizeDecl canonicalizes a declaration's source for comparison.
func normalizeDecl(text string) string {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", "package x\n"+text, 0)
	if err != nil || len(f.Decls) == 0 {
		return strings.TrimSpace(text)
	}
	var b bytes.Buffer
	_ = printer.Fprint(&b, fset, f.Decls[0])
	return b.String()
}

// CopyFunction copies a group member into the destination package under
// dest.Symbol (WBS 16.112): originals stay untouched, the body's imports
// come along, and the file is formatted. If the destination already
// declares the symbol with the same body the call is a no-op (returns
// false); a different declaration is ErrConflict.
func CopyFunction(m *goanalysis.Module, fn hardening.FunctionInfo, dest hardening.Destination, sources []string) (bool, error) {
	f := fileByRel(m, fn.File)
	if f == nil {
		return false, fmt.Errorf("%w: file %s", ErrNotFound, fn.File)
	}
	fd := findFunc(f, fn.Symbol)
	if fd == nil {
		return false, fmt.Errorf("%w: %s", ErrNotFound, fn.QualifiedName())
	}
	src, err := os.ReadFile(f.Path)
	if err != nil {
		return false, err
	}
	text := declText(m, f, src, fd, dest.Symbol, dest.ImportPath)
	destDir := filepath.Join(m.Dir, filepath.FromSlash(dest.Dir))
	if existing, err := existingDecl(destDir, dest.Symbol); err != nil {
		return false, err
	} else if existing != "" {
		if normalizeDecl(existing) == normalizeDecl(text) {
			return false, nil
		}
		return false, fmt.Errorf("%w: %s.%s already exists with different content", ErrConflict, dest.ImportPath, dest.Symbol)
	}
	var add []importSpec
	for _, p := range fn.Imports {
		if p == dest.ImportPath {
			continue
		}
		spec := importSpec{Path: p}
		for _, is := range f.AST.Imports {
			if strings.Trim(is.Path.Value, "\"`") == p && is.Name != nil {
				spec.Name = is.Name.Name
			}
		}
		add = append(add, spec)
	}
	if err := confined(m.Dir, destDir); err != nil {
		return false, err
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return false, err
	}
	target := filepath.Join(destDir, snake(dest.Symbol)+".go")
	var body bytes.Buffer
	if old, err := os.ReadFile(target); err == nil {
		body.Write(old)
		body.WriteString("\n")
	} else {
		fmt.Fprintf(&body, "package %s\n", dest.Name)
	}
	fmt.Fprintf(&body, "\n// %s is the shared implementation consolidated by overseer from: %s.\n%s\n",
		dest.Symbol, strings.Join(sources, ", "), text)
	out, err := fixImports(body.Bytes(), add, nil)
	if err != nil {
		return false, err
	}
	return writeIfChanged(m.Dir, target, out)
}

// existingDecl returns the source of a top-level function named sym in the
// package directory ("" = none).
func existingDecl(dir, sym string) (string, error) {
	ents, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		src, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		fset := token.NewFileSet()
		af, err := parser.ParseFile(fset, p, src, 0)
		if err != nil {
			return "", err
		}
		for _, d := range af.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == sym {
				return string(src[fset.Position(fd.Pos()).Offset:fset.Position(fd.End()).Offset]), nil
			}
		}
	}
	return "", nil
}

// SharedTestFile is the module-relative path of a destination's
// consolidated test.
func SharedTestFile(dest hardening.Destination) string {
	return dest.Dir + "/" + snake(dest.Symbol) + "_shared_test.go"
}

func plainType(t string) bool { return !strings.ContainsAny(t, "./") }

// WriteSharedTests writes the consolidated table-driven test of the shared
// function (WBS 16.113/16.117): one row per distinct behavioural case,
// with its provenance. Signatures with package-qualified types or other
// than one result get a compile-only test. Deterministic: rewriting the
// same matrix changes nothing.
func WriteSharedTests(moduleDir string, dest hardening.Destination, sig hardening.Signature, rows []hardening.MatrixRow) (bool, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "package %s\n\n", dest.Name)
	tableable := len(sig.Results) == 1 && plainType(sig.Results[0])
	for _, p := range sig.Params {
		if !plainType(p) {
			tableable = false
		}
	}
	name := "Test" + dest.Symbol + "Shared"
	if !tableable || len(rows) == 0 {
		fmt.Fprintf(&b, "import \"testing\"\n\n// %s ensures the shared implementation exists (generated by overseer).\nfunc %s(t *testing.T) {\n\t_ = %s\n}\n", name, name, dest.Symbol)
	} else {
		b.WriteString("import (\n\t\"reflect\"\n\t\"testing\"\n)\n\n")
		fmt.Fprintf(&b, "// %s is the consolidated behavioural matrix of the duplicated\n// implementations (generated by overseer; every distinct case is kept).\nfunc %s(t *testing.T) {\n", name, name)
		b.WriteString("\tcases := []struct {\n\t\tname string\n")
		for i, p := range sig.Params {
			if sig.Variadic && i == len(sig.Params)-1 {
				fmt.Fprintf(&b, "\t\tin%d %s\n", i, p)
				continue
			}
			fmt.Fprintf(&b, "\t\tin%d %s\n", i, p)
		}
		fmt.Fprintf(&b, "\t\twant %s\n\t}{\n", sig.Results[0])
		for i, r := range rows {
			if len(r.Inputs) != len(sig.Params) || len(r.Expected) != 1 {
				continue
			}
			fmt.Fprintf(&b, "\t\t{name: %q", fmt.Sprintf("case-%d", i+1))
			for j, in := range r.Inputs {
				fmt.Fprintf(&b, ", in%d: %s", j, in)
			}
			fmt.Fprintf(&b, ", want: %s}, // from %s\n", r.Expected[0], strings.Join(r.Sources, ", "))
		}
		b.WriteString("\t}\n\tfor _, tc := range cases {\n")
		var args []string
		for i := range sig.Params {
			a := fmt.Sprintf("tc.in%d", i)
			if sig.Variadic && i == len(sig.Params)-1 {
				a += "..."
			}
			args = append(args, a)
		}
		fmt.Fprintf(&b, "\t\tgot := %s(%s)\n", dest.Symbol, strings.Join(args, ", "))
		b.WriteString("\t\tif !reflect.DeepEqual(got, tc.want) {\n\t\t\tt.Errorf(\"%s: got %v, want %v\", tc.name, got, tc.want)\n\t\t}\n\t}\n}\n")
	}
	out, err := format.Source(b.Bytes())
	if err != nil {
		return false, fmt.Errorf("gorefactor: format shared test: %w", err)
	}
	dir := filepath.Join(moduleDir, filepath.FromSlash(dest.Dir))
	if err := confined(moduleDir, dir); err != nil {
		return false, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	return writeIfChanged(moduleDir, filepath.Join(moduleDir, filepath.FromSlash(SharedTestFile(dest))), out)
}

// MigrateReferences rewrites, in one package (production and test files),
// every type-resolved use of an old symbol to the destination (WBS
// 16.119–16.121): unqualified uses and package selectors alike, function
// values and callbacks included. Comments and strings are never touched.
// The destination import is added and member-package imports that became
// unused are removed. Returns the number of references rewritten (0 when
// already migrated).
func MigrateReferences(m *goanalysis.Module, pkgPath string, old map[string]bool, oldPkgs map[string]string, dest hardening.Destination) (int, error) {
	p := m.Package(pkgPath)
	if p == nil {
		return 0, fmt.Errorf("%w: package %s", ErrNotFound, pkgPath)
	}
	total := 0
	for _, f := range p.Files {
		if f.Info == nil || f.Excluded {
			continue
		}
		src, err := os.ReadFile(f.Path)
		if err != nil {
			return total, err
		}
		inDest := pkgPath == dest.ImportPath && !f.XTest
		repl := dest.Name + "." + dest.Symbol
		if inDest {
			repl = dest.Symbol
		}
		var edits []edit
		seen := map[*ast.Ident]bool{}
		ast.Inspect(f.AST, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if obj := f.Info.Uses[sel.Sel]; obj != nil && old[goanalysis.ObjectKey(obj)] {
					if _, isPkg := sel.X.(*ast.Ident); isPkg {
						if xo := f.Info.Uses[sel.X.(*ast.Ident)]; xo != nil {
							if isPkgName(xo) {
								edits = append(edits, edit{offset(m, sel.Pos()), offset(m, sel.End()), repl})
								seen[sel.Sel] = true
								return false
							}
						}
					}
				}
			}
			if id, ok := n.(*ast.Ident); ok && !seen[id] {
				if obj := f.Info.Uses[id]; obj != nil && old[goanalysis.ObjectKey(obj)] {
					edits = append(edits, edit{offset(m, id.Pos()), offset(m, id.End()), repl})
				}
			}
			return true
		})
		if len(edits) == 0 {
			continue
		}
		out := apply(src, edits)
		var add []importSpec
		if !inDest {
			add = []importSpec{{Path: dest.ImportPath}}
		}
		cands := map[string]string{}
		for path, name := range oldPkgs {
			if path != pkgPath {
				cands[path] = name
			}
		}
		out, err = fixImports(out, add, cands)
		if err != nil {
			return total, fmt.Errorf("%s: %w", f.Rel, err)
		}
		if _, err := writeIfChanged(m.Dir, f.Path, out); err != nil {
			return total, err
		}
		total += len(edits)
	}
	return total, nil
}

// RemoveFunction deletes a duplicate implementation (and its doc comment)
// and drops imports only it used (WBS 16.127). Already removed → false.
func RemoveFunction(m *goanalysis.Module, fn hardening.FunctionInfo) (bool, error) {
	f := fileByRel(m, fn.File)
	if f == nil {
		return false, nil
	}
	fd := findFunc(f, fn.Symbol)
	if fd == nil {
		return false, nil
	}
	src, err := os.ReadFile(f.Path)
	if err != nil {
		return false, err
	}
	start := fd.Pos()
	if fd.Doc != nil {
		start = fd.Doc.Pos()
	}
	out := apply(src, []edit{{offset(m, start), offset(m, fd.End()), ""}})
	names := importNames(f)
	cands := map[string]string{}
	for _, p := range fn.Imports {
		if n, ok := names[p]; ok {
			cands[p] = n
		}
	}
	out, err = fixImports(out, nil, cands)
	if err != nil {
		return false, err
	}
	return writeIfChanged(m.Dir, f.Path, out)
}

// RemoveTests deletes helper-only test functions (WBS 16.125) after their
// cases moved into the shared matrix. A file left without declarations is
// deleted. Returns the number of tests removed.
func RemoveTests(m *goanalysis.Module, tests []hardening.TestInfo) (int, error) {
	byFile := map[string][]string{}
	for _, t := range tests {
		byFile[t.File] = append(byFile[t.File], t.Name)
	}
	files := make([]string, 0, len(byFile))
	for k := range byFile {
		files = append(files, k)
	}
	sort.Strings(files)
	removed := 0
	for _, rel := range files {
		f := fileByRel(m, rel)
		if f == nil {
			continue
		}
		src, err := os.ReadFile(f.Path)
		if err != nil {
			return removed, err
		}
		want := map[string]bool{}
		for _, n := range byFile[rel] {
			want[n] = true
		}
		var edits []edit
		left := 0
		for _, d := range f.AST.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if ok && fd.Recv == nil && want[fd.Name.Name] {
				start := fd.Pos()
				if fd.Doc != nil {
					start = fd.Doc.Pos()
				}
				edits = append(edits, edit{offset(m, start), offset(m, fd.End()), ""})
				continue
			}
			if gd, ok := d.(*ast.GenDecl); ok && gd.Tok == token.IMPORT {
				continue
			}
			left++
		}
		if len(edits) == 0 {
			continue
		}
		removed += len(edits)
		if left == 0 {
			if err := confined(m.Dir, f.Path); err != nil {
				return removed, err
			}
			if err := os.Remove(f.Path); err != nil {
				return removed, err
			}
			continue
		}
		out, err := fixImports(apply(src, edits), nil, importNames(f))
		if err != nil {
			return removed, err
		}
		if _, err := writeIfChanged(m.Dir, f.Path, out); err != nil {
			return removed, err
		}
	}
	return removed, nil
}
