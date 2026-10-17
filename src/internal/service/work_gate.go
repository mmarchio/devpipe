package service

import (
	"sort"
	"strings"
	"sync"
)

// Work-gate sources.
const (
	// GateDisk blocks side effects at critical disk usage (decision U7).
	GateDisk = "disk"
	// GateAuditVerification blocks new work after an unacknowledged audit
	// verification failure (WBS 18.110, decision U9).
	GateAuditVerification = "audit_verification"
)

// WorkGate is the process-wide admission control for NEW work: while any
// source blocks it, the engine sends no new worker request (the execution
// waits with the reason). Work already in flight finishes normally. A
// source may also block every repository side effect (validation runs and
// checkpoints, e.g. critical disk usage — decision U7). A nil gate never
// blocks.
type WorkGate struct {
	mu     sync.RWMutex
	blocks map[string]string
	all    map[string]bool
}

// NewWorkGate returns an open gate.
func NewWorkGate() *WorkGate { return &WorkGate{blocks: map[string]string{}, all: map[string]bool{}} }

// Block closes the gate for new worker requests (source, reason).
func (g *WorkGate) Block(source, reason string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.blocks[source] = reason
	delete(g.all, source)
}

// BlockSideEffects closes the gate for new worker requests, validation
// runs and checkpoints.
func (g *WorkGate) BlockSideEffects(source, reason string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.blocks[source] = reason
	g.all[source] = true
}

// BlockedSideEffects returns why validation / checkpoints are blocked
// ("" when they are not).
func (g *WorkGate) BlockedSideEffects() string {
	if g == nil {
		return ""
	}
	g.mu.RLock()
	var parts []string
	for src := range g.all {
		parts = append(parts, g.blocks[src])
	}
	g.mu.RUnlock()
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}

// Clear lifts source's block.
func (g *WorkGate) Clear(source string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.blocks, source)
	delete(g.all, source)
}

// Blocks returns the active blocks (source → reason).
func (g *WorkGate) Blocks() map[string]string {
	out := map[string]string{}
	if g == nil {
		return out
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	for k, v := range g.blocks {
		out[k] = v
	}
	return out
}

// Blocked returns why new work is blocked ("" when it is not).
func (g *WorkGate) Blocked() string {
	b := g.Blocks()
	if len(b) == 0 {
		return ""
	}
	keys := make([]string, 0, len(b))
	for k := range b {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = b[k]
	}
	return strings.Join(parts, "; ")
}

// gateWaitError parks a step whose side effect the work gate holds: the
// step stays INTENT and the execution WAITs until the gate opens.
type gateWaitError struct{ reason string }

func (e *gateWaitError) Error() string { return e.reason }
