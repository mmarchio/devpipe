package service

import (
	"reflect"
	"testing"

	"overseer/internal/prompt"
)

// P11-01 risk control (approved PLAN Decision 1): cache keys no longer hash
// the runtime-state version, so every value the composer can render must be
// covered by an explicit cache-key input. This guard enumerates the fields
// of the composition input and fails when a field appears that is not
// mapped — e.g. a future runtime-derived retry or continuation counter must
// be added to the cache input (PromptCacheService.buildCacheInput) and then
// to this map.
var compositionInputKeyCoverage = map[string]string{
	"ProjectID":               "PromptCacheInput.ProjectID + semantic snapshot hash",
	"PhaseID":                 "semantic snapshot hash (phase)",
	"UnitID":                  "semantic snapshot hash (work unit)",
	"ContextSnapshotID":       "identity only — content covered by the semantic snapshot hash",
	"TemplateID":              "TemplateHash (VersionedHash id)",
	"TemplateVersion":         "TemplateHash (VersionedHash version)",
	"TemplateSource":          "TemplateHash (VersionedHash content)",
	"RoleID":                  "RoleHash / semantic snapshot hash (role configuration)",
	"RoleVersion":             "RoleHash",
	"Role":                    "RoleHash",
	"StateMachineID":          "PromptCacheInput.StateMachineID",
	"StateMachineVersion":     "PromptCacheInput.StateMachineVersion",
	"StateKey":                "PromptCacheInput.StateKey",
	"StateFiles":              "semantic snapshot hash (file type/version/hash pins)",
	"Required":                "RoleHash (state-file requirements)",
	"Unit":                    "WorkUnitSpecHash",
	"Continuation":            "ContinuationStatusHash",
	"ProtocolID":              "ResponseProtocolHash",
	"ProtocolVersion":         "ResponseProtocolHash",
	"ProtocolContent":         "ResponseProtocolHash",
	"AllowedStatuses":         "not set by the composer's source resolution (protocol content carries the statuses)",
	"StateInstructionContent": "StateInstructionHash",
	"StateInstructionVersion": "StateInstructionHash",
	"StateInstructionHash":    "StateInstructionHash (content hash covered by content)",
	"SnapshotHash":            "always empty in composition; snapshot identity is the semantic hash",
	"PhaseWBSVersion":         "PhaseWBSHash (PHASE_PLAN)",
	"PhaseWBSContent":         "PhaseWBSHash (PHASE_PLAN)",
	"PlanningPolicy":          "PlanningConfigHash (PHASE_PLAN)",
	"PlanningGranularity":     "PlanningConfigHash (PHASE_PLAN)",
}

func TestP1101CompositionInputFullyKeyed(t *testing.T) {
	typ := reflect.TypeOf(prompt.CanonicalCompositionInput{})
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if _, ok := compositionInputKeyCoverage[name]; !ok {
			t.Errorf("composition input field %s is not covered by a prompt-cache key input; add it to the cache input (runtime-derived values must be explicit key inputs) and to compositionInputKeyCoverage", name)
		}
	}
	for name := range compositionInputKeyCoverage {
		if _, ok := typ.FieldByName(name); !ok {
			t.Errorf("stale coverage entry %s", name)
		}
	}
}
