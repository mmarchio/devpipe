package service

import (
	"testing"

	"overseer/internal/execution"
	"overseer/internal/persistence"
)

func failedRun(stdout string) *persistence.ValidationRunRecord {
	return &persistence.ValidationRunRecord{Status: string(execution.ValidationFail), Records: []execution.ValidationRecord{
		{Program: "go", Args: []string{"test", "./..."}, Outcome: execution.ValidationPass, Stdout: "ok  \tcomfy-env\t0.1s\n"},
		{Program: "go", Args: []string{"test", "./internal/..."}, Outcome: execution.ValidationFail, Stdout: stdout},
	}}
}

// The same failure with different timings, cache markers and temporary
// paths has one signature; a different failure has another.
func TestFailureSignature(t *testing.T) {
	a := failedRun("ok  \tcomfy-env/internal/config\t(cached)\n--- FAIL: Test_3_2_8 (0.00s)\n    t014_test.go:46: precondition: locked_dir must be unreadable\nFAIL\nFAIL\tcomfy-env/internal/installation\t0.003s\n")
	b := failedRun("ok  \tcomfy-env/internal/config\t0.120s\n--- FAIL: Test_3_2_8 (0.01s)\n    t014_test.go:46: precondition: locked_dir must be unreadable\nFAIL\nFAIL\tcomfy-env/internal/installation\t0.004s\n")
	c := failedRun("--- FAIL: Test_3_2_8 (0.00s)\n    t014_test.go:61: expected 4 recorded entries, got 3\nFAIL\n")
	d := failedRun("--- FAIL: TestX (0.00s)\n    x_test.go:9: open /tmp/TestX123/001/f: no such file\n")
	e := failedRun("--- FAIL: TestX (0.00s)\n    x_test.go:9: open /tmp/TestX987/002/f: no such file\n")
	if failureSignature(a) != failureSignature(b) {
		t.Error("identical failures with different timings have different signatures")
	}
	if failureSignature(a) == failureSignature(c) {
		t.Error("different failures share a signature")
	}
	if failureSignature(d) != failureSignature(e) {
		t.Error("identical failures with different temporary paths have different signatures")
	}
	if got := attemptLimitOf(reasonIdenticalFailuresPrefix + " 3 times for task 3.2.8 (validation runs as root (uid 0)): replan the task"); got != LimitIdenticalFailures {
		t.Errorf("identical-failure recovery maps to %q", got)
	}
}
