// Package gorefactor applies the mechanical source edits of duplicate
// consolidation (WBS 16.112–16.128): copy a function into its shared
// destination, write the consolidated table-driven test, migrate type-
// resolved references package by package, and remove duplicate
// implementations and helper-only tests. Every edit is located through
// go/ast + go/types (internal/goanalysis) — never by text search — and
// every touched file is re-formatted with go/format. Operations are
// idempotent: rerunning one whose effect is present changes nothing.
package gorefactor

import (
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"overseer/internal/goanalysis"
	"overseer/internal/statefile"
)

// ErrConflict reports a destination symbol that exists with different
// content.
var ErrConflict = errors.New("gorefactor: conflicting declaration")

// ErrNotFound reports a declaration that could not be located.
var ErrNotFound = errors.New("gorefactor: declaration not found")

type edit struct {
	start, end int
	text       string
}

// apply replaces non-overlapping byte ranges.
func apply(src []byte, edits []edit) []byte {
	sort.Slice(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
	out := append([]byte(nil), src...)
	for _, e := range edits {
		out = append(out[:e.start], append([]byte(e.text), out[e.end:]...)...)
	}
	return out
}

// importSpec is one import (Name "" = default package name).
type importSpec struct{ Name, Path string }

// fixImports rewrites the import block of src: adds `add`, removes every
// candidate (path → package name) whose name is no longer referenced as a
// package selector, then gofmt-formats the file.
func fixImports(src []byte, add []importSpec, candidates map[string]string) ([]byte, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("gorefactor: parse: %w", err)
	}
	used := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok && id.Obj == nil {
				used[id.Name] = true
			}
		}
		return true
	})
	var specs []importSpec
	have := map[string]bool{}
	var first, last token.Pos
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.IMPORT {
			continue
		}
		if !first.IsValid() {
			first = gd.Pos()
		}
		last = gd.End()
		for _, s := range gd.Specs {
			is := s.(*ast.ImportSpec)
			p, _ := strconv.Unquote(is.Path.Value)
			name := ""
			if is.Name != nil {
				name = is.Name.Name
			}
			if cand, ok := candidates[p]; ok && name != "_" && name != "." {
				eff := name
				if eff == "" {
					eff = cand
				}
				if !used[eff] {
					continue
				}
			}
			if !have[p] {
				have[p] = true
				specs = append(specs, importSpec{name, p})
			}
		}
	}
	for _, a := range add {
		if !have[a.Path] {
			have[a.Path] = true
			specs = append(specs, a)
		}
	}
	sort.SliceStable(specs, func(i, j int) bool { return specs[i].Path < specs[j].Path })
	var block strings.Builder
	if len(specs) > 0 {
		block.WriteString("import (\n")
		for _, s := range specs {
			if s.Name != "" {
				block.WriteString("\t" + s.Name + " ")
			} else {
				block.WriteString("\t")
			}
			block.WriteString(strconv.Quote(s.Path) + "\n")
		}
		block.WriteString(")\n")
	}
	var out []byte
	if first.IsValid() {
		out = apply(src, []edit{{fset.Position(first).Offset, fset.Position(last).Offset, strings.TrimSuffix(block.String(), "\n")}})
	} else {
		at := fset.Position(f.Name.End()).Offset
		out = apply(src, []edit{{at, at, "\n\n" + strings.TrimSuffix(block.String(), "\n")}})
	}
	formatted, err := format.Source(out)
	if err != nil {
		return nil, fmt.Errorf("gorefactor: format: %w", err)
	}
	return formatted, nil
}

// importNames maps each import path of a file to the package name it
// binds, from type information.
func importNames(f *goanalysis.File) map[string]string {
	out := map[string]string{}
	for _, is := range f.AST.Imports {
		p, _ := strconv.Unquote(is.Path.Value)
		if is.Name != nil {
			out[p] = is.Name.Name
			continue
		}
		if f.Info != nil {
			if pn, ok := f.Info.Implicits[is].(*types.PkgName); ok {
				out[p] = pn.Imported().Name()
				continue
			}
		}
		out[p] = path.Base(p)
	}
	return out
}

func writeIfChanged(root, file string, data []byte) (bool, error) {
	if err := confined(root, file); err != nil {
		return false, err
	}
	old, err := os.ReadFile(file)
	if err == nil && string(old) == string(data) {
		return false, nil
	}
	if err := os.WriteFile(file, data, 0o644); err != nil {
		return false, err
	}
	return true, nil
}

func offset(m *goanalysis.Module, p token.Pos) int { return m.Fset.Position(p).Offset }

// findFunc locates a top-level function declaration by name (no receiver).
func findFunc(f *goanalysis.File, name string) *ast.FuncDecl {
	for _, d := range f.AST.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == name {
			return fd
		}
	}
	return nil
}

func fileByRel(m *goanalysis.Module, rel string) *goanalysis.File {
	for _, p := range m.Packages {
		for _, f := range p.Files {
			if f.Rel == rel {
				return f
			}
		}
	}
	return nil
}

// snake converts an identifier to a file-name stem (NormalizeName →
// normalize_name).
func snake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r + ('a' - 'A'))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// confined rejects a path outside the module root (WBS 18.85 / 18.86):
// package paths come from analysing repository content a worker can edit,
// so every write / create / delete is checked — lexically and after
// resolving symlinks — before it happens.
func confined(root, target string) error {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return fmt.Errorf("gorefactor: %s is outside the module %s: %w", target, root, statefile.ErrUnsafePath)
	}
	if _, err := statefile.SafeRelPath(root, rel); err != nil {
		return fmt.Errorf("gorefactor: %s is outside the module %s: %w", target, root, err)
	}
	return nil
}
