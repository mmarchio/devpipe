package service

import "testing"

// Each attempt-limit recovery reason maps to its limit (resolving it
// resets that counter); other recoveries reset nothing.
func TestAttemptLimitOf(t *testing.T) {
	for reason, want := range map[string]string{
		recoveryPlanExhausted:       LimitPlanAttempts,
		reasonFixExhausted:          LimitFixAttempts,
		reasonHardeningFixExhausted: LimitHardeningFixes,
		"the test command did not complete after 3 attempts: worker: TIMEOUT":                                        LimitCommandResends,
		"statefiles/ACTIVE_TASK.md: wbs: malformed task file: line 8 (still wrong after 3 resends with the problem)": LimitStatefileResends,
		"repeated worker protocol violations (3): repair attempts exhausted":                                         LimitProtocolRepairs,
		"continuation limit reached: the task was interrupted again":                                                 LimitContinuations,
		"gitcheckpoint: not a git repository":                                                                        "",
		"no current plan":                                                                                            "",
	} {
		if got := attemptLimitOf(reason); got != want {
			t.Errorf("attemptLimitOf(%q) = %q, want %q", reason, got, want)
		}
	}
}
