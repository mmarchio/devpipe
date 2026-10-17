// Package bounds holds the size and encoding limits of editable text
// artifacts (Phase 18, WBS 18.89 / 18.96 / 18.165). Every service write
// path checks content here, so malformed or oversized input is rejected
// as invalid input — never a crash, never a 500.
package bounds

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Kind is an editable text artifact.
type Kind string

const (
	WBS            Kind = "WBS document"
	StateFile      Kind = "state file"
	PromptTemplate Kind = "prompt template"
	RolePrompt     Kind = "role system prompt"
	Instruction    Kind = "instruction"
)

// Default limits (decision register: WBS / state files 1 MiB; templates,
// role prompts and instructions 256 KiB).
var limits = map[Kind]int{
	WBS:            1 << 20,
	StateFile:      1 << 20,
	PromptTemplate: 256 << 10,
	RolePrompt:     256 << 10,
	Instruction:    256 << 10,
}

// Limit returns the maximum size in bytes of a kind.
func Limit(k Kind) int { return limits[k] }

var (
	// ErrTooLarge: the content exceeds its kind's limit.
	ErrTooLarge = errors.New("content too large")
	// ErrInvalidText: the content is not valid UTF-8 or contains NUL.
	ErrInvalidText = errors.New("content is not valid text")
)

// Check validates content of kind k.
func Check(k Kind, content string) error {
	if max := limits[k]; max > 0 && len(content) > max {
		return fmt.Errorf("%w: %s is %d bytes, the limit is %d", ErrTooLarge, k, len(content), max)
	}
	if !utf8.ValidString(content) {
		return fmt.Errorf("%w: %s is not valid UTF-8", ErrInvalidText, k)
	}
	if strings.IndexByte(content, 0) >= 0 {
		return fmt.Errorf("%w: %s contains a NUL byte", ErrInvalidText, k)
	}
	return nil
}
