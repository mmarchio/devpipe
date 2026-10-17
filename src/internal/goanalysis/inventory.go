package goanalysis

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"sort"
	"strconv"
	"strings"

	"overseer/internal/hardening"
)

// impurePackages are standard packages whose calls have observable effects
// (I/O, time, randomness, synchronization, process state).
var impurePackages = map[string]bool{
	"os": true, "io": true, "io/ioutil": true, "io/fs": true, "net": true, "net/http": true, "time": true,
	"math/rand": true, "math/rand/v2": true, "crypto/rand": true, "sync": true, "sync/atomic": true,
	"syscall": true, "log": true, "log/slog": true, "os/exec": true, "os/signal": true, "runtime": true,
}

// Inventory returns every top-level function and method declared in
// non-excluded production files (WBS 16.92). init, main and generated /
// vendored code are skipped.
func Inventory(m *Module) ([]hardening.FunctionInfo, error) {
	var out []hardening.FunctionInfo
	for _, p := range m.Packages {
		for _, f := range p.ProductionFiles() {
			if f.Excluded || f.Info == nil {
				continue
			}
			src, err := os.ReadFile(f.Path)
			if err != nil {
				return nil, err
			}
			for _, d := range f.AST.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Body == nil || fd.Name.Name == "init" || (fd.Name.Name == "main" && p.Name == "main") || fd.Name.Name == "_" {
					continue
				}
				out = append(out, describe(m, p, f, fd, src))
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].QualifiedName() < out[j].QualifiedName() })
	return out, nil
}

func describe(m *Module, p *Package, f *File, fd *ast.FuncDecl, src []byte) hardening.FunctionInfo {
	info := f.Info
	obj, _ := info.Defs[fd.Name].(*types.Func)
	fi := hardening.FunctionInfo{
		ImportPath: p.ImportPath, Package: p.Name, Dir: p.RelDir, File: f.Rel,
		Symbol: fd.Name.Name, Exported: fd.Name.IsExported(),
	}
	var recvObj types.Object
	if fd.Recv != nil && len(fd.Recv.List) > 0 {
		if obj != nil {
			if sig, ok := obj.Type().(*types.Signature); ok && sig.Recv() != nil {
				fi.Receiver = recvName(sig.Recv().Type())
				recvObj = sig.Recv()
			}
		}
	}
	if obj != nil {
		fi.Signature = signatureOf(obj.Type().(*types.Signature))
	}
	start, end := m.Fset.Position(fd.Body.Pos()).Offset, m.Fset.Position(fd.Body.End()).Offset
	if start >= 0 && end <= len(src) && start <= end {
		h := sha256.Sum256(src[start:end])
		fi.BodyHash = hex.EncodeToString(h[:])
	}
	fi.Statements = countStatements(fd.Body)
	facts := bodyFacts(info, fd, obj, recvObj, p.ImportPath)
	fi.Imports, fi.Calls, fi.LocalDeps, fi.Effects, fi.ReceiverUsed = facts.imports, facts.calls, facts.localDeps, facts.effects, facts.receiverUsed
	fi.Fingerprint = Fingerprint(info, fd, obj)
	return fi
}

func signatureOf(sig *types.Signature) hardening.Signature {
	s := hardening.Signature{Params: []string{}, Results: []string{}, Variadic: sig.Variadic()}
	for i := 0; i < sig.TypeParams().Len(); i++ {
		tp := sig.TypeParams().At(i)
		s.TypeParams = append(s.TypeParams, types.TypeString(tp.Constraint(), Qualifier))
	}
	for i := 0; i < sig.Params().Len(); i++ {
		s.Params = append(s.Params, types.TypeString(sig.Params().At(i).Type(), Qualifier))
	}
	for i := 0; i < sig.Results().Len(); i++ {
		s.Results = append(s.Results, types.TypeString(sig.Results().At(i).Type(), Qualifier))
	}
	return s
}

func countStatements(n ast.Node) int {
	c := 0
	ast.Inspect(n, func(x ast.Node) bool {
		switch x.(type) {
		case *ast.BlockStmt, nil:
		case ast.Stmt:
			c++
		}
		return true
	})
	return c
}

