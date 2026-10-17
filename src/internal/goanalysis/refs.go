package goanalysis

import (
	"go/ast"
	"sort"

	"overseer/internal/hardening"
)

// References finds every resolved use of the objects in keys
// (ObjectKey values) across production and test files (WBS 16.119):
// direct calls, function values and callbacks. Declarations are not
// references; text in comments or strings never resolves to an object, so
// it is never reported (WBS 16.93). Results are grouped by key, sorted by
// file and line.
func References(m *Module, keys map[string]bool) map[string][]hardening.Reference {
	out := map[string][]hardening.Reference{}
	for _, p := range m.Packages {
		for _, f := range p.Files {
			if f.Info == nil {
				continue
			}
			calls := map[*ast.Ident]bool{}
			ast.Inspect(f.AST, func(n ast.Node) bool {
				if c, ok := n.(*ast.CallExpr); ok {
					if id := calleeIdent(c.Fun); id != nil {
						calls[id] = true
					}
				}
				return true
			})
			ast.Inspect(f.AST, func(n ast.Node) bool {
				id, ok := n.(*ast.Ident)
				if !ok {
					return true
				}
				obj := f.Info.Uses[id]
				if obj == nil {
					return true
				}
				k := ObjectKey(obj)
				if !keys[k] {
					return true
				}
				kind := "VALUE"
				if calls[id] {
					kind = "CALL"
				}
				out[k] = append(out[k], hardening.Reference{
					ImportPath: p.ImportPath, File: f.Rel, Line: m.Fset.Position(id.Pos()).Line, Test: f.Test, Kind: kind,
				})
				return true
			})
		}
	}
	for k := range out {
		rs := out[k]
		sort.Slice(rs, func(i, j int) bool {
			if rs[i].File != rs[j].File {
				return rs[i].File < rs[j].File
			}
			return rs[i].Line < rs[j].Line
		})
	}
	return out
}

func calleeIdent(fun ast.Expr) *ast.Ident {
	switch f := fun.(type) {
	case *ast.Ident:
		return f
	case *ast.SelectorExpr:
		return f.Sel
	case *ast.IndexExpr:
		return calleeIdent(f.X)
	case *ast.IndexListExpr:
		return calleeIdent(f.X)
	case *ast.ParenExpr:
		return calleeIdent(f.X)
	}
	return nil
}

// SymbolScan reports, per key, whether the object is still declared and
// how many production / test references remain (WBS 16.124/16.128).
type SymbolScan struct {
	Declared        bool
	ProductionRefs  int
	TestRefs        int
	ReferencingPkgs []string
}

// Scan inspects the module for each key.
func Scan(m *Module, keys map[string]bool) map[string]SymbolScan {
	out := map[string]SymbolScan{}
	for k := range keys {
		out[k] = SymbolScan{}
	}
	for _, p := range m.Packages {
		for _, f := range p.Files {
			if f.Info == nil || f.Test {
				continue
			}
			for _, obj := range f.Info.Defs {
				if obj == nil {
					continue
				}
				if k := ObjectKey(obj); keys[k] {
					s := out[k]
					s.Declared = true
					out[k] = s
				}
			}
		}
	}
	for k, refs := range References(m, keys) {
		s := out[k]
		pk := map[string]bool{}
		for _, r := range refs {
			if r.Test {
				s.TestRefs++
			} else {
				s.ProductionRefs++
			}
			pk[r.ImportPath] = true
		}
		s.ReferencingPkgs = sortedSet(pk)
		out[k] = s
	}
	return out
}
