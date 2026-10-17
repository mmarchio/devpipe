package prompt

import (
	"reflect"
	"testing"
)

// t87ValidImplContent is a valid IMPLEMENTATION template source (WBS 8.3
// structured sections, WBS 8.7 T8.7.1): all sections required by the
// IMPLEMENTATION purpose policy are present and every variable is
// registered.
const t87ValidImplContent = "# Objective\n{{work_unit.objective}}\n\n# Instructions\n{{state.instructions}}\n\n# Acceptance Criteria\n{{work_unit.acceptance_criteria}}\n\n# Required Context\n{{state_files.content}}\n\n# Required Response\n{{response_protocol.schema}}\n"

func t87Registry(t *testing.T) *Registry {
	t.Helper()
	reg := NewRegistry()
	for _, p := range []string{
		"work_unit.objective",
		"state.instructions",
		"work_unit.acceptance_criteria",
		"state_files.content",
		"response_protocol.schema",
	} {
		if err := reg.Register(p, "x"); err != nil {
			t.Fatalf("register %q: %v", p, err)
		}
	}
	return reg
}

func t87Template(purpose PromptPurpose, enabled bool) *PromptTemplate {
	tpl, err := NewTemplate(TemplateInput{Key: "impl-task", Name: "Impl", Purpose: purpose})
	if err != nil {
		panic(err)
	}
	tpl.Enabled = enabled
	return tpl
}

// T8.7.1 — A valid implementation template passes validation.
func TestT871ValidImplementationTemplatePasses(t *testing.T) {
	req := ValidationRequest{
		Template: t87Template(PurposeImplementation, true),
		Content:  t87ValidImplContent,
		Registry: t87Registry(t),
		StateKey: "IMPLEMENT",
	}
	rep := ValidateTemplate(req)
	if !rep.Valid() {
		t.Fatalf("report = %v, want valid", rep.String())
	}
}

// T8.7.2 — An invalid (unregistered) variable fails validation with a
// VARIABLE finding naming the offending path.
func TestT872InvalidVariableFails(t *testing.T) {
	content := "# Objective\n{{work_unit.objective}}\n\n# Acceptance Criteria\n{{work_unit.acceptance_criteria}}\n\n# Required Response\n{{response_protocol.schema}}\n\n# Bogus\n{{runtime.missing}}\n"
	rep := ValidateTemplate(ValidationRequest{
		Template: t87Template(PurposeImplementation, true),
		Content:  content,
		Registry: t87Registry(t),
	})
	if rep.Valid() {
		t.Fatalf("report valid, want a VARIABLE finding")
	}
	found := false
	for _, is := range rep {
		if is.Category == CategoryVariable && is.Detail == "runtime.missing" {
			found = true
		}
	}
	if !found {
		t.Fatalf("report = %v, want VARIABLE finding for runtime.missing", rep.String())
	}
}

// T8.7.3 — Missing required section fails where applicable: an
// IMPLEMENTATION template lacking its Acceptance Criteria section reports a
// REQUIRED_SECTION finding, while a CONTROL template (no required sections
// by policy) with no sections at all stays valid.
func TestT873MissingRequiredSection(t *testing.T) {
	missing := "# Objective\n{{work_unit.objective}}\n\n# Required Response\n{{response_protocol.schema}}\n"
	rep := ValidateTemplate(ValidationRequest{
		Template: t87Template(PurposeImplementation, true),
		Content:  missing,
		Registry: t87Registry(t),
	})
	found := false
	for _, is := range rep {
		if is.Category == CategoryRequiredSection && is.Detail == "Acceptance Criteria" {
			found = true
		}
	}
	if !found {
		t.Fatalf("report = %v, want REQUIRED_SECTION finding for Acceptance Criteria", rep.String())
	}

	bare := "free form text, no sections\n"
	rep = ValidateTemplate(ValidationRequest{
		Template: t87Template(PurposeControl, true),
		Content:  bare,
		Registry: t87Registry(t),
	})
	if !rep.Valid() {
		t.Fatalf("CONTROL template report = %v, want valid (no required sections)", rep.String())
	}
}