type facts struct {
	imports, calls, localDeps []string
	effects                   hardening.Effects
	receiverUsed              bool
}

func bodyFacts(info *types.Info, fd *ast.FuncDecl, self *types.Func, recv types.Object, own string) facts {
	imps, calls, local := map[string]bool{}, map[string]bool{}, map[string]bool{}
	read, written, impure := map[string]bool{}, map[string]bool{}, map[string]bool{}
	var out facts
	lhs := map[*ast.Ident]bool{}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			for _, e := range x.Lhs {
				markWrite(e, lhs)
			}
		case *ast.IncDecStmt:
			markWrite(x.X, lhs)
		case *ast.UnaryExpr:
			if x.Op == token.AND {
				markWrite(x.X, lhs)
			}
		case *ast.CallExpr:
			if obj := calleeObject(info, x.Fun); obj != nil {
				if k := ObjectKey(obj); k != "" {
					calls[k] = true
					if pkg := obj.Pkg(); pkg != nil && impurePackages[pkg.Path()] {
						impure[k] = true
					}
					if pkg := obj.Pkg(); pkg != nil && pkg.Path() == "fmt" && (strings.HasPrefix(obj.Name(), "Print") || strings.HasPrefix(obj.Name(), "Fprint")) {
						impure[k] = true
					}
				}
			}
		}
		return true
	})
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		obj := info.Uses[id]
		if obj == nil {
			return true
		}
		if recv != nil && obj == recv {
			out.receiverUsed = true
		}
		switch o := obj.(type) {
		case *types.PkgName:
			imps[o.Imported().Path()] = true
		case *types.Var:
			if o.Pkg() != nil && o.Parent() == o.Pkg().Scope() {
				k := ObjectKey(o)
				if lhs[id] {
					written[k] = true
				} else {
					read[k] = true
				}
			}
		}
		if obj.Pkg() != nil && obj.Pkg().Path() == own && obj.Parent() == obj.Pkg().Scope() {
			local[obj.Name()] = true
		}
		if self != nil && obj == types.Object(self) {
			local[obj.Name()] = true
		}
		if fn, ok := obj.(*types.Func); ok && fn.Pkg() != nil && fn.Pkg().Path() == own {
			if sig, ok := fn.Type().(*types.Signature); ok && sig.Recv() != nil {
				// a method of an own-package type (through the receiver
				// or another value) is a package-local dependency.
				local[recvName(sig.Recv().Type())+"."+fn.Name()] = true
			}
		}
		return true
	})
	out.imports, out.calls, out.localDeps = sortedSet(imps), sortedSet(calls), sortedSet(local)
	out.effects = hardening.Effects{GlobalsRead: sortedSet(read), GlobalsWritten: sortedSet(written), Impure: sortedSet(impure)}
	return out
}

func markWrite(e ast.Expr, lhs map[*ast.Ident]bool) {
	for {
		switch x := e.(type) {
		case *ast.Ident:
			lhs[x] = true
			return
		case *ast.SelectorExpr:
			if id, ok := x.X.(*ast.Ident); ok {
				// pkg.Var = … writes Var; v.field = … writes v.
				lhs[x.Sel] = true
				lhs[id] = true
				return
			}
			e = x.X
		case *ast.IndexExpr:
			e = x.X
		case *ast.StarExpr:
			e = x.X
		case *ast.ParenExpr:
			e = x.X
		default:
			return
		}
	}
}

func calleeObject(info *types.Info, fun ast.Expr) types.Object {
	switch f := fun.(type) {
	case *ast.Ident:
		return info.Uses[f]
	case *ast.SelectorExpr:
		return info.Uses[f.Sel]
	case *ast.IndexExpr:
		return calleeObject(info, f.X)
	case *ast.IndexListExpr:
		return calleeObject(info, f.X)
	case *ast.ParenExpr:
		return calleeObject(info, f.X)
	}
	return nil
}

