package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"overseer/internal/execution"
	"overseer/internal/hardening"
	"overseer/internal/wbs"
)

// p16HardeningRig is a p16 rig whose repository is a Go module (files)
// committed before bootstrap, with final hardening enabled and real `go`
// final validation.
func p16HardeningRig(t *testing.T, files map[string]string, tune func(*hardening.Settings)) *p16Rig {
	t.Helper()
	r := p16NewRigWith(t, p16Options{noBootstrap: true, engine: func(s *execution.EngineSettings, h *hardening.Settings) {
		*h = hardening.DefaultSettings()
		h.GroupCheckpointPolicy = string(execution.CheckpointRequired)
		h.MinStatements = 2
		if tune != nil {
			tune(h)
		}
		s.FinalValidation = []wbs.ValidationSpec{{Name: "go vet", Program: "go", Args: []string{"vet", "./..."}},
			{Name: "go test", Program: "go", Args: []string{"test", "-count=1", "./..."}}}
	}})
	for p, c := range files {
		full := filepath.Join(r.dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p15Git(t, r.dir, "add", "-A")
	p15Git(t, r.dir, "commit", "-q", "-m", "module")
	r.boot(t)
	return r
}

// runPhase plans and completes the single user phase.
func (r *p16Rig) runToHardening(t *testing.T) {
	t.Helper()
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
}

func (r *p16Rig) read(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.dir, rel))
	if err != nil {
		return ""
	}
	return string(b)
}

const p16NormalizeBody = `	s = strings.TrimSpace(s)
	s = strings.ToLower(s)
	return strings.ReplaceAll(s, " ", "-")
`

// p16DupModule: project / phase / role carry the identical normalizeName
// (16.169) with the 16.170 test shape: 6 cases shared by the project and
// phase helper tests, 2 unique project cases, 1 unique phase case, and a
// caller-specific project test.
func p16DupModule() map[string]string {
	shared := `		{in: " A ", want: "a"},
		{in: "B B", want: "b-b"},
		{in: "", want: ""},
		{in: "  ", want: ""},
		{in: "X y Z", want: "x-y-z"},
		{in: "Already-ok", want: "already-ok"},
`
	return map[string]string{
		"go.mod": "module ex\n\ngo 1.25\n",
		"project/project.go": "package project\n\nimport \"strings\"\n\nfunc normalizeName(s string) string {\n" + p16NormalizeBody + "}\n\n" +
			"// Create names a project.\nfunc Create(name string) string { return \"project:\" + normalizeName(name) }\n",
		"phase/phase.go": "package phase\n\nimport \"strings\"\n\nfunc normalizeName(s string) string {\n" + p16NormalizeBody + "}\n\n" +
			"// Title names a phase.\nfunc Title(n string) string { return \"phase:\" + normalizeName(n) }\n",
		"role/role.go": "package role\n\nimport \"strings\"\n\nfunc normalizeName(s string) string {\n" + p16NormalizeBody + "}\n\n" +
			"// Key names a role.\nfunc Key(n string) string { return \"role:\" + normalizeName(n) }\n",
		"project/project_test.go": "package project\n\nimport \"testing\"\n\nfunc TestNormalizeName(t *testing.T) {\n\tcases := []struct{ in, want string }{\n" + shared +
			"\t\t{in: \"P1 P1\", want: \"p1-p1\"},\n\t\t{in: \"\\tTab\", want: \"tab\"},\n\t}\n\tfor _, c := range cases {\n\t\tif got := normalizeName(c.in); got != c.want {\n\t\t\tt.Errorf(\"%q: %q\", c.in, got)\n\t\t}\n\t}\n}\n\n" +
			"func TestProjectCreationNormalizesName(t *testing.T) {\n\tif Create(\" My P \") != \"project:my-p\" {\n\t\tt.Fatal()\n\t}\n}\n",
		"phase/phase_test.go": "package phase\n\nimport \"testing\"\n\nfunc TestNorm(t *testing.T) {\n\ttests := []struct{ in, want string }{\n" + shared +
			"\t\t{in: \"Q\", want: \"q\"},\n\t}\n\tfor _, tc := range tests {\n\t\tgot := normalizeName(tc.in)\n\t\tif got != tc.want {\n\t\t\tt.Errorf(\"%q\", got)\n\t\t}\n\t}\n}\n",
	}
}

