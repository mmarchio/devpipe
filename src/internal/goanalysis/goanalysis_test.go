package goanalysis

import (
	"context"
	"strings"
	"testing"

	"overseer/internal/hardening"
)

func load(t *testing.T, files map[string]string) *Module {
	t.Helper()
	files2 := map[string]string{}
	for k, v := range files {
		if strings.HasPrefix(k, "vendor/") {
			continue
		}
		files2[k] = v
	}
	m, err := LoadModule(context.Background(), writeModule(t, files2), Options{})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func byName(fns []hardening.FunctionInfo, q string) *hardening.FunctionInfo {
	for i := range fns {
		if fns[i].QualifiedName() == q {
			return &fns[i]
		}
	}
	return nil
}

// P16-43 (16.103): packages + import graph match go list.
func TestP1643LoadModule(t *testing.T) {
	m := load(t, map[string]string{
		"go.mod":      "module ex\n\ngo 1.25\n",
		"a/a.go":      "package a\n\nimport \"ex/b\"\n\nfunc A() int { return b.B() }\n",
		"b/b.go":      "package b\n\nimport \"ex/c\"\n\nfunc B() int { return c.C() }\n",
		"c/c.go":      "package c\n\nfunc C() int { return 1 }\n",
		"c/x_test.go": "package c_test\n\nimport (\"testing\"; \"ex/c\")\n\nfunc TestC(t *testing.T) { _ = c.C() }\n",
	})
	if m.Path != "ex" || len(m.Packages) != 3 {
		t.Fatalf("%s %d", m.Path, len(m.Packages))
	}
	g := m.ImportGraph()
	if strings.Join(g.Imports("ex/a"), ",") != "ex/b" || strings.Join(g.Imports("ex/b"), ",") != "ex/c" || len(g.Imports("ex/c")) != 0 || g.FindCycle() != nil {
		t.Fatalf("graph a:%v b:%v", g.Imports("ex/a"), g.Imports("ex/b"))
	}
	if c := g.Prospective("ex/shared", []string{"ex/a"}, []string{"ex/c"}).FindCycle(); c == nil {
		t.Fatal("transitive cycle via loaded graph not found")
	}
	if _, err := LoadModule(context.Background(), writeModule(t, map[string]string{"go.mod": "module bad\n\ngo 1.25\n", "x/x.go": "package x\n\nfunc F() int { return \"s\" }\n"}), Options{}); err == nil {
		t.Fatal("type error not reported")
	}
}

// P16-44 / P16-45 / P16-46 (16.92–16.95, 16.98, 16.99, 16.172).
func TestP1644InventoryFingerprintEffects(t *testing.T) {
	m := load(t, fixture)
	fns, err := Inventory(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fns {
		if f.ImportPath == "ex/gen" {
			t.Fatal("generated file inventoried")
		}
	}
	pn, ph, ro := byName(fns, "ex/project.normalizeName"), byName(fns, "ex/phase.normalizeName"), byName(fns, "ex/role.normalizeName")
	cl, key, cnt := byName(fns, "ex/phase.clean"), byName(fns, "ex/project.Store.Key"), byName(fns, "ex/project.count")
	if pn == nil || ph == nil || ro == nil || cl == nil || key == nil || cnt == nil {
		t.Fatalf("inventory incomplete: %d", len(fns))
	}
	if pn.File != "project/project.go" || pn.Statements != 3 || pn.Exported || pn.Signature.String() != "(string) (string)" ||
		strings.Join(pn.Imports, ",") != "strings" || !strings.Contains(strings.Join(pn.Calls, ","), "strings.ToLower") {
		t.Fatalf("project.normalizeName: %+v", pn)
	}
	// Renamed locals / formatting / comments normalize away (16.94).
	if pn.Fingerprint != ph.Fingerprint || pn.Fingerprint != ro.Fingerprint || pn.BodyHash == ph.BodyHash {
		t.Fatal("equivalent implementations do not match")
	}
	// Behaviourally different constant → different fingerprint (16.95/16.172).
	if cl.Fingerprint == pn.Fingerprint {
		t.Fatal("similar-but-different matched")
	}
	if !key.ReceiverUsed || key.Receiver != "Store" {
		t.Fatalf("receiver: %+v", key)
	}
	if strings.Join(cnt.Effects.GlobalsWritten, ",") != "ex/project.hits" || len(cnt.LocalDeps) == 0 {
		t.Fatalf("effects: %+v", cnt.Effects)
	}
	groups := hardening.GroupByFingerprint(fns)
	if len(groups) != 1 || len(groups[0].Members) != 3 {
		t.Fatalf("groups: %+v", groups)
	}
	if hardening.CheckSignatureCompatibility(groups[0].Members) != "" || hardening.CheckSideEffects(groups[0].Members) != "" || hardening.CheckLocalDependencies(groups[0].Members) != "" {
		t.Fatal("group should be safe")
	}
}

// P16-47 (16.93, 16.119, 16.124, 16.128).
func TestP1647References(t *testing.T) {
	m := load(t, fixture)
	keys := map[string]bool{"ex/project.normalizeName": true, "ex/phase.normalizeName": true, "ex/role.normalizeName": true}
	refs := References(m, keys)
	kinds := map[string]string{}
	for k, rs := range refs {
		for _, r := range rs {
			kinds[k+"@"+r.File] += r.Kind + ","
		}
	}
	// project: Create call + 2 test calls; comment and string not refs.
	if len(refs["ex/project.normalizeName"]) != 3 || kinds["ex/project.normalizeName@project/project.go"] != "CALL," {
		t.Fatalf("project refs: %+v", refs["ex/project.normalizeName"])
	}
	// phase: call + function value (Cb) + 2 test calls.
	if !strings.Contains(kinds["ex/phase.normalizeName@phase/phase.go"], "VALUE") {
		t.Fatalf("function value not found: %v", kinds)
	}
	// role: callback passed to Apply.
	if kinds["ex/role.normalizeName@role/role.go"] != "VALUE," {
		t.Fatalf("callback: %v", kinds)
	}
	sc := Scan(m, keys)
	if s := sc["ex/project.normalizeName"]; !s.Declared || s.ProductionRefs != 1 || s.TestRefs != 2 {
		t.Fatalf("scan: %+v", s)
	}
	if s := Scan(m, map[string]bool{"ex/project.gone": true})["ex/project.gone"]; s.Declared || s.ProductionRefs != 0 {
		t.Fatal("absent symbol")
	}
}

// P16-48 (16.114, 16.115).
func TestP1648TestInventory(t *testing.T) {
	m := load(t, fixture)
	members := map[string]bool{"ex/project.normalizeName": true, "ex/phase.normalizeName": true, "ex/role.normalizeName": true}
	tests := TestInventory(m, members)
	got := map[string]hardening.TestInfo{}
	for _, ti := range tests {
		got[ti.ImportPath+"."+ti.Name] = ti
	}
	if len(tests) != 3 {
		t.Fatalf("tests: %v", tests)
	}
	pt := got["ex/project.TestNormalize"]
	if !pt.Extractable || len(pt.Cases) != 2 || pt.Cases[0].Inputs[0] != `" A B "` || pt.Cases[0].Expected[0] != `"a-b"` {
		t.Fatalf("direct cases: %+v", pt)
	}
	ph := got["ex/phase.TestNorm"]
	if !ph.Extractable || len(ph.Cases) != 2 || ph.Cases[1].Inputs[0] != `"\t"` {
		t.Fatalf("table cases: %+v", ph)
	}
	if op := got["ex/phase.TestOpaque"]; op.Extractable {
		t.Fatal("opaque call extracted")
	}
	if _, ok := got["ex/project.TestProjectCreationNormalizesName"]; ok {
		t.Fatal("caller test calls no member directly")
	}
	rows := hardening.BuildTestMatrix(append(pt.Cases, ph.Cases...))
	if len(rows) != 3 || len(rows[0].Sources) != 2 {
		t.Fatalf("matrix: %+v", rows)
	}
	if hardening.ClassifyTest(pt, members) != hardening.TestHelperOnly {
		t.Fatal("helper-only")
	}
}
