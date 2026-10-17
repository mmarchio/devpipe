package execution

import (
	"errors"
	"fmt"
)

// BootstrapErrorCode is a stable machine-readable bootstrap failure code
// (WBS 15.84).
type BootstrapErrorCode string

// Bootstrap error codes (WBS 15.84).
const (
	CodeProjectNotReady       BootstrapErrorCode = "PROJECT_NOT_READY"
	CodeConfigInvalid         BootstrapErrorCode = "CONFIG_INVALID"
	CodeWorkdirInvalid        BootstrapErrorCode = "WORKDIR_INVALID"
	CodeRepositoryInvalid     BootstrapErrorCode = "REPOSITORY_INVALID"
	CodeNoPhase               BootstrapErrorCode = "NO_PHASE"
	CodeWBSInvalid            BootstrapErrorCode = "WBS_INVALID"
	CodeStateMachineInvalid   BootstrapErrorCode = "STATE_MACHINE_INVALID"
	CodeRoleInvalid           BootstrapErrorCode = "ROLE_INVALID"
	CodeStateFileInvalid      BootstrapErrorCode = "STATE_FILE_INVALID"
	CodePromptInvalid         BootstrapErrorCode = "PROMPT_INVALID"
	CodeCompressionFailed     BootstrapErrorCode = "COMPRESSION_FAILED"
	CodeConcurrentBootstrap   BootstrapErrorCode = "CONCURRENT_BOOTSTRAP"
	CodePersistenceFailed     BootstrapErrorCode = "PERSISTENCE_FAILED"
	CodeMaterializationFailed BootstrapErrorCode = "MATERIALIZATION_FAILED"
)

var allCodes = []BootstrapErrorCode{
	CodeProjectNotReady, CodeConfigInvalid, CodeWorkdirInvalid, CodeRepositoryInvalid,
	CodeNoPhase, CodeWBSInvalid, CodeStateMachineInvalid, CodeRoleInvalid,
	CodeStateFileInvalid, CodePromptInvalid, CodeCompressionFailed, CodeConcurrentBootstrap,
	CodePersistenceFailed, CodeMaterializationFailed,
}

// AllCodes returns every code in declaration order.
func AllCodes() []BootstrapErrorCode { return append([]BootstrapErrorCode(nil), allCodes...) }

// Valid reports a known code.
func (c BootstrapErrorCode) Valid() bool {
	for _, v := range allCodes {
		if v == c {
			return true
		}
	}
	return false
}

// ParseCode accepts exactly one canonical code.
func ParseCode(s string) (BootstrapErrorCode, error) {
	c := BootstrapErrorCode(s)
	if !c.Valid() {
		return "", fmt.Errorf("execution: invalid bootstrap error code %q", s)
	}
	return c, nil
}

// FailureClass classifies a bootstrap failure (WBS 15.85).
type FailureClass string

// Failure classes.
const (
	ClassConfiguration  FailureClass = "CONFIGURATION"
	ClassRecoverable    FailureClass = "RECOVERABLE"
	ClassInfrastructure FailureClass = "INFRASTRUCTURE"
)

// Valid reports a known class.
func (c FailureClass) Valid() bool {
	return c == ClassConfiguration || c == ClassRecoverable || c == ClassInfrastructure
}

// ParseClass accepts exactly one canonical class.
func ParseClass(s string) (FailureClass, error) {
	c := FailureClass(s)
	if !c.Valid() {
		return "", fmt.Errorf("execution: invalid failure class %q", s)
	}
	return c, nil
}

// DefaultClass maps a code to its class (PLAN Decision 9): materialization,
// compression and concurrency failures are recoverable; persistence is
// infrastructure; everything else is a configuration failure.
func DefaultClass(c BootstrapErrorCode) FailureClass {
	switch c {
	case CodeMaterializationFailed, CodeCompressionFailed, CodeConcurrentBootstrap:
		return ClassRecoverable
	case CodePersistenceFailed:
		return ClassInfrastructure
	}
	return ClassConfiguration
}

// Retryable reports whether an existing execution survives this class
// (stays INITIALIZING for a resume) rather than becoming FAILED.
func (c FailureClass) Retryable() bool { return c == ClassRecoverable || c == ClassInfrastructure }

// BootstrapError is a typed bootstrap failure.
type BootstrapError struct {
	Code     BootstrapErrorCode
	Class    FailureClass
	Stage    BootstrapStage
	Message  string
	Resource string
	Cause    error
}

// NewError builds an error with the code's default class.
func NewError(code BootstrapErrorCode, stage BootstrapStage, msg string, cause error) *BootstrapError {
	return &BootstrapError{Code: code, Class: DefaultClass(code), Stage: stage, Message: msg, Cause: cause}
}

func (e *BootstrapError) Error() string {
	s := fmt.Sprintf("bootstrap %s", e.Code)
	if e.Stage != "" {
		s += " at " + string(e.Stage)
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	if e.Cause != nil {
		s += ": " + e.Cause.Error()
	}
	return s
}

// Unwrap exposes the cause.
func (e *BootstrapError) Unwrap() error { return e.Cause }

// AsBootstrapError extracts a *BootstrapError.
func AsBootstrapError(err error) (*BootstrapError, bool) {
	var be *BootstrapError
	if errors.As(err, &be) {
		return be, true
	}
	return nil, false
}