// T8.7.4 — A disabled template cannot satisfy an active state: validating
// with a state target against Enabled=false reports a STATE finding; the
// same template validated with no state target does not, and an enabled
// template with a state target does not.
func TestT874DisabledTemplateCannotSatisfyActiveState(t *testing.T) {
	req := ValidationRequest{
		Template: t87Template(PurposeImplementation, false),
		Content:  t87ValidImplContent,
		Registry: t87Registry(t),
		StateKey: "IMPLEMENT",
	}
	rep := ValidateTemplate(req)
	if rep.Valid() {
		t.Fatalf("disabled template with state target validated, want STATE finding")
	}
	if !containsIssue(rep, CategoryState, "IMPLEMENT") {
		t.Fatalf("report = %v, want STATE finding for state IMPLEMENT", rep.String())
	}

	rep = ValidateTemplate(ValidationRequest{
		Template: t87Template(PurposeImplementation, false),
		Content:  t87ValidImplContent,
		Registry: t87Registry(t),
	})
	if !rep.Valid() {
		t.Fatalf("disabled template without state target: %v, want valid", rep.String())
	}

	tplOn := t87Template(PurposeImplementation, true)
	rep = ValidateTemplate(ValidationRequest{
		Template: tplOn,
		Content:  t87ValidImplContent,
		Registry: t87Registry(t),
		StateKey: "IMPLEMENT",
	})
	if !rep.Valid() {
		t.Fatalf("enabled template with state target: %v, want valid", rep.String())
	}
}

// T8.7.5 — Multiple validation failures are returned in one report,
// deterministically ordered: unknown variable + missing section together,
// and a syntax error + (unparseable) unknown variable do not mask each
// other's category; two runs over the same input are byte-identical.
func TestT875MultipleFailuresReturned(t *testing.T) {
	// Unknown variable AND a missing required section at once.
	content := "# Objective\n{{work_unit.objective}}\n\n# Bogus\n{{wbs.missing}}\n"
	req := ValidationRequest{
		Template: t87Template(PurposeImplementation, true),
		Content:  content,
		Registry: t87Registry(t),
	}
	rep1 := ValidateTemplate(req)
	rep2 := ValidateTemplate(req)
	if rep1.Valid() {
		t.Fatalf("report valid, want multiple findings")
	}
	counts := map[ValidationCategory]int{}
	for _, is := range rep1 {
		counts[is.Category]++
	}
	if counts[CategoryVariable] != 1 || counts[CategoryRequiredSection] != 2 {
		t.Fatalf("findings = %v, want 1 VARIABLE + 2 REQUIRED_SECTION", rep1.String())
	}
	if !reflect.DeepEqual(rep1, rep2) {
		t.Fatalf("reports differ: %v vs %v", rep1, rep2)
	}
	if rep1.String() != rep2.String() {
		t.Fatalf("renderings differ: %q vs %q", rep1.String(), rep2.String())
	}

	// A syntax error is reported under SYNTAX; variable checks are skipped
	// for an unparseable source, so exactly one SYNTAX finding comes back.
	rep := ValidateTemplate(ValidationRequest{
		Template: t87Template(PurposeImplementation, true),
		Content:  "{{ project.name }} trailing {{state.instructions",
		Registry: t87Registry(t),
	})
	if len(rep) == 0 {
		t.Fatalf("syntax report valid, want a SYNTAX finding")
	}
	got := false
	for _, is := range rep {
		if is.Category == CategorySyntax {
			got = true
		}
	}
	if !got {
		t.Fatalf("report = %v, want a SYNTAX finding", rep.String())
	}
}

func containsIssue(rep ValidationReport, cat ValidationCategory, detail string) bool {
	for _, is := range rep {
		if is.Category == cat && is.Detail == detail {
			return true
		}
	}
	return false
}
