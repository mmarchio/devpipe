package archtest

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// T18.195 — event, audit and timeline reads are ordered by database
// sequence, never by wall-clock timestamps.
func TestT18195SequenceOrdering(t *testing.T) {
	root, err := moduleRoot()
	if err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(root, "internal", "dbrepo", "*.go"))
	stmt := regexp.MustCompile("(?s)FROM (execution_events|audit_events)\\b[^`]*?ORDER BY ([a-z_. ]+)")
	n := 0
	for _, f := range files {
		if filepath.Ext(f) != ".go" || regexp.MustCompile(`_test\.go$`).MatchString(f) {
			continue
		}
		src, _ := os.ReadFile(f)
		for _, m := range stmt.FindAllStringSubmatch(string(src), -1) {
			n++
			if regexp.MustCompile(`created_at`).MatchString(m[2]) {
				t.Errorf("%s: %s ordered by wall clock: ORDER BY %s", filepath.Base(f), m[1], m[2])
			}
		}
	}
	if n == 0 {
		t.Fatal("no event queries found (pattern broken)")
	}
}
