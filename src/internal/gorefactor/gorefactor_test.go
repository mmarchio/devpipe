package gorefactor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"overseer/internal/goanalysis"
	"overseer/internal/hardening"
)

var fixture = map[string]string{
	"go.mod": "module ex\n\ngo 1.25\n",
	"project/project.go": `package project

import "strings"

// normalizeName canonicalizes a project name.
func normalizeName(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ToLower(s)
	return strings.ReplaceAll(s, " ", "-")
}

// Create mentions normalizeName in a comment and a string.
func Create(name string) string { return "project:" + normalizeName(name) + "" }

var Label = "normalizeName"
`,
	"phase/phase.go": `package phase

import "strings"

func normalizeName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.ToLower(name)
	return strings.ReplaceAll(name, " ", "-")
}

func Title(n string) string { return strings.ToUpper(normalizeName(n)) }

var Cb = normalizeName
`,
	"role/role.go": `package role

import "strings"

func normalizeName(v string) string {
	v = strings.TrimSpace(v)
	v = strings.ToLower(v)
	return strings.ReplaceAll(v, " ", "-")
}

func Apply(xs []string, f func(string) string) []string {
	var out []string
	for _, x := range xs {
		out = append(out, f(x))
	}
	return out
}

func All(xs []string) []string { return Apply(xs, normalizeName) }
`,
	"project/project_test.go": `package project

import "testing"

func TestNormalize(t *testing.T) {
	if got := normalizeName(" A B "); got != "a-b" {
		t.Fatal(got)
	}
	if normalizeName("P2") != "p2" {
		t.Fatal()
	}
}

func TestProjectCreationNormalizesName(t *testing.T) {
	if Create(" My P ") != "project:my-p" {
		t.Fatal()
	}
}
`,
	"phase/phase_test.go": `package phase

import "testing"

func TestNorm(t *testing.T) {
	cases := []struct{ in, want string }{
		{in: " A B ", want: "a-b"},
		{in: "\t", want: ""},
	}
	for _, tc := range cases {
		got := normalizeName(tc.in)
		if got != tc.want {
			t.Errorf("%q", got)
		}
	}
}
`,
}

func writeModule(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for p, c := range files {
		full := filepath.Join(dir, p)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func goCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go %v: %v\n%s", args, err, out)
	}
}