// TestP16169DuplicateHardening — WBS 16.169 / 16.170 / 16.87–16.149: the
// group is detected, stringutil chosen, the import graph verified, the
// shared function copied and tested, callers migrated package by package
// (each tested), zero old references confirmed, redundant tests
// consolidated, originals removed, validated and checkpointed; one
// implementation remains and every distinct case survives.
func TestP16169DuplicateHardening(t *testing.T) {
	r := p16HardeningRig(t, p16DupModule(), nil)
	r.runToHardening(t)
	baseline := r.head(t)
	r.runUntil(t, execution.StepCompleted, 400)
	// Baseline checkpoint precedes every hardening edit.
	var baseCommit string
	if err := r.h.DBScan(context.Background(), "SELECT commit_sha FROM repository_checkpoints WHERE kind = 'HARDENING_BASELINE' AND execution_id = '"+r.execID.String()+"'", &baseCommit); err != nil {
		t.Fatal(err)
	}
	if out := p15Git(t, r.dir, "show", baseCommit+":project/project.go"); !strings.Contains(out, "func normalizeName") {
		t.Fatal("baseline does not identify the pre-hardening repository")
	}
	_ = baseline
	// Operation order (16.169).
	var ops string
	if err := r.h.DBScan(context.Background(), `SELECT string_agg(o.kind || ':' || replace(o.package, 'ex/', ''), ',' ORDER BY o.seq) FROM hardening_operations o
		JOIN hardening_groups g ON g.id = o.group_id JOIN hardening_passes p ON p.id = g.pass_id WHERE p.execution_id = '`+r.execID.String()+`'`, &ops); err != nil {
		t.Fatal(err)
	}
	want := "COPY_TO_SHARED:,TEST_SHARED:,UPDATE_REFERENCES:phase,TEST_CALLERS:phase,UPDATE_REFERENCES:project,TEST_CALLERS:project," +
		"UPDATE_REFERENCES:role,TEST_CALLERS:role,CONSOLIDATE_TESTS:,REMOVE_DUPLICATES:,VALIDATE:,CHECKPOINT:"
	if ops != want {
		t.Fatalf("operations\n got %s\nwant %s", ops, want)
	}
	// One implementation remains, in stringutil.
	for _, f := range []string{"project/project.go", "phase/phase.go", "role/role.go"} {
		if src := r.read(t, f); strings.Contains(src, "func normalizeName") || !strings.Contains(src, "stringutil.NormalizeName") {
			t.Fatalf("%s not consolidated:\n%s", f, src)
		}
	}
	if src := r.read(t, "stringutil/normalize_name.go"); strings.Count(src, "func NormalizeName") != 1 {
		t.Fatalf("shared implementation:\n%s", src)
	}
	// 16.170: shared behaviour once, all distinct cases (6 + 2 + 1 = 9),
	// caller-specific test kept, helper-only tests removed.
	shared := r.read(t, "stringutil/normalize_name_shared_test.go")
	if n := strings.Count(shared, "{name: \"case-"); n != 9 {
		t.Fatalf("shared matrix rows = %d:\n%s", n, shared)
	}
	if pt := r.read(t, "project/project_test.go"); strings.Contains(pt, "TestNormalizeName(") || !strings.Contains(pt, "TestProjectCreationNormalizesName") {
		t.Fatalf("project tests:\n%s", pt)
	}
	if r.read(t, "phase/phase_test.go") != "" {
		t.Fatal("redundant phase helper test kept")
	}
	// Group record, lineage, report, checkpoint.
	var status, lineage string
	if err := r.h.DBScan(context.Background(), `SELECT g.status, g.lineage::text FROM hardening_groups g JOIN hardening_passes p ON p.id = g.pass_id
		WHERE p.execution_id = '`+r.execID.String()+`' AND g.classification = 'SAFE'`, &status, &lineage); err != nil {
		t.Fatal(err)
	}
	if status != "CONSOLIDATED" || !strings.Contains(lineage, "ex/stringutil.NormalizeName") || !strings.Contains(lineage, "ex/role.normalizeName") {
		t.Fatalf("group %s lineage %s", status, lineage)
	}
	report := r.report(t)
	for _, w := range []string{`"consolidated_groups":1`, `"functions_removed":3`, `"unique_cases_preserved":9`, `"termination":"FIXED_POINT"`} {
		if !strings.Contains(report, w) {
			t.Fatalf("report lacks %s: %s", w, report)
		}
	}
	if n := r.count(t, "SELECT count(*) FROM repository_checkpoints WHERE kind = 'HARDENING_GROUP' AND status = 'CREATED' AND execution_id = '"+r.execID.String()+"'"); n != 1 {
		t.Fatalf("group checkpoints = %d", n)
	}
	// Hardening progress is separate from WBS progress (16.145).
	if r.unitStatuses(t, 1) != "a:DONE" {
		t.Fatal(r.unitStatuses(t, 1))
	}
	// Final validation ran against the hardened repository.
	if countOf(r.events(t), "FINAL_VALIDATION_PASSED") != 1 {
		t.Fatal("final validation")
	}
}

// report returns the persisted hardening report JSON (compact).
func (r *p16Rig) report(t *testing.T) string {
	t.Helper()
	var report string
	if err := r.h.DBScan(context.Background(), `SELECT rep.report::text FROM hardening_reports rep JOIN hardening_passes p ON p.id = rep.pass_id WHERE p.execution_id = '`+r.execID.String()+`'`, &report); err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(strings.ReplaceAll(report, ": ", ":"), ", ", ",")
}
