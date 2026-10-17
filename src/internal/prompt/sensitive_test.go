package prompt

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// WBS 8.38 — Sensitive Data Exclusion (P8-37).
//
// Chosen policy (stated for TESTER/PM):
//
//  1. Default-deny is two-layered:
//     (a) CLASSIFICATION — a variable field whose name carries a secret
//         marker (password, passwd, secret, api_key, token, credential,
//         dsn, connection_string) is a SENSITIVE setting: Registering it as
//         prompt-safe is rejected and its value never enters a template.
//     (b) REGISTRATION — every other setting is prompt-UNSAFE until an
//         explicit RegisterPromptSafe: absence of registration is
//         unavailability (no lookup fallback).
//
//  2. Prompt preview (PreviewText) inlines ONLY prompt-safe registered
//     values; every other variable (unregistered, unsafe-registered,
//     sensitive) stays as its {{path}} placeholder, so secrets cannot leak
//     through the preview surface. CheckVariables keeps its P8-07 contract
//     (the full registered set): a non-prompt-safe variable is not a
//     validation failure, it is simply not inlined.
//
//  3. Prompt provenance (CleanProvenance) drops every entry whose content
//     equals a value registered as sensitive, so a secret that reached a
//     component by any path is stripped from the provenance surface.

// t838Registry returns a registry seeded with one prompt-safe setting and
// one sensitive setting (registered through the unsafe Register path, which
// succeeds but grants no template access).
func t838Registry(t *testing.T) *Registry {
	t.Helper()
	reg := NewRegistry()
	if err := reg.RegisterPromptSafe("project.docs_url", "https://docs.example/overseer"); err != nil {
		t.Fatalf("RegisterPromptSafe: %v", err)
	}
	if err := reg.Register("project.database_password", "p@ssw0rd-db"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := reg.Register("project.api_key", "sk-live-000"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return reg
}

// T8.38.1 — DB password unavailable to template.
func TestT8381DBPasswordUnavailableToTemplate(t *testing.T) {
	reg := t838Registry(t)

	// Classification half: the field name carries a secret marker.
	if !IsSecretField("database_password") {
		t.Fatalf("field database_password must be classified sensitive")
	}
	// Registration half: prompt-safe registration is rejected.
	if err := reg.RegisterPromptSafe("project.database_password", "x"); err == nil {
		t.Fatalf("RegisterPromptSafe on a sensitive field: expected rejection")
	}
	// Surface half: the unsafe-registered value is NOT prompt-safe, so it is
	// unavailable to any template through the registry's prompt-safe set.
	if reg.PromptSafe("project.database_password") {
		t.Fatalf("sensitive setting must not be prompt-safe")
	}
	if !reg.ContainsSensitive("p@ssw0rd-db") {
		t.Fatalf("registry must track the sensitive value for surface filtering")
	}

	// End-to-end through the parser wiring: the preview keeps the
	// placeholder for the sensitive path instead of inlining the value.
	ir, err := ParseTemplate("db: {{project.database_password}}")
	if err != nil {
		t.Fatalf("ParseTemplate: %v", err)
	}
	got := PreviewText(ir, reg)
	if strings.Contains(got, "p@ssw0rd-db") {
		t.Fatalf("preview exposes DB password: %q", got)
	}
	if got != "db: {{project.database_password}}" {
		t.Fatalf("preview = %q, want the placeholder retained", got)
	}
}

// T8.38.2 — Provider secret unavailable.
func TestT8382ProviderSecretUnavailable(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register("project.provider_secret", "prov-cred-42"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	for _, f := range []string{"provider_secret", "api_key", "auth_token", "session_token", "provider_credential", "license_secret"} {
		if !IsSecretField(f) {
			t.Fatalf("field %q must be classified sensitive", f)
		}
		if err := reg.RegisterPromptSafe("project."+f, "x"); err == nil {
			t.Fatalf("RegisterPromptSafe(project.%s): expected rejection", f)
		}
	}
	if reg.PromptSafe("project.provider_secret") {
		t.Fatalf("provider secret must not be prompt-safe")
	}
	ir, err := ParseTemplate("call with {{project.provider_secret}}")
	if err != nil {
		t.Fatalf("ParseTemplate: %v", err)
	}
	got := PreviewText(ir, reg)
	if strings.Contains(got, "prov-cred-42") {
		t.Fatalf("preview exposes provider secret: %q", got)
	}
	if got != "call with {{project.provider_secret}}" {
		t.Fatalf("preview = %q, want the placeholder retained", got)
	}
}

// T8.38.3 — Registered nonsecret setting available.
func TestT8383RegisteredNonsecretSettingAvailable(t *testing.T) {
	reg := t838Registry(t)
	if !reg.PromptSafe("project.docs_url") {
		t.Fatalf("prompt-safe registered setting must be prompt-safe")
	}
	ir, err := ParseTemplate("docs: {{project.docs_url}}")
	if err != nil {
		t.Fatalf("ParseTemplate: %v", err)
	}
	got := PreviewText(ir, reg)
	if got != "docs: https://docs.example/overseer" {
		t.Fatalf("preview = %q, want the registered nonsecret inlined", got)
	}
	// A non-secret field that was never registered is still unavailable.
	if reg.PromptSafe("project.build_number") {
		t.Fatalf("unregistered non-secret setting must not be prompt-safe")
	}
}

// T8.38.4 — Prompt preview doesn't expose secrets.
func TestT8384PreviewDoesNotExposeSecrets(t *testing.T) {
	reg := t838Registry(t)
	ir, err := ParseTemplate("a {{project.docs_url}} b {{project.database_password}} c {{project.api_key}}")
	if err != nil {
		t.Fatalf("ParseTemplate: %v", err)
	}
	got := PreviewText(ir, reg)
	if strings.Contains(got, "p@ssw0rd-db") || strings.Contains(got, "sk-live-000") {
		t.Fatalf("preview exposes a secret: %q", got)
	}
	if got != "a https://docs.example/overseer b {{project.database_password}} c {{project.api_key}}" {
		t.Fatalf("preview = %q, want only the prompt-safe value inlined", got)
	}
}

// T8.38.5 — Prompt provenance doesn't expose secrets.
func TestT8385ProvenanceDoesNotExposeSecrets(t *testing.T) {
	reg := t838Registry(t)
	comp, err := NewPromptComponent(
		ComponentTaskContext,
		SourceSystemDefault,
		0,
		"connect with p@ssw0rd-db",
		uuid.Nil,
		1,
		"h1",
	)
	if err != nil {
		t.Fatalf("NewPromptComponent: %v", err)
	}
	comp2, err := NewPromptComponent(
		ComponentPhaseContext,
		SourceSystemDefault,
		1,
		"phase two",
		uuid.Nil,
		1,
		"h2",
	)
	if err != nil {
		t.Fatalf("NewPromptComponent: %v", err)
	}
	out := CleanProvenance(reg, ProvenanceMapWithContent(Components{comp, comp2}))
	if len(out) != 1 {
		t.Fatalf("CleanProvenance kept %d entries, want 1 (the secret-bearing entry dropped)", len(out))
	}
	if out[0].Content != "phase two" {
		t.Fatalf("surviving entry content = %q, want the non-secret component", out[0].Content)
	}
}
