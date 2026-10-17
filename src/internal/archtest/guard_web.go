package archtest

import (
	"go/ast"
	"strings"
)

// webForbiddenImports are prohibited anywhere under internal/web (WBS 17.1,
// T17.1.3 / T17.1.4): the web layer reaches PostgreSQL and workers only
// through application services.
var webForbiddenImports = []string{
	Module + "/internal/dbrepo",
	Module + "/internal/database",
	Module + "/internal/gateway",
	"github.com/jackc",
}

// handlersForbiddenImports are additionally prohibited in
// internal/web/handlers (T17.1.2): page handlers depend on consumer-side
// interfaces, never on concrete application services.
var handlersForbiddenImports = []string{
	Module + "/internal/service",
}

// runtimeWriteMethods are runtime / workflow state write operations the web
// layer must never call directly (T17.1.5). Without type information the
// check is by method name; the names are specific to state mutation.
var runtimeWriteMethods = map[string]bool{
	// RuntimeStateService / TransitionApplicationService.
	"ApplyTransition": true,
	"Initialize":      true,
	// LifecycleService generic transitions (named Start / Pause / Resume /
	// Cancel operations remain the allowed API).
	"Transition":        true,
	"TransitionTx":      true,
	"TransitionGuarded": true,
	// Repository writes on execution / work-unit / phase / runtime state.
	"UpdatePosition":         true,
	"MarkStatus":             true,
	"MarkDoneWithValidation": true,
	"ClaimUnit":              true,
	"MarkCompleted":          true,
	"InsertRuntimeState":     true,
	"UpdateRuntimeState":     true,
}

// isWebDir reports whether dir (relative to internal/, slash separated) is
// the web package or one of its subpackages.
func isWebDir(dir string) bool {
	return dir == "web" || strings.HasPrefix(dir, "web/")
}

// CheckWebImports applies the Phase 17 web import boundary to one non-test
// file. dir is the package directory relative to internal/.
func CheckWebImports(dir string, imports []string) []Violation {
	if !isWebDir(dir) {
		return nil
	}
	var out []Violation
	match := func(imp string, list []string, rule string) {
		for _, bad := range list {
			if imp == bad || strings.HasPrefix(imp, bad+"/") {
				out = append(out, Violation{Package: dir, Import: imp, Rule: rule})
				return
			}
		}
	}
	for _, imp := range imports {
		match(imp, webForbiddenImports, "web-infra")
		if dir == "web/handlers" || strings.HasPrefix(dir, "web/handlers/") {
			match(imp, handlersForbiddenImports, "web-handlers-service")
		}
	}
	return out
}

// CheckWebRuntimeWrites reports every call in a web file whose selector
// names a runtime-state write method (T17.1.5).
func CheckWebRuntimeWrites(dir string, f *ast.File) []Violation {
	if !isWebDir(dir) {
		return nil
	}
	var out []Violation
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if runtimeWriteMethods[sel.Sel.Name] {
			out = append(out, Violation{Package: dir, Import: sel.Sel.Name, Rule: "web-runtime-write"})
		}
		return true
	})
	return out
}
