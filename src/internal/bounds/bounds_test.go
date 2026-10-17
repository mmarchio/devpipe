package bounds

import (
	"errors"
	"strings"
	"testing"
)

// T18.89 — size, UTF-8 and NUL checks per artifact kind.
func TestT1889Check(t *testing.T) {
	if err := Check(WBS, "# Phase\n- unit\n"); err != nil {
		t.Fatal(err)
	}
	if err := Check(PromptTemplate, strings.Repeat("x", Limit(PromptTemplate)+1)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized: %v", err)
	}
	if err := Check(WBS, strings.Repeat("x", Limit(PromptTemplate)+1)); err != nil {
		t.Fatalf("WBS has the larger limit: %v", err)
	}
	if err := Check(StateFile, "bad \xff\xfe bytes"); !errors.Is(err, ErrInvalidText) {
		t.Fatalf("invalid UTF-8: %v", err)
	}
	if err := Check(Instruction, "nul\x00byte"); !errors.Is(err, ErrInvalidText) {
		t.Fatalf("NUL: %v", err)
	}
}
