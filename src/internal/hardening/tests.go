package hardening

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// TestCase is one behavioural case extracted from a test: the normalized
// argument and expected-result expressions of one call to the tested
// helper (WBS 16.115). Names and table structure don't matter.
type TestCase struct {
	Inputs   []string `json:"inputs"`
	Expected []string `json:"expected"`
	// Source is the test function (import path + name) it came from.
	Source string `json:"source"`
}

// Fingerprint identifies a case by behaviour only.
func (c TestCase) Fingerprint() string {
	h := sha256.Sum256([]byte(strings.Join(c.Inputs, "\x00") + "\x01" + strings.Join(c.Expected, "\x00")))
	return hex.EncodeToString(h[:12])
}

// TestInfo is one test function in the inventory (WBS 16.114).
type TestInfo struct {
	ImportPath string `json:"import_path"`
	File       string `json:"file"`
	Name       string `json:"name"`
	// ProductionCalls are the module (non-test) functions the test calls,
	// qualified.
	ProductionCalls []string `json:"production_calls"`
	// Cases are the extracted cases of calls to group members.
	Cases []TestCase `json:"cases,omitempty"`
	// Extractable: every call to a group member yielded a case (literal
	// arguments compared against a literal expectation).
	Extractable bool `json:"extractable"`
}

// Test classes (WBS 16.118/16.125).
const (
	TestHelperOnly     = "HELPER_ONLY"
	TestCallerSpecific = "CALLER_SPECIFIC"
)

// ClassifyTest: a test whose only production calls are group members, and
// whose cases were all extracted, only tests the duplicated helper; any
// other test exercises its caller and is preserved.
func ClassifyTest(t TestInfo, members map[string]bool) string {
	if !t.Extractable || len(t.ProductionCalls) == 0 {
		return TestCallerSpecific
	}
	for _, c := range t.ProductionCalls {
		if !members[c] {
			return TestCallerSpecific
		}
	}
	return TestHelperOnly
}

// MatrixRow is one distinct behavioural case of the consolidated test.
type MatrixRow struct {
	Fingerprint string   `json:"fingerprint"`
	Inputs      []string `json:"inputs"`
	Expected    []string `json:"expected"`
	Sources     []string `json:"sources"`
}

// BuildTestMatrix is the union of distinct cases (WBS 16.116): identical
// cases from different tests collapse into one row listing every source;
// no unique case is lost. Rows keep first-seen order of the sorted input
// so output is deterministic.
func BuildTestMatrix(cases []TestCase) []MatrixRow {
	idx := map[string]int{}
	var rows []MatrixRow
	sorted := append([]TestCase(nil), cases...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Source < sorted[j].Source })
	for _, c := range sorted {
		fp := c.Fingerprint()
		if i, ok := idx[fp]; ok {
			if !contains(rows[i].Sources, c.Source) {
				rows[i].Sources = append(rows[i].Sources, c.Source)
			}
			continue
		}
		idx[fp] = len(rows)
		rows = append(rows, MatrixRow{Fingerprint: fp, Inputs: c.Inputs, Expected: c.Expected, Sources: []string{c.Source}})
	}
	return rows
}

// MatrixCovers reports whether every case appears in the matrix
// (coverage preserved semantically, WBS 16.126).
func MatrixCovers(rows []MatrixRow, cases []TestCase) bool {
	have := map[string]bool{}
	for _, r := range rows {
		have[r.Fingerprint] = true
	}
	for _, c := range cases {
		if !have[c.Fingerprint()] {
			return false
		}
	}
	return true
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
