package prompt

import (
	"errors"
	"testing"
)

// T8.5.1 — Registered namespaces available.
func TestT851RegisteredNamespacesAvailable(t *testing.T) {
	want := []VariableNamespace{
		NamespaceProject,
		NamespacePhase,
		NamespaceWBS,
		NamespaceWorkUnit,
		NamespaceRuntime,
		NamespaceState,
		NamespaceRole,
		NamespaceStateFiles,
		NamespaceContinuation,
		NamespaceResponseProtocol,
	}
	for _, n := range want {
		if !KnownNamespace(n) {
			t.Fatalf("namespace %q must be known", n)
		}
	}
	if len(AllNamespaces) != len(want) {
		t.Fatalf("AllNamespaces has %d entries, want %d", len(AllNamespaces), len(want))
	}
	for _, bad := range []VariableNamespace{"", "internal", "settings", "project_extra", "response-protocol"} {
		if KnownNamespace(bad) {
			t.Fatalf("namespace %q must be unknown", bad)
		}
	}
}

// T8.5.2 — A registered variable path resolves to its value, across every
// WBS 8.5 namespace, using the WBS 8.5 example fields.
func TestT852RegisteredVariableResolves(t *testing.T) {
	r := NewRegistry()
	cases := map[string]string{
		"project.name":                  "Overseer",
		"phase.name":                    "Phase 1",
		"phase.order":                   "1",
		"wbs.version":                   "3",
		"work_unit.id":                  "u-1",
		"work_unit.name":                "init",
		"work_unit.objective":           "bootstrap",
		"work_unit.acceptance_criteria": "gate green",
		"work_unit.required_tests":      "go test ./...",
		"work_unit.constraints":         "pure domain",
		"state.key":                     "IMPLEMENT",
		"state.instructions":            "do the work",
		"role.key":                      "ENGINEER",
		"runtime.state":                 "IMPLEMENT",
		"runtime.version":               "7",
	}
	for p, v := range cases {
		if err := r.Register(p, v); err != nil {
			t.Fatalf("register %q: %v", p, err)
		}
		got, err := r.Resolve(p)
		if err != nil {
			t.Fatalf("resolve %q: %v", p, err)
		}
		if got != v {
			t.Fatalf("resolve %q = %q, want %q", p, got, v)
		}
	}
	if r.Len() != len(cases) {
		t.Fatalf("registry len = %d, want %d", r.Len(), len(cases))
	}
}

// T8.5.3 — Unknown / unregistered variable rejected.
func TestT853UnknownVariableRejected(t *testing.T) {
	r := NewRegistry()
	if err := r.Register("project.name", "Overseer"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := r.Resolve("project.name"); err != nil {
		t.Fatalf("registered resolve: %v", err)
	}
	for _, p := range []string{
		"project.name2",
		"phase.name",
		"project.",
		".name",
		"project",
		"",
		"internal.Field",
		"settings.password",
	} {
		_, err := r.Resolve(p)
		if err == nil {
			t.Fatalf("unregistered path %q: expected rejection, got none", p)
		}
		if !errors.Is(err, ErrUnknownVariable) {
			t.Fatalf("path %q: err = %v, want ErrUnknownVariable", p, err)
		}
	}
}

// T8.5.4 — Internal Go fields and sensitive settings are unavailable unless
// explicitly registered. Internal Go field names and setting paths resolve
// to nothing by default; an explicit Register makes a sensitive value
// available.
func TestT854InternalGoFieldsAndSensitiveSettingsUnavailable(t *testing.T) {
	r := NewRegistry()

	// Internal Go field names (reflection-style) are not variable paths.
	for _, p := range []string{
		"PromptTemplate.ID",
		"PromptTemplate.createdAt",
		"config.DSN",
		"db.password",
		"internal.dbPassword",
	} {
		if err := r.Register(p, "x"); err == nil {
			t.Fatalf("register internal field %q: expected rejection", p)
		}
		if _, err := r.Resolve(p); err == nil {
			t.Fatalf("internal field %q resolved without registration", p)
		}
	}

	// A sensitive setting is unavailable until explicitly registered.
	sensitive := "project.database_password"
	if err := r.Register(sensitive, "secret"); err != nil {
		t.Fatalf("register sensitive: %v", err)
	}
	got, err := r.Resolve(sensitive)
	if err != nil || got != "secret" {
		t.Fatalf("registered sensitive resolve = %q, %v; want secret", got, err)
	}
	fresh := NewRegistry()
	if _, err := fresh.Resolve(sensitive); err == nil {
		t.Fatalf("sensitive setting resolved without explicit registration")
	}
}
