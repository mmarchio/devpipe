package archtest

import (
	"os"
	"path/filepath"
	"testing"
)

// T1.14.1 — Domain cannot import persistence implementation.
func TestDomainCannotImportDatabase(t *testing.T) {
	v := CheckImports("project", []string{Module + "/internal/database"})
	if len(v) != 1 || v[0].Rule != "domain-infra" {
		t.Fatalf("expected 1 domain-infra violation, got %v", v)
	}
}

// T1.14.2 — Domain cannot import web layer.
func TestDomainCannotImportWeb(t *testing.T) {
	v := CheckImports("state", []string{Module + "/internal/web"})
	if len(v) != 1 || v[0].Rule != "domain-infra" {
		t.Fatalf("expected 1 domain-infra violation, got %v", v)
	}
}

// T1.14.3 — Worker abstraction doesn't expose Hermes types (gateway adapter).
func TestWorkerCannotImportGateway(t *testing.T) {
	v := CheckImports("worker", []string{Module + "/internal/gateway"})
	if len(v) != 1 || v[0].Rule != "domain-infra" {
		t.Fatalf("expected 1 domain-infra violation, got %v", v)
	}
}

// T1.14.4 — Compression abstraction doesn't expose provider-specific types.
func TestCompressionCannotImportConcreteProvider(t *testing.T) {
	v := CheckImports("compression", []string{Module + "/internal/gateway"})
	rules := map[string]bool{}
	for _, x := range v {
		rules[x.Rule] = true
	}
	if !rules["compression-provider"] {
		t.Fatalf("expected compression-provider violation, got %v", v)
	}
}

// T1.14.5 — Architecture test fails deliberately: a synthetic violating
// import set MUST be detected (proves the check is live, not vacuous).
func TestDeliberateFailureIsDetected(t *testing.T) {
	cases := []struct {
		pkg string
		imp string
	}{
		{"wbs", Module + "/internal/database"},
		{"role", Module + "/internal/web"},
		{"worker", Module + "/internal/gateway"},
		{"orchestration", "net/http"},
	}
	for _, c := range cases {
		if v := CheckImports(c.pkg, []string{c.imp}); len(v) == 0 {
			t.Errorf("deliberate failure not detected: %s -> %s", c.pkg, c.imp)
		}
	}
}

// Negative controls: allowed imports produce no violations.
func TestAllowedImportsAreClean(t *testing.T) {
	for _, pkg := range []string{"project", "phase", "wbs", "state", "role", "prompt", "compression", "worker", "execution", "orchestration", "cache", "persistence", "statefile"} {
		v := CheckImports(pkg, []string{"context", "fmt", Module + "/internal/project"})
		if len(v) != 0 {
			t.Errorf("%s: allowed imports flagged: %v", pkg, v)
		}
	}
}

// Live walk over the current module must report zero violations.
func TestCurrentModuleIsClean(t *testing.T) {
	root := filepath.Join("..", "..")
	abs, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(abs, "go.mod")); err != nil {
		t.Skipf("no module at %s: %v", abs, err)
	}
	v, err := WalkModule(abs)
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 0 {
		for _, x := range v {
			t.Error(x)
		}
	}
}

// T9.1.3 — provider-specific types don't leak into the compression domain:
// the concrete provider adapter package, HTTP, and third-party SDKs are
// rejected; the stdlib, uuid and the prompt domain are allowed.
func TestCompressionDomainImportAllowlist(t *testing.T) {
	bad := []string{
		Module + "/internal/compressionprovider",
		"net/http",
		"github.com/sashabaranov/go-openai",
		Module + "/internal/dbrepo",
	}
	for _, imp := range bad {
		if len(CheckImports("compression", []string{imp})) == 0 {
			t.Fatalf("compression importing %s not detected", imp)
		}
	}
	good := []string{"context", "crypto/sha256", "github.com/google/uuid", Module + "/internal/prompt", Module + "/internal/compression"}
	if v := CheckImports("compression", good); len(v) != 0 {
		t.Fatalf("allowed imports flagged: %v", v)
	}
	for _, pkg := range []string{"prompt", "orchestration", "worker"} {
		if len(CheckImports(pkg, []string{Module + "/internal/compressionprovider"})) == 0 {
			t.Fatalf("%s importing the concrete compression provider not detected", pkg)
		}
	}
}

// P12-41 (WBS 12.90, T12.1.2): Hermes-specific packages never leak into
// domain, orchestration, execution, service or web code.
func TestP1241HermesAdapterBoundary(t *testing.T) {
	for _, dir := range []string{"worker", "orchestration", "execution", "service", "web", "state", "prompt", "project", "worker/workertest"} {
		if v := CheckAdapterImports(dir, []string{Module + "/internal/gateway/hermes"}); len(v) != 1 || v[0].Rule != "worker-adapter-boundary" {
			t.Fatalf("%s may import the Hermes adapter: %v", dir, v)
		}
	}
	if v := CheckAdapterImports("service", []string{Module + "/internal/gateway/hermes/hermestest"}); len(v) != 1 {
		t.Fatal("subpackage not covered")
	}
	for _, dir := range []string{"app", "gateway/hermes", "gateway/hermes/hermestest"} {
		if v := CheckAdapterImports(dir, []string{Module + "/internal/gateway/hermes"}); len(v) != 0 {
			t.Fatalf("%s must be allowed: %v", dir, v)
		}
	}
	if v := CheckAdapterImports("service", []string{Module + "/internal/worker"}); len(v) != 0 {
		t.Fatal("generic worker contract must be allowed")
	}
}
