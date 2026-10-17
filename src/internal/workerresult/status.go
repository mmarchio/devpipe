package workerresult

import "errors"

// WorkerResultStatus is the worker-reported outcome (WBS 13.1).
type WorkerResultStatus string

// Worker-reported statuses.
const (
	StatusComplete   WorkerResultStatus = "complete"
	StatusIncomplete WorkerResultStatus = "incomplete"
	StatusBlocked    WorkerResultStatus = "blocked"
	StatusFailed     WorkerResultStatus = "failed"
)

// Status parse errors.
var (
	ErrStatusEmpty   = errors.New("workerresult: status is empty")
	ErrStatusUnknown = errors.New("workerresult: unknown status")
)

// ParseStatus accepts exactly one of the four lowercase statuses.
func ParseStatus(s string) (WorkerResultStatus, error) {
	if s == "" {
		return "", ErrStatusEmpty
	}
	st := WorkerResultStatus(s)
	if !st.Valid() {
		return "", ErrStatusUnknown
	}
	return st, nil
}

// Valid reports whether s is one of the four statuses.
func (s WorkerResultStatus) Valid() bool {
	switch s {
	case StatusComplete, StatusIncomplete, StatusBlocked, StatusFailed:
		return true
	}
	return false
}

// IsComplete reports a worker-reported COMPLETE (never "accepted").
func (r WorkerResult) IsComplete() bool { return r.Status == StatusComplete }

// IsIncomplete reports a worker-reported INCOMPLETE.
func (r WorkerResult) IsIncomplete() bool { return r.Status == StatusIncomplete }

// IsBlocked reports a worker-reported BLOCKED.
func (r WorkerResult) IsBlocked() bool { return r.Status == StatusBlocked }

// IsFailed reports a worker-reported FAILED (never a parse failure).
func (r WorkerResult) IsFailed() bool { return r.Status == StatusFailed }
