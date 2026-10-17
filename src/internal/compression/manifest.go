package compression

import (
	"regexp"
	"sort"
	"strings"

	"overseer/internal/prompt"
)

// ItemKind classifies one individually identifiable preserved item (WBS
// 9.18-9.22).
type ItemKind string

const (
	ItemProjectID    ItemKind = "PROJECT_ID"
	ItemPhaseID      ItemKind = "PHASE_ID"
	ItemWorkUnitID   ItemKind = "WORK_UNIT_ID"
	ItemWorkUnitName ItemKind = "WORK_UNIT_NAME"
	ItemDependencyID ItemKind = "DEPENDENCY_ID"
	ItemStateKey     ItemKind = "STATE_KEY"
	ItemRoleKey      ItemKind = "ROLE_KEY"
	ItemAcceptance   ItemKind = "ACCEPTANCE"
	ItemTest         ItemKind = "TEST"
	ItemConstraint   ItemKind = "CONSTRAINT"
	ItemSchemaField  ItemKind = "SCHEMA_FIELD"
	ItemStatusValue  ItemKind = "STATUS_VALUE"
	// ItemContextBoundary is an UNTRUSTED CONTEXT block delimiter or the
	// authority statement (WBS 18.72): compression must keep it verbatim.
	ItemContextBoundary ItemKind = "CONTEXT_BOUNDARY"
)

var knownItemKinds = map[ItemKind]bool{
	ItemProjectID: true, ItemPhaseID: true, ItemWorkUnitID: true, ItemWorkUnitName: true,
	ItemDependencyID: true, ItemStateKey: true, ItemRoleKey: true, ItemAcceptance: true,
	ItemTest: true, ItemConstraint: true, ItemSchemaField: true, ItemStatusValue: true, ItemContextBoundary: true,
}

// Known reports whether k is a valid item kind.
func (k ItemKind) Known() bool { return knownItemKinds[k] }

// ManifestItem is one preserved item with its overseer-assigned stable ID
// (AC-n / TEST-n / CONSTRAINT-n by captured index; fixed IDs for single
// identifiers). IDs are never parsed from model output, so a model cannot
// rename or invent one (T9.18.4, T9.20.4).
type ManifestItem struct {
	Ordinal int
	Kind    ItemKind
	ID      string
	Text    string
}

// Manifest is the ordered preservation manifest of one canonical prompt.
type Manifest []ManifestItem

// ByKind returns the items of one kind, in order.
func (m Manifest) ByKind(k ItemKind) Manifest {
	var out Manifest
	for _, it := range m {
		if it.Kind == k {
			out = append(out, it)
		}
	}
	return out
}

// ManifestInput carries the captured structured values the canonical
// composition used (never re-parsed from rendered text).
type ManifestInput struct {
	ProjectID    string
	PhaseID      string
	WorkUnitID   string
	WorkUnitName string
	Dependencies []string
	Acceptance   []string
	Tests        []string
	Constraints  []string
	StateKey     string
	RoleKey      string
}

var (
	schemaFieldRE  = regexp.MustCompile(`"([A-Za-z_][A-Za-z0-9_]*)"\s*:`)
	schemaStatusRE = regexp.MustCompile(`"status"\s*:\s*"([^"]+)"`)
)

// BuildManifest builds the deterministic preservation manifest (PLAN P9-11):
// identifiers, acceptance criteria (AC-n), required tests (TEST-n),
// constraints (CONSTRAINT-n), and the response-schema fields + allowed status
// values extracted from the RESPONSE_PROTOCOL components. An item is
// included only when its text occurs in the canonical components (an
// identifier the prompt never included cannot be required, WBS 9.20 "where
// included"). Pure and deterministic.
func BuildManifest(in ManifestInput, comps []CompressionComponent) Manifest {
	var all strings.Builder
	for _, c := range comps {
		all.WriteString(c.Content)
		all.WriteByte('\n')
	}
	corpus := all.String()
	var m Manifest
	seen := map[string]bool{}
	add := func(kind ItemKind, id, text string) {
		if strings.TrimSpace(text) == "" || seen[id] || !strings.Contains(corpus, text) {
			return
		}
		seen[id] = true
		m = append(m, ManifestItem{Ordinal: len(m), Kind: kind, ID: id, Text: text})
	}
	add(ItemProjectID, "PROJECT", in.ProjectID)
	add(ItemPhaseID, "PHASE", in.PhaseID)
	add(ItemWorkUnitID, "WORK_UNIT", in.WorkUnitID)
	add(ItemWorkUnitName, "WORK_UNIT_NAME", in.WorkUnitName)
	for i, d := range in.Dependencies {
		add(ItemDependencyID, "DEP-"+itoa(i+1), d)
	}
	add(ItemStateKey, "STATE", in.StateKey)
	add(ItemRoleKey, "ROLE", in.RoleKey)
	for i, a := range in.Acceptance {
		add(ItemAcceptance, "AC-"+itoa(i+1), a)
	}
	for i, t := range in.Tests {
		add(ItemTest, "TEST-"+itoa(i+1), t)
	}
	for i, c := range in.Constraints {
		add(ItemConstraint, "CONSTRAINT-"+itoa(i+1), c)
	}
	for _, c := range comps {
		if c.Type != prompt.ComponentResponseProtocol {
			continue
		}
		for _, mt := range schemaFieldRE.FindAllStringSubmatch(c.Content, -1) {
			id := "FIELD-" + mt[1]
			if !seen[id] {
				add(ItemSchemaField, id, `"`+mt[1]+`"`)
			}
		}
		for _, mt := range schemaStatusRE.FindAllStringSubmatch(c.Content, -1) {
			for _, v := range strings.Split(mt[1], "|") {
				v = strings.TrimSpace(v)
				if v == "" || v == "..." {
					continue
				}
				id := "STATUS-" + v
				if !seen[id] {
					add(ItemStatusValue, id, v)
				}
			}
		}
	}
	// Trust boundaries (WBS 18.72): every untrusted block's delimiters and
	// the authority statement survive compression verbatim.
	format, _ := prompt.NewDeterministicFormat(prompt.DefaultContextDelimVersion)
	labels := prompt.UntrustedLabels()
	sort.Strings(labels)
	for ci, c := range comps {
		for _, l := range labels {
			if open := format.OpenTag(l) + "\n"; strings.HasPrefix(c.Content, open) {
				add(ItemContextBoundary, "BOUNDARY-"+itoa(ci+1)+"-OPEN", strings.TrimSuffix(open, "\n"))
				add(ItemContextBoundary, "BOUNDARY-"+itoa(ci+1)+"-CLOSE", format.CloseTag(l))
			}
		}
	}
	add(ItemContextBoundary, "AUTHORITY", prompt.AuthorityStatement)
	return m
}
