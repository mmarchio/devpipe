package wbs

// TaskAnalysis is a task analysis the ARCHITECT submitted: whether the
// analysis found optimizations that fix the recovery reason, and what
// they are.
type TaskAnalysis struct {
	Task          string
	Fixable       bool
	Findings      []string
	Optimizations []string
}