func sortedSet(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Fingerprint is a behavioural hash of a function (WBS 16.94/16.95):
// formatting, comments, positions and the names of parameters / locals
// (α-renamed in order of declaration) are dropped; the signature, every
// expression's type, operators, constants, control flow and resolved
// callees / package-level objects are kept, so similar but different
// functions never collide. A self-reference is spelled "self".
func Fingerprint(info *types.Info, fd *ast.FuncDecl, self *types.Func) string {
	var b strings.Builder
	if self != nil {
		sig := self.Type().(*types.Signature)
		b.WriteString(signatureOf(sig).String())
		if sig.Recv() != nil {
			b.WriteString(" recv:" + types.TypeString(sig.Recv().Type(), Qualifier))
		}
	}
	locals := map[types.Object]int{}
	local := func(o types.Object) string {
		if _, ok := locals[o]; !ok {
			locals[o] = len(locals)
		}
		return fmt.Sprintf("v%d", locals[o])
	}
	// Parameters, receiver and named results are numbered first, in order.
	if fd.Recv != nil {
		for _, f := range fd.Recv.List {
			for _, n := range f.Names {
				if o := info.Defs[n]; o != nil {
					local(o)
				}
			}
		}
	}
	for _, fl := range []*ast.FieldList{fd.Type.Params, fd.Type.Results} {
		if fl == nil {
			continue
		}
		for _, f := range fl.List {
			for _, n := range f.Names {
				if o := info.Defs[n]; o != nil {
					local(o)
				}
			}
		}
	}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if n == nil {
			b.WriteByte(')')
			return true
		}
		b.WriteByte('(')
		fmt.Fprintf(&b, "%T", n)
		if e, ok := n.(ast.Expr); ok {
			if tv, ok := info.Types[e]; ok && tv.Type != nil {
				b.WriteString(":" + types.TypeString(tv.Type, Qualifier))
				if tv.Value != nil {
					b.WriteString("=" + tv.Value.ExactString())
				}
			}
		}
		switch x := n.(type) {
		case *ast.Ident:
			obj := info.Defs[x]
			if obj == nil {
				obj = info.Uses[x]
			}
			switch {
			case obj == nil:
				b.WriteString(" _")
			case self != nil && obj == types.Object(self):
				b.WriteString(" self")
			case obj.Pkg() == nil:
				b.WriteString(" builtin." + obj.Name())
			case isLocal(obj):
				b.WriteString(" " + local(obj))
			default:
				if k := ObjectKey(obj); k != "" {
					b.WriteString(" " + k)
				} else if v, ok := obj.(*types.Var); ok && v.IsField() {
					b.WriteString(" field." + obj.Name())
				} else if _, ok := obj.(*types.PkgName); ok {
					b.WriteString(" pkg")
				} else {
					b.WriteString(" obj." + obj.Name())
				}
			}
		case *ast.BasicLit:
			b.WriteString(" " + x.Kind.String() + ":" + normLit(x))
		case *ast.BinaryExpr:
			b.WriteString(" " + x.Op.String())
		case *ast.UnaryExpr:
			b.WriteString(" " + x.Op.String())
		case *ast.AssignStmt:
			b.WriteString(" " + x.Tok.String())
		case *ast.IncDecStmt:
			b.WriteString(" " + x.Tok.String())
		case *ast.BranchStmt:
			b.WriteString(" " + x.Tok.String())
		case *ast.RangeStmt:
			b.WriteString(" " + x.Tok.String())
		case *ast.ChanType:
			fmt.Fprintf(&b, " %d", x.Dir)
		}
		return true
	})
	h := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(h[:])
}

func isLocal(obj types.Object) bool {
	if obj.Pkg() == nil {
		return false
	}
	if _, ok := obj.(*types.PkgName); ok {
		return false
	}
	if v, ok := obj.(*types.Var); ok && v.IsField() {
		return false
	}
	return obj.Parent() != nil && obj.Parent() != obj.Pkg().Scope() && obj.Parent() != types.Universe
}

func normLit(x *ast.BasicLit) string {
	if x.Kind == token.STRING {
		if s, err := strconv.Unquote(x.Value); err == nil {
			return strconv.Quote(s)
		}
	}
	return x.Value
}
