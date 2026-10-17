package workerresult

import (
	"fmt"
	"sort"
	"strings"
)

// ParseMode controls extraction strictness (WBS 13.10).
type ParseMode string

// Parse modes.
const (
	ModeStrict   ParseMode = "STRICT"
	ModeTolerant ParseMode = "TOLERANT"
)

// RepairMode controls repair (WBS 13.33).
type RepairMode string

// Repair modes.
const (
	RepairNone              RepairMode = "NONE"
	RepairDeterministicOnly RepairMode = "DETERMINISTIC_ONLY"
	// RepairLLM is recognised but not available (PLAN Decision 1).
	RepairLLM RepairMode = "LLM_REPAIR"
)

// Limits are protocol-level size limits (WBS 13.37/13.38), independent of
// Phase 12 transport limits.
type Limits struct {
	MaxResponseBytes  int
	MaxSummaryBytes   int
	MaxItemBytes      int
	MaxCompleted      int
	MaxRemaining      int
	MaxBlocking       int
	MaxArtifacts      int
	MaxAcceptance     int
	MaxNextActionByte int
}

// Settings are the project's response-protocol settings (WBS 13.96).
type Settings struct {
	ProtocolVersion ProtocolVersion
	ParseMode       ParseMode
	RepairMode      RepairMode
	Limits          Limits
}

// DefaultLimits (PLAN Decision 10).
func DefaultLimits() Limits {
	return Limits{MaxResponseBytes: 256 << 10, MaxSummaryBytes: 4 << 10, MaxItemBytes: 2 << 10, MaxCompleted: 100, MaxRemaining: 100,
		MaxBlocking: 50, MaxArtifacts: 500, MaxAcceptance: 200, MaxNextActionByte: 4 << 10}
}

// DefaultSettings: protocol 1.0, STRICT, DETERMINISTIC_ONLY.
func DefaultSettings() Settings {
	return Settings{ProtocolVersion: SupportedProtocol, ParseMode: ModeStrict, RepairMode: RepairDeterministicOnly, Limits: DefaultLimits()}
}

// SettingsError lists invalid fields.
type SettingsError struct{ Fields map[string]string }

func (e *SettingsError) Error() string {
	keys := make([]string, 0, len(e.Fields))
	for k := range e.Fields {
		keys = append(keys, k+": "+e.Fields[k])
	}
	sort.Strings(keys)
	return "workerresult: invalid settings: " + strings.Join(keys, "; ")
}

// Validate checks the settings (WBS 13.96.2).
func (s Settings) Validate() error {
	f := map[string]string{}
	if err := s.ProtocolVersion.Compatible(); err != nil {
		f["protocol_version"] = "unsupported protocol version " + s.ProtocolVersion.String()
	}
	if s.ParseMode != ModeStrict && s.ParseMode != ModeTolerant {
		f["parse_mode"] = fmt.Sprintf("must be STRICT or TOLERANT, got %q", s.ParseMode)
	}
	switch s.RepairMode {
	case RepairNone, RepairDeterministicOnly, RepairLLM:
	default:
		f["repair_mode"] = fmt.Sprintf("must be NONE, DETERMINISTIC_ONLY or LLM_REPAIR, got %q", s.RepairMode)
	}
	l := s.Limits
	for name, v := range map[string]struct{ val, min, max int }{
		"max_response_bytes": {l.MaxResponseBytes, 256, 16 << 20}, "max_summary_bytes": {l.MaxSummaryBytes, 16, 1 << 20},
		"max_item_bytes": {l.MaxItemBytes, 16, 1 << 20}, "max_completed_items": {l.MaxCompleted, 1, 10000},
		"max_remaining_items": {l.MaxRemaining, 1, 10000}, "max_blocking_items": {l.MaxBlocking, 1, 10000},
		"max_artifact_items": {l.MaxArtifacts, 0, 100000}, "max_acceptance_items": {l.MaxAcceptance, 0, 10000},
		"max_next_action_bytes": {l.MaxNextActionByte, 16, 1 << 20},
	} {
		if v.val < v.min || v.val > v.max {
			f[name] = fmt.Sprintf("must be between %d and %d", v.min, v.max)
		}
	}
	if len(f) > 0 {
		return &SettingsError{Fields: f}
	}
	return nil
}

// ValidateReadiness is the pre-execution protocol check (WBS 13.97): every
// problem is returned together.
func ValidateReadiness(s Settings, instructionResolvable, parserAvailable bool) error {
	var p []string
	if err := s.Validate(); err != nil {
		for k, v := range err.(*SettingsError).Fields {
			p = append(p, k+": "+v)
		}
	}
	if s.RepairMode == RepairLLM {
		p = append(p, "repair_mode: LLM_REPAIR is not available")
	}
	if !instructionResolvable {
		p = append(p, "response protocol instruction not resolvable")
	}
	if !parserAvailable {
		p = append(p, "result parser not available")
	}
	if len(p) == 0 {
		return nil
	}
	sort.Strings(p)
	return &ReadinessError{Problems: p}
}

// ReadinessError lists every readiness problem.
type ReadinessError struct{ Problems []string }

func (e *ReadinessError) Error() string {
	return "workerresult: protocol not ready: " + strings.Join(e.Problems, "; ")
}
