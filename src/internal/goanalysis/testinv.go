package goanalysis

import (
	"bytes"
	"go/ast"
	"go/printer"
	"go/token"
	"go/types"
	"sort"
	"strconv"
	"strings"

	"overseer/internal/hardening"
)

// TestInventory lists the test functions (TestXxx(t *testing.T)) that call
// any of the member objects (WBS 16.114), with the production functions
// they call and the behavioural cases extracted from their member calls
// (WBS 16.115). Supported shapes: a member call compared (== / !=, or
// reflect.DeepEqual) against an expectation, directly or through
// `got := f(…)`, with literal arguments or fields of a range variable over
// a table literal (one case per row). A member call in any other shape
// makes the test non-extractable (it is then always preserved).
func TestInventory(m *Module, members map[string]bool) []hardening.TestInfo {
	prod := map[string]bool{}
	for _, p := range m.Packages {
		for _, f := range p.ProductionFiles() {
			if f.Info == nil {
				continue
			}
			for _, obj := range f.Info.Defs {
				if fn, ok := obj.(*types.Func); ok {
					if k := ObjectKey(fn); k != "" {
						prod[k] = true
					}
				}
			}
		}
	}
	var out []hardening.TestInfo
	for _, p := range m.Packages {
		for _, f := range p.Files {
			if !f.Test || f.Info == nil {
				continue
			}
			for _, d := range f.AST.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Recv != nil || fd.Body == nil || !strings.HasPrefix(fd.Name.Name, "Test") {
					continue
				}
				ti, calls := inspectTest(m, p, f, fd, members, prod)
				if calls > 0 {
					out = append(out, ti)
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ImportPath != out[j].ImportPath {
			return out[i].ImportPath < out[j].ImportPath
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func inspectTest(m *Module, p *Package, f *File, fd *ast.FuncDecl, members, prod map[string]bool) (hardening.TestInfo, int) {
	ti := hardening.TestInfo{ImportPath: p.ImportPath, File: f.Rel, Name: fd.Name.Name, Extractable: true}
	source := p.ImportPath + "." + fd.Name.Name
	pc := map[string]bool{}
	memberCalls := 0
	ex := &extractor{m: m, info: f.Info, members: members, tables: tableBindings(f.Info, fd.Body)}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		obj := calleeObject(f.Info, c.Fun)
		k := ObjectKey(obj)
		if k == "" || !prod[k] {
			return true
		}
		pc[k] = true
		if members[k] {
			memberCalls++
		}
		return true
	})
	ti.ProductionCalls = sortedSet(pc)
	if memberCalls == 0 {
		return ti, 0
	}
	cases, ok := ex.extract(fd.Body)
	ti.Extractable = ok
	for i := range cases {
		cases[i].Source = source
	}
	ti.Cases = cases
	return ti, memberCalls
}

type extractor struct {
	m       *Module
	info    *types.Info
	members map[string]bool
	// tables maps a range value variable to its table rows (field →
	// literal source per row).
	tables map[types.Object][]map[string]string
	// gotBindings maps `got` variables to the member call assigned to
	// them.
	gotBindings map[types.Object]*ast.CallExpr
}

// tableBindings finds `for _, tt := range <table>` where the table is a
// slice composite literal of struct literals with keyed fields (inline or
// through a local variable), and records each row's field expressions.
func tableBindings(info *types.Info, body *ast.BlockStmt) map[types.Object][]map[string]string {
	vars := map[types.Object]*ast.CompositeLit{}
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != len(as.Rhs) {
			return true
		}
		for i, l := range as.Lhs {
			if id, ok := l.(*ast.Ident); ok {
				if cl, ok := as.Rhs[i].(*ast.CompositeLit); ok {
					if o := info.Defs[id]; o != nil {
						vars[o] = cl
					} else if o := info.Uses[id]; o != nil {
						vars[o] = cl
					}
				}
			}
		}
		return true
	})
	out := map[types.Object][]map[string]string{}
	ast.Inspect(body, func(n ast.Node) bool {
		rs, ok := n.(*ast.RangeStmt)
		if !ok || rs.Value == nil {
			return true
		}
		vid, ok := rs.Value.(*ast.Ident)
		if !ok {
			return true
		}
		var lit *ast.CompositeLit
		switch x := rs.X.(type) {
		case *ast.CompositeLit:
			lit = x
		case *ast.Ident:
			lit = vars[info.Uses[x]]
		}
		if lit == nil {
			return true
		}
		var rows []map[string]string
		for _, el := range lit.Elts {
			row, ok := el.(*ast.CompositeLit)
			if !ok {
				return true
			}
			fields := map[string]string{}
			for _, fe := range row.Elts {
				kv, ok := fe.(*ast.KeyValueExpr)
				if !ok {
					return true
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok {
					return true
				}
				fields[key.Name] = exprString(kv.Value)
			}
			rows = append(rows, fields)
		}
		if o := info.Defs[vid]; o != nil {
			out[o] = rows
		}
		return true
	})
	return out
}

// extract walks the test body and returns every case; ok=false when a
// member call could not be turned into cases.
func (e *extractor) extract(body *ast.BlockStmt) ([]hardening.TestCase, bool) {
	var cases []hardening.TestCase
	ok := true
	handled := map[*ast.CallExpr]bool{}
	// got := member(args) … got != want
	gots := map[types.Object]*ast.CallExpr{}
	ast.Inspect(body, func(n ast.Node) bool {
		if as, isAs := n.(*ast.AssignStmt); isAs && len(as.Rhs) == 1 {
			if c, isCall := as.Rhs[0].(*ast.CallExpr); isCall && e.isMember(c) && len(as.Lhs) == 1 {
				if id, isID := as.Lhs[0].(*ast.Ident); isID {
					o := e.info.Defs[id]
					if o == nil {
						o = e.info.Uses[id]
					}
					if o != nil {
						gots[o] = c
					}
				}
			}
		}
		return true
	})
	e.gotBindings = gots
	ast.Inspect(body, func(n ast.Node) bool {
		var lhs, rhs ast.Expr
		switch x := n.(type) {
		case *ast.BinaryExpr:
			if x.Op != token.EQL && x.Op != token.NEQ {
				return true
			}
			lhs, rhs = x.X, x.Y
		case *ast.CallExpr:
			if sel, isSel := x.Fun.(*ast.SelectorExpr); isSel && sel.Sel.Name == "DeepEqual" && len(x.Args) == 2 {
				lhs, rhs = x.Args[0], x.Args[1]
			} else {
				return true
			}
		default:
			return true
		}
		call, want := e.callOf(lhs), rhs
		if call == nil {
			call, want = e.callOf(rhs), lhs
		}
		if call == nil {
			return true
		}
		cs, good := e.casesFor(call, want)
		if !good {
			ok = false
			return true
		}
		handled[call] = true
		cases = append(cases, cs...)
		return true
	})
	ast.Inspect(body, func(n ast.Node) bool {
		if c, isCall := n.(*ast.CallExpr); isCall && e.isMember(c) && !handled[c] {
			ok = false
		}
		return true
	})
	return cases, ok
}

func (e *extractor) isMember(c *ast.CallExpr) bool {
	return e.members[ObjectKey(calleeObject(e.info, c.Fun))]
}

// callOf resolves an expression to the member call it denotes (the call
// itself, or a variable bound to one).
func (e *extractor) callOf(x ast.Expr) *ast.CallExpr {
	switch v := x.(type) {
	case *ast.CallExpr:
		if e.isMember(v) {
			return v
		}
	case *ast.Ident:
		if o := e.info.Uses[v]; o != nil {
			return e.gotBindings[o]
		}
	case *ast.ParenExpr:
		return e.callOf(v.X)
	}
	return nil
}

// casesFor expands one call against an expectation into cases: one per
// table row when any argument / expectation reads a range variable, else
// a single case of literal expressions.
func (e *extractor) casesFor(call *ast.CallExpr, want ast.Expr) ([]hardening.TestCase, bool) {
	var table types.Object
	parts := append(append([]ast.Expr(nil), call.Args...), want)
	for _, a := range parts {
		if sel, ok := a.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok {
				if o := e.info.Uses[id]; o != nil {
					if _, ok := e.tables[o]; ok {
						if table != nil && table != o {
							return nil, false
						}
						table = o
					}
				}
			}
		}
	}
	render := func(a ast.Expr, row map[string]string) (string, bool) {
		if sel, ok := a.(*ast.SelectorExpr); ok && table != nil {
			if id, ok := sel.X.(*ast.Ident); ok && e.info.Uses[id] == table {
				v, ok := row[sel.Sel.Name]
				return v, ok
			}
		}
		if !literal(a) {
			return "", false
		}
		return exprString(a), true
	}
	rows := []map[string]string{nil}
	if table != nil {
		rows = e.tables[table]
	}
	var out []hardening.TestCase
	for _, row := range rows {
		var c hardening.TestCase
		for _, a := range call.Args {
			s, ok := render(a, row)
			if !ok {
				return nil, false
			}
			c.Inputs = append(c.Inputs, s)
		}
		w, ok := render(want, row)
		if !ok {
			return nil, false
		}
		c.Expected = []string{w}
		out = append(out, c)
	}
	return out, true
}

// literal reports a constant-like expression: basic literals, negations,
// composite literals of literals, nil / true / false.
func literal(x ast.Expr) bool {
	switch v := x.(type) {
	case *ast.BasicLit:
		return true
	case *ast.UnaryExpr:
		return literal(v.X)
	case *ast.ParenExpr:
		return literal(v.X)
	case *ast.Ident:
		return v.Name == "nil" || v.Name == "true" || v.Name == "false"
	case *ast.CompositeLit:
		for _, el := range v.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				el = kv.Value
			}
			if !literal(el) {
				return false
			}
		}
		return true
	}
	return false
}

// exprString renders an expression canonically; string literals are
// re-quoted so `x` and "x" are the same case.
func exprString(x ast.Expr) string {
	if bl, ok := x.(*ast.BasicLit); ok && bl.Kind == token.STRING {
		if s, err := strconv.Unquote(bl.Value); err == nil {
			return strconv.Quote(s)
		}
	}
	var b bytes.Buffer
	_ = printer.Fprint(&b, token.NewFileSet(), x)
	return b.String()
}
