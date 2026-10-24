package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// databaseAuthorityFiles hold the statefile workflow's decision paths
// (Phase 19): the MCP service, the engine's commands and checks, and the
// outbox. The workflow state lives in the database only.
var databaseAuthorityFiles = []string{
	"internal/service/mcp_service.go",
	"internal/service/engine_statefiles.go",
	"internal/service/engine_statefiles_db.go",
	"internal/service/engine_plan.go",
	"internal/service/engine_replan.go",
	"internal/service/engine_requeue.go",
	"internal/service/engine_taskops.go",
	"internal/service/engine_pause_after.go",
	"internal/service/engine_repeat.go",
	"internal/service/outbox.go",
	"internal/mcp/server.go",
	"internal/mcp/tools.go",
	"internal/mcp/render.go",
}

// statefileIO are the retired statefile readers and writers (and direct
// file I/O). writeStatefile remains for the generated team files only.
var statefileIO = map[string]bool{
	"readTaskFile": true, "readPlanningDoc": true, "writeStatefile": true, "verifyStatefiles": true, "loadWBSFiles": true,
	"activeTask": true, "backlogCandidate": true, "adoptable": true, "adoptPhase": true, "clearPlanRejection": true,
	"ParseTasks": true, "ParseTaskAnalysis": true, "ResetTask": true,
	"ReadFile": true, "WriteFile": true, "Create": true, "OpenFile": true, "Remove": true,
}

// T12.14 — no workflow decision path reaches a statefile reader or writer.
func TestDatabaseAuthorityReadsNoStatefile(t *testing.T) {
	root, err := moduleRoot()
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range databaseAuthorityFiles {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var name string
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				name = fn.Name
			case *ast.SelectorExpr:
				name = fn.Sel.Name
			}
			if statefileIO[name] {
				t.Errorf("%s: %s calls %s", rel, fset.Position(call.Pos()), name)
			}
			return true
		})
	}
}
