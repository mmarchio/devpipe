package prompt

// P8-36 — WBS 8.34 prompt cache-key preparation, TDD T8.34.1-T8.34.4.
//
// CacheKeyInput is the deterministic input set Phase 10 will use for cache
// keys. Exact field set (stated per the task NOTES so TESTER/PM can
// verify):
//
//  1. project configuration version  (pinned integer)
//  2. state-machine version          (pinned integer)
//  3. state key                      (the pinned state the prompt is for)
//  4. role configuration hash        (WBS 5.16 ConfigurationHash — a role
//     edit that moves the hash moves the cache input, T8.34.2)
//  5. template version + hash        (pinned template identity, T8.34.4)
//  6. execution (context) snapshot hash (stands in for the WBS/state files
//     the snapshot pins — a state-file edit moves it, T8.34.3)
//  7. response protocol version + hash (pinned protocol identity)
//  8. continuation status hash       (empty = no continuation status
//     captured; a fresh prompt pins "")
//
// Change-detection policy: every field is a CAPTURED/pinned value — the
// serialization never reads "latest" state. The cache input is the hex
// SHA-256 of a per-field length-prefixed serialization (the same framing
// as CanonicalPromptHash), so:
//   - repeated construction over the same input is byte-identical
//     (T8.34.1);
//   - a change to ANY single field moves the cache input (T8.34.2 /
//     T8.34.3 / T8.34.4 + per-field determinism pins below).
//
// No compressed-prompt caching is implemented here (Phase 10 scope).

import "testing"

func cacheKeyFixture() CacheKeyInput {
	return CacheKeyInput{
		ProjectConfigurationVersion: 7,
		RoleConfigurationHash:       "ro" + "9a",
		TemplateVersion:             3,
		TemplateHash:                "te" + "77",
		ExecutionSnapshotHash:       "ex" + "11",
		ResponseProtocolVersion:     2,
		ResponseProtocolHash:        "rp" + "33",
		ContinuationStatusHash:      "co" + "55",
		// State-machine identity.
		StateMachineVersion: 4,
		StateKey:            "IMPLEMENT",
	}
}

// T8.34.1 — Cache-input structure deterministic. Repeated construction
// over the same pinned input set yields a byte-identical cache input, and
// the input is a 64-hex-char sha-256 digest.
func TestT8341_CacheInputDeterministic(t *testing.T) {
	first, err := CacheKey(cacheKeyFixture())
	if err != nil {
		t.Fatalf("CacheKey: %v", err)
	}
	for i := 0; i < 5; i++ {
		again, err := CacheKey(cacheKeyFixture())
		if err != nil {
			t.Fatalf("CacheKey repeat %d: %v", i, err)
		}
		if again != first {
			t.Fatalf("same pinned inputs produced different cache inputs: %q vs %q", first, again)
		}
	}
	if len(first) != 64 {
		t.Fatalf("expected a 64-hex-char sha-256 digest, got %q (len %d)", first, len(first))
	}
}

// T8.34.2 — Role edit changes cache input. A role edit that moves the
// role configuration hash (WBS 5.16) moves the cache input.
func TestT8342_RoleEditChangesCacheInput(t *testing.T) {
	base := cacheKeyFixture()
	h0, err := CacheKey(base)
	if err != nil {
		t.Fatalf("CacheKey: %v", err)
	}
	changed := base
	changed.RoleConfigurationHash = "ed" + "44"
	h1, err := CacheKey(changed)
	if err != nil {
		t.Fatalf("CacheKey: %v", err)
	}
	if h1 == h0 {
		t.Fatalf("role configuration hash change did not move the cache input: %q", h0)
	}
}

// T8.34.3 — WBS/state-file change changes cache input. WBS/state-file
// edits ride on the execution snapshot hash: the snapshot's content hash
// moves, and it is a direct cache-input field.
func TestT8343_WBSStateFileChangeChangesCacheInput(t *testing.T) {
	base := cacheKeyFixture()
	h0, err := CacheKey(base)
	if err != nil {
		t.Fatalf("CacheKey: %v", err)
	}
	changed := base
	changed.ExecutionSnapshotHash = "wf" + "66"
	h1, err := CacheKey(changed)
	if err != nil {
		t.Fatalf("CacheKey: %v", err)
	}
	if h1 == h0 {
		t.Fatalf("execution snapshot (WBS/state-file) hash change did not move the cache input: %q", h0)
	}
}

// T8.34.4 — Template edit changes cache input. A template edit bumps the
// pinned template version/hash, which are direct cache-input fields.
func TestT8344_TemplateEditChangesCacheInput(t *testing.T) {
	base := cacheKeyFixture()
	h0, err := CacheKey(base)
	if err != nil {
		t.Fatalf("CacheKey: %v", err)
	}
	changed := base
	changed.TemplateVersion = 4
	changed.TemplateHash = "tq" + "88"
	h1, err := CacheKey(changed)
	if err != nil {
		t.Fatalf("CacheKey: %v", err)
	}
	if h1 == h0 {
		t.Fatalf("template version/hash change did not move the cache input: %q", h0)
	}
}

// Determinism companion pins: every remaining cache-input field moves the
// cache input on its own, and a fresh (no-continuation) prompt pins an
// empty continuation status hash deterministically.
func TestT834X_EachFieldMovesCacheInput(t *testing.T) {
	base := cacheKeyFixture()
	h0, err := CacheKey(base)
	if err != nil {
		t.Fatalf("CacheKey: %v", err)
	}
	variants := []struct {
		name   string
		modify func(*CacheKeyInput)
	}{
		{"project configuration version", func(i *CacheKeyInput) { i.ProjectConfigurationVersion = 8 }},
		{"state-machine version", func(i *CacheKeyInput) { i.StateMachineVersion = 6 }},
		{"state key", func(i *CacheKeyInput) { i.StateKey = "VALIDATE" }},
		{"template version only", func(i *CacheKeyInput) { i.TemplateVersion = 5 }},
		{"template hash only", func(i *CacheKeyInput) { i.TemplateHash = "th" + "01" }},
		{"response protocol version", func(i *CacheKeyInput) { i.ResponseProtocolVersion = 3 }},
		{"response protocol hash", func(i *CacheKeyInput) { i.ResponseProtocolHash = "rh" + "02" }},
		{"continuation status hash", func(i *CacheKeyInput) { i.ContinuationStatusHash = "cn" + "03" }},
	}
	for _, v := range variants {
		changed := base
		v.modify(&changed)
		h1, err := CacheKey(changed)
		if err != nil {
			t.Fatalf("CacheKey (%s): %v", v.name, err)
		}
		if h1 == h0 {
			t.Fatalf("changing %s did not move the cache input: %q", v.name, h0)
		}
	}
}

// No-continuation policy pin: a fresh prompt pins an empty continuation
// status hash; that is deterministic and distinct from a non-empty one.
func TestT834Y_NoContinuationIsEmptyAndDistinct(t *testing.T) {
	base := cacheKeyFixture()
	fresh := base
	fresh.ContinuationStatusHash = ""
	h0, err := CacheKey(fresh)
	if err != nil {
		t.Fatalf("CacheKey (no continuation): %v", err)
	}
	again, err := CacheKey(fresh)
	if err != nil {
		t.Fatalf("CacheKey (no continuation) repeat: %v", err)
	}
	if again != h0 {
		t.Fatalf("no-continuation input not deterministic: %q vs %q", h0, again)
	}
	if h0 == func() string { k, _ := CacheKey(base); return k }() {
		t.Fatalf("empty and non-empty continuation status hash produced the same cache input")
	}
}
