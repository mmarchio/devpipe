package project

import "testing"

// T3.6.1–T3.6.8 — allowed transitions accepted.
func TestLifecycleAllowedTransitions(t *testing.T) {
	for _, tc := range [][2]ProjectStatus{
		{StatusDRAFT, StatusREADY},       // T3.6.1
		{StatusREADY, StatusRUNNING},     // T3.6.2
		{StatusRUNNING, StatusPAUSED},    // T3.6.3
		{StatusPAUSED, StatusRUNNING},    // T3.6.4
		{StatusRUNNING, StatusCOMPLETED}, // T3.6.5
		{StatusRUNNING, StatusFAILED},    // T3.6.6
		{StatusRUNNING, StatusCANCELLED}, // T3.6.7
		{StatusPAUSED, StatusCANCELLED},  // T3.6.8
	} {
		if !CanTransitionTo(tc[0], tc[1]) {
			t.Errorf("%s -> %s must be allowed", tc[0], tc[1])
		}
	}
}

// T3.6.9 — DRAFT → RUNNING rejected.
func TestLifecycleDraftToRunning(t *testing.T) {
	if CanTransitionTo(StatusDRAFT, StatusRUNNING) {
		t.Fatal("DRAFT -> RUNNING must be rejected")
	}
}

// T3.6.10 — COMPLETED → RUNNING rejected (terminal).
func TestLifecycleCompletedTerminal(t *testing.T) {
	for _, to := range []ProjectStatus{StatusRUNNING, StatusDRAFT, StatusREADY, StatusPAUSED, StatusCOMPLETED, StatusFAILED, StatusCANCELLED} {
		if CanTransitionTo(StatusCOMPLETED, to) {
			t.Errorf("COMPLETED -> %s must be rejected (terminal)", to)
		}
	}
}

// T3.6.11 — CANCELLED → RUNNING rejected (terminal).
func TestLifecycleCancelledTerminal(t *testing.T) {
	for _, to := range []ProjectStatus{StatusRUNNING, StatusDRAFT, StatusREADY, StatusPAUSED, StatusCOMPLETED, StatusFAILED, StatusCANCELLED} {
		if CanTransitionTo(StatusCANCELLED, to) {
			t.Errorf("CANCELLED -> %s must be rejected (terminal)", to)
		}
	}
}

// FAILED is terminal too (WBS 3.6: terminal set is COMPLETED/FAILED/CANCELLED).
func TestLifecycleFailedTerminal(t *testing.T) {
	for _, to := range []ProjectStatus{StatusRUNNING, StatusDRAFT, StatusREADY, StatusPAUSED, StatusCOMPLETED, StatusFAILED, StatusCANCELLED} {
		if CanTransitionTo(StatusFAILED, to) {
			t.Errorf("FAILED -> %s must be rejected (terminal)", to)
		}
	}
}

// Exhaustive matrix: every (from, to) pair not in the allowed set is rejected.
func TestLifecycleFullMatrix(t *testing.T) {
	all := []ProjectStatus{StatusDRAFT, StatusREADY, StatusRUNNING, StatusPAUSED, StatusCOMPLETED, StatusFAILED, StatusCANCELLED}
	allowed := [][2]ProjectStatus{
		{StatusDRAFT, StatusREADY}, {StatusREADY, StatusRUNNING},
		{StatusRUNNING, StatusPAUSED}, {StatusRUNNING, StatusCOMPLETED},
		{StatusRUNNING, StatusFAILED}, {StatusRUNNING, StatusCANCELLED},
		{StatusPAUSED, StatusRUNNING}, {StatusPAUSED, StatusCANCELLED},
	}
	ok := func(f, t ProjectStatus) bool {
		for _, a := range allowed {
			if a[0] == f && a[1] == t {
				return true
			}
		}
		return false
	}
	for _, from := range all {
		for _, to := range all {
			if CanTransitionTo(from, to) != ok(from, to) {
				t.Errorf("matrix: %s -> %s = %v, want %v", from, to, CanTransitionTo(from, to), ok(from, to))
			}
		}
	}
}
