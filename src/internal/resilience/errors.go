package resilience

import (
	"context"
	"errors"
	"strings"
	"sync"
	"syscall"
)

// ErrorClass is the production classification of a failure (WBS 18.188).
type ErrorClass string

const (
	// Transient failures may succeed when repeated (bounded retry).
	Transient ErrorClass = "TRANSIENT"
	// Permanent failures fail again when repeated.
	Permanent ErrorClass = "PERMANENT"
	// Recoverable failures need reconciliation, not a blind repeat.
	Recoverable ErrorClass = "RECOVERABLE"
	// Ambiguous failures may have produced an external side effect.
	Ambiguous ErrorClass = "AMBIGUOUS"
	// OperatorRequired failures need a human (disk full, permissions).
	OperatorRequired ErrorClass = "OPERATOR_REQUIRED"
	// FatalProcess failures mean the process cannot continue safely.
	FatalProcess ErrorClass = "FATAL_PROCESS"
)

// Classes lists every class in display order.
func Classes() []ErrorClass {
	return []ErrorClass{Transient, Permanent, Recoverable, Ambiguous, OperatorRequired, FatalProcess}
}

// Retryable reports whether a generic retry loop may repeat an operation
// that failed with class c: only TRANSIENT (WBS 18.190).
func Retryable(c ErrorClass) bool { return c == Transient }

type marked struct {
	err   error
	class ErrorClass
}

func (m *marked) Error() string { return m.err.Error() }
func (m *marked) Unwrap() error { return m.err }

// Mark attaches an explicit class to err; errors.Is / errors.As still see
// the original chain. A nil err stays nil.
func Mark(err error, c ErrorClass) error {
	if err == nil {
		return nil
	}
	return &marked{err: err, class: c}
}

// sqlStater is satisfied by driver errors carrying a SQLSTATE (pgconn's
// *PgError) without importing the driver.
type sqlStater interface{ SQLState() string }

var (
	sentinelMu sync.RWMutex
	sentinels  []sentinel
)

type sentinel struct {
	target error
	class  ErrorClass
}

// RegisterSentinel maps a subsystem sentinel to a class (consulted after
// explicit marks and the built-in rules). Later registrations of the same
// target replace earlier ones.
func RegisterSentinel(target error, c ErrorClass) {
	sentinelMu.Lock()
	defer sentinelMu.Unlock()
	for i := range sentinels {
		if sentinels[i].target == target {
			sentinels[i].class = c
			return
		}
	}
	sentinels = append(sentinels, sentinel{target: target, class: c})
}

// ClassOf classifies err (WBS 18.188). Order: the outermost explicit mark;
// context cancellation (PERMANENT for retry purposes — the caller decided
// to stop); context deadline (TRANSIENT); SQLSTATE rules; filesystem
// errors needing an operator; registered sentinels; RECOVERABLE.
func ClassOf(err error) ErrorClass {
	if err == nil {
		return ""
	}
	var m *marked
	if errors.As(err, &m) {
		return m.class
	}
	switch {
	case errors.Is(err, context.Canceled):
		return Permanent
	case errors.Is(err, context.DeadlineExceeded):
		return Transient
	}
	var s sqlStater
	if errors.As(err, &s) {
		if c, ok := sqlStateClass(s.SQLState()); ok {
			return c
		}
	}
	if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EROFS) || errors.Is(err, syscall.EPERM) {
		return OperatorRequired
	}
	sentinelMu.RLock()
	defer sentinelMu.RUnlock()
	for _, sn := range sentinels {
		if errors.Is(err, sn.target) {
			return sn.class
		}
	}
	return Recoverable
}

// sqlStateClass maps PostgreSQL SQLSTATEs.
func sqlStateClass(code string) (ErrorClass, bool) {
	switch {
	case code == "40001" || code == "40P01": // serialization failure, deadlock
		return Transient, true
	case strings.HasPrefix(code, "08"): // connection exception
		return Transient, true
	case code == "57P01" || code == "57P02" || code == "57P03": // admin shutdown, crash shutdown, cannot connect now
		return Transient, true
	case code == "53100": // disk full
		return OperatorRequired, true
	case strings.HasPrefix(code, "23"), strings.HasPrefix(code, "22"), strings.HasPrefix(code, "42"):
		return Permanent, true
	}
	return "", false
}

// IsAmbiguous reports whether err may hide an external side effect.
func IsAmbiguous(err error) bool { return ClassOf(err) == Ambiguous }

// diskFullText is how child processes (git, compilers, test runners)
// report ENOSPC on their own output.
const diskFullText = "no space left on device"

// IsDiskFull reports a full disk (WBS 18.54): ENOSPC, PostgreSQL's
// disk_full (53100), or a child process reporting it in its message.
func IsDiskFull(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ENOSPC) {
		return true
	}
	var s sqlStater
	if errors.As(err, &s) && s.SQLState() == "53100" {
		return true
	}
	return MentionsDiskFull(err.Error())
}

// MentionsDiskFull reports whether process output reports ENOSPC.
func MentionsDiskFull(output string) bool {
	return strings.Contains(strings.ToLower(output), diskFullText)
}