func reload(t *testing.T, dir string) *goanalysis.Module {
	t.Helper()
	m, err := goanalysis.LoadModule(context.Background(), dir, goanalysis.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func read(t *testing.T, dir, rel string) string {
	b, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// P16-49 … P16-53 (16.112, 16.113, 16.117, 16.120, 16.121, 16.125–16.128,
// 16.169, 16.170): the full mechanical consolidation, step by step, each
// step compiled / tested.
func TestP1649To53Consolidation(t *testing.T) {
	dir := writeModule(t, fixture)
	m := reload(t, dir)
	fns, _ := goanalysis.Inventory(m)
	groups := hardening.GroupByFingerprint(fns)
	if len(groups) != 1 {
		t.Fatalf("groups %d", len(groups))
	}
	g := groups[0]
	dest, _ := hardening.ChooseUtilityPackage(g.Members, m.PackageInfos(), m.Path, "internal")
	var sources []string
	old := map[string]bool{}
	oldPkgs := map[string]string{}
	for _, mem := range g.Members {
		sources = append(sources, mem.QualifiedName())
		old[mem.QualifiedName()] = true
		oldPkgs[mem.ImportPath] = mem.Package
	}
	// P16-49 copy (originals still compile).
	changed, err := CopyFunction(m, g.Members[0], *dest, sources)
	if err != nil || !changed {
		t.Fatal(err)
	}
	goCmd(t, dir, "build", "./...")
	if changed, err := CopyFunction(m, g.Members[0], *dest, sources); err != nil || changed {
		t.Fatalf("copy not idempotent: %v %v", changed, err)
	}
	conflict := *dest
	if _, err := CopyFunction(reload(t, dir), g.Members[0], conflict, sources); err != nil {
		t.Fatal(err)
	}
	// P16-50 shared tests (union matrix) pass independently.
	m = reload(t, dir)
	tests := goanalysis.TestInventory(m, old)
	var cases []hardening.TestCase
	for _, ti := range tests {
		cases = append(cases, ti.Cases...)
	}
	rows := hardening.BuildTestMatrix(cases)
	if len(rows) != 3 {
		t.Fatalf("rows %d", len(rows))
	}
	if _, err := WriteSharedTests(dir, *dest, g.Members[0].Signature, rows); err != nil {
		t.Fatal(err)
	}
	goCmd(t, dir, "test", "./"+dest.Dir)
	if !strings.Contains(read(t, dir, SharedTestFile(*dest)), "ex/phase.TestNorm, ex/project.TestNormalize") {
		t.Fatal("provenance missing")
	}
	// P16-51 migrate package by package; each compiles and tests.
	for _, pkg := range hardening.CallerPackages(g) {
		m = reload(t, dir)
		n, err := MigrateReferences(m, pkg, old, oldPkgs, *dest)
		if err != nil || n == 0 {
			t.Fatalf("%s: %d %v", pkg, n, err)
		}
		goCmd(t, dir, "build", "./...")
		goCmd(t, dir, "test", "./"+strings.TrimPrefix(pkg, "ex/"))
		if n, err := MigrateReferences(reload(t, dir), pkg, old, oldPkgs, *dest); err != nil || n != 0 {
			t.Fatalf("migration not idempotent: %d %v", n, err)
		}
	}
	proj := read(t, dir, "project/project.go")
	if !strings.Contains(proj, `"normalizeName"`) || !strings.Contains(proj, "mentions normalizeName in a comment") || !strings.Contains(proj, "stringutil.NormalizeName(name)") {
		t.Fatalf("comment / string rewritten or call missed:\n%s", proj)
	}
	if !strings.Contains(read(t, dir, "phase/phase.go"), "var Cb = stringutil.NormalizeName") || !strings.Contains(read(t, dir, "role/role.go"), "Apply(xs, stringutil.NormalizeName)") {
		t.Fatal("function value / callback not migrated")
	}
	// Zero old references (production + tests) before removal.
	m = reload(t, dir)
	for k, s := range goanalysis.Scan(m, old) {
		if s.ProductionRefs != 0 || s.TestRefs != 0 || !s.Declared {
			t.Fatalf("%s: %+v", k, s)
		}
	}
	// P16-52 helper-only tests removed; caller-specific kept.
	tests = goanalysis.TestInventory(m, map[string]bool{dest.ImportPath + "." + dest.Symbol: true})
	members := map[string]bool{dest.ImportPath + "." + dest.Symbol: true}
	var helper []hardening.TestInfo
	for _, ti := range tests {
		if ti.ImportPath != dest.ImportPath && hardening.ClassifyTest(ti, members) == hardening.TestHelperOnly {
			helper = append(helper, ti)
		}
	}
	if len(helper) != 2 {
		t.Fatalf("helper tests: %+v", helper)
	}
	if n, err := RemoveTests(m, helper); err != nil || n != 2 {
		t.Fatal(n, err)
	}
	if strings.Contains(read(t, dir, "project/project_test.go"), "TestNormalize(") || !strings.Contains(read(t, dir, "project/project_test.go"), "TestProjectCreationNormalizesName") {
		t.Fatal("wrong tests removed")
	}
	if _, err := os.Stat(filepath.Join(dir, "phase/phase_test.go")); !os.IsNotExist(err) {
		t.Fatal("empty test file kept")
	}
	// P16-53 remove duplicates; only one implementation remains.
	m = reload(t, dir)
	for _, mem := range g.Members {
		if ok, err := RemoveFunction(m, mem); err != nil || !ok {
			t.Fatal(mem.QualifiedName(), err)
		}
	}
	if ok, _ := RemoveFunction(reload(t, dir), g.Members[0]); ok {
		t.Fatal("remove not idempotent")
	}
	goCmd(t, dir, "vet", "./...")
	goCmd(t, dir, "test", "./...")
	m = reload(t, dir)
	for _, s := range goanalysis.Scan(m, old) {
		if s.Declared {
			t.Fatal("old implementation remains")
		}
	}
	fns, _ = goanalysis.Inventory(m)
	if len(hardening.GroupByFingerprint(fns)) != 0 {
		t.Fatal("duplicate set not empty after consolidation")
	}
	if strings.Contains(read(t, dir, "role/role.go"), "\"strings\"") {
		t.Fatalf("unused strings import kept in role:\n%s", read(t, dir, "role/role.go"))
	}
}

// P16-49 (16.102): copying into a package the body already uses (a domain
// owner) drops the self-qualifier and the self-import.
func TestP1649CopyIntoUsedPackage(t *testing.T) {
	body := "\ttotal := money.Amount(0)\n\tfor _, a := range xs {\n\t\ttotal += a\n\t}\n\treturn money.Round(total)\n"
	dir := writeModule(t, map[string]string{
		"go.mod":         "module ex\n\ngo 1.25\n",
		"money/money.go": "package money\n\ntype Amount int64\n\nfunc Round(a Amount) Amount { return a }\n",
		"svc/svc.go":     "package svc\n\nimport \"ex/money\"\n\nfunc sum(xs []money.Amount) money.Amount {\n" + body + "}\n\nvar S = sum\n",
	})
	m := reload(t, dir)
	fns, _ := goanalysis.Inventory(m)
	var sum hardening.FunctionInfo
	for _, f := range fns {
		if f.Symbol == "sum" {
			sum = f
		}
	}
	dest := hardening.Destination{ImportPath: "ex/money", Name: "money", Dir: "money", Exists: true, Kind: hardening.DestDomain, Symbol: "Sum"}
	if _, err := CopyFunction(m, sum, dest, []string{"ex/svc.sum"}); err != nil {
		t.Fatal(err)
	}
	src := read(t, dir, "money/sum.go")
	if strings.Contains(src, "money.") || strings.Contains(src, `"ex/money"`) || !strings.Contains(src, "func Sum(xs []Amount) Amount") {
		t.Fatalf("self-qualified copy:\n%s", src)
	}
	goCmd(t, dir, "build", "./...")
}
