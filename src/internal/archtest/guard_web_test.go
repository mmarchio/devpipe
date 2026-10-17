package archtest

import (
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// T17.1.1 — the web package tree builds independently.
func TestT1711WebBuildsIndependently(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Skipf("no module at %s", root)
	}
	cmd := exec.Command("go", "build", "./internal/web/...", "./internal/readmodel/...")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("web build failed: %v\n%s", err, out)
	}
}

// T17.1.2 — page handlers depend on interfaces, not concrete services.
func TestT1712HandlersCannotImportServices(t *testing.T) {
	v := CheckWebImports("web/handlers", []string{Module + "/internal/service"})
	if len(v) != 1 || v[0].Rule != "web-handlers-service" {
		t.Fatalf("expected web-handlers-service violation, got %v", v)
	}
	// The JSON API package keeps its concrete-service registration.
	if v := CheckWebImports("web", []string{Module + "/internal/service"}); len(v) != 0 {
		t.Fatalf("web API flagged: %v", v)
	}
	if v := CheckWebImports("web/handlers", []string{Module + "/internal/readmodel", "net/http"}); len(v) != 0 {
		t.Fatalf("allowed handler imports flagged: %v", v)
	}
}

// T17.1.3 — the web tree never imports PostgreSQL adapters.
func TestT1713WebCannotImportPostgres(t *testing.T) {
	for _, dir := range []string{"web", "web/handlers", "web/middleware", "web/viewmodels"} {
		for _, imp := range []string{Module + "/internal/dbrepo", Module + "/internal/database", "github.com/jackc/pgx/v5", "github.com/jackc/pgx/v5/pgxpool"} {
			if v := CheckWebImports(dir, []string{imp}); len(v) != 1 || v[0].Rule != "web-infra" {
				t.Fatalf("%s -> %s not detected: %v", dir, imp, v)
			}
		}
	}
	if v := CheckWebImports("service", []string{Module + "/internal/dbrepo"}); len(v) != 0 {
		t.Fatalf("non-web dir flagged: %v", v)
	}
}

// T17.1.4 — the web tree never imports the Hermes adapter (or any gateway).
func TestT1714WebCannotImportHermes(t *testing.T) {
	for _, imp := range []string{Module + "/internal/gateway", Module + "/internal/gateway/hermes"} {
		if v := CheckWebImports("web/middleware", []string{imp}); len(v) != 1 {
			t.Fatalf("%s not detected: %v", imp, v)
		}
	}
}

// T17.1.5 — web code cannot call runtime-state write operations.
func TestT1715WebCannotMutateRuntimeState(t *testing.T) {
	src := `package web
func f(s, l, r any) {
	s.ApplyTransition(nil, "p", nil)
	l.TransitionTx(nil, nil, nil, "", "", nil)
	r.UpdatePosition(nil)
	l.Pause(nil, "p")
}`
	f, err := parser.ParseFile(token.NewFileSet(), "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	v := CheckWebRuntimeWrites("web", f)
	if len(v) != 3 {
		t.Fatalf("expected 3 runtime-write violations, got %v", v)
	}
	if v := CheckWebRuntimeWrites("service", f); len(v) != 0 {
		t.Fatalf("non-web dir flagged: %v", v)
	}
}

// The readmodel package is a domain package (no infrastructure imports).
func TestReadModelIsDomain(t *testing.T) {
	if v := CheckImports("readmodel", []string{Module + "/internal/database"}); len(v) != 1 {
		t.Fatalf("readmodel infra import not detected: %v", v)
	}
	if v := CheckImports("readmodel", []string{"net/http"}); len(v) != 1 {
		t.Fatalf("readmodel net/http not detected: %v", v)
	}
}
