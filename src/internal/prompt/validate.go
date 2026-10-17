package prompt

import (
	"sort"
	"strings"
)

// WBS 8.7: validate templates when created or changed rather than
// discovering problems during worker execution. The validator is a pure
// composition of the P8-04 parser and the P8-03 registry: it collects
// EVERY finding (not first-failure) into one deterministic report.

// ValidationCategory names the check that produced a finding. The fixed
// category order is the report's primary sort key (T8.7.5 determinism).
type ValidationCategory string

const (
	CategorySyntax          ValidationCategory = "SYNTAX"
	CategoryVariable        ValidationCategory = "VARIABLE"
	CategoryRequiredSection ValidationCategory = "REQUIRED_SECTION"
	CategoryPurpose         ValidationCategory = "PURPOSE"
	CategoryState           ValidationCategory = "STATE"
)

// categoryOrder is the deterministic report ordering.
var categoryOrder = []ValidationCategory{
	CategorySyntax,
	CategoryVariable,
	CategoryRequiredSection,
	CategoryPurpose,
	CategoryState,
}

// ValidationIssue is one validation finding. Detail carries the offending
// value (variable path, section title, purpose, state key) so reports are
// actionable and byte-stable for the same input.
type ValidationIssue struct {
	Category ValidationCategory
	Detail   string
}

// ValidationReport is the ordered set of findings for one template
// validation run. An empty report is valid. The same input always yields a
// byte-identical report (determinism: fixed category order, then
// deterministic detail order per category).
type ValidationReport []ValidationIssue

// Valid reports whether the report carries no findings.
func (r ValidationReport) Valid() bool { return len(r) == 0 }

// String renders the report deterministically: one "CATEGORY detail" line
// per finding, in report order.
func (r ValidationReport) String() string {
	if len(r) == 0 {
		return "ok"
	}
	lines := make([]string, 0, len(r))
	for _, is := range r {
		lines = append(lines, string(is.Category)+" "+is.Detail)
	}
	return strings.Join(lines, "; ")
}

// ValidationRequest is everything ValidateTemplate checks: the template
// record (purpose, enabled flag), the versioned CONTENT being validated
// (create = new content, change = new version content), the variable
// registry in force, and an optional state target for the state-compatibility
// check (empty StateKey = no state target, e.g. plain create without a state).
type ValidationRequest struct {
	Template *PromptTemplate
	Content  string
	Registry *Registry

	StateKey string
}

// requiredSectionsByPurpose is the required-sections policy derived from the
// WBS 8.2/8.3 defaults: the nine default templates are structured around the
// 8.3 example sections, and a template's PURPOSE decides which of them must
// be present. Objective / Acceptance Criteria / Required Response are
// load-bearing for the worker-execution purposes (the 8.3 example shows the
// implementation shape); planning and continuation templates are free-form
// beyond the response contract; CONTROL/REVIEW/CUSTOM carry no mandatory
// sections. Section names are matched as markdown H1 titles, trimmed.
var requiredSectionsByPurpose = map[PromptPurpose][]string{
	PurposeImplementation: {"Objective", "Acceptance Criteria", "Required Response"},
	PurposeValidation:     {"Objective", "Acceptance Criteria", "Required Response"},
	PurposePhasePlanning:  {"Required Response"},
	PurposeContinuation:   {"Required Response"},
	PurposeReview:         {},
	PurposeControl:        {},
	PurposeCustom:         {},
}

// ValidateTemplate runs all five WBS 8.7 checks and returns every finding:
//
//   - syntax: via ParseTemplate (T8.6); an unparseable source short-circuits
//     the variable check (there is no IR) but section checks still run over
//     the raw source;
//   - variables: via CheckVariables against the registry;
//   - required sections: per the purpose policy;
//   - purpose: the template's purpose must be a known WBS purpose;
//   - state compatibility: a disabled template cannot satisfy a targeted
//     active state (T8.7.4).
//
// The returned report is deterministically ordered.
func ValidateTemplate(req ValidationRequest) ValidationReport {
	var report ValidationReport

	tpl := req.Template
	if tpl != nil && !tpl.Purpose.Known() {
		report = append(report, ValidationIssue{Category: CategoryPurpose, Detail: string(tpl.Purpose)})
	}

	var ir *TemplateIR
	if ir, _ = ParseTemplate(req.Content); ir != nil {
		if req.Registry != nil {
			for _, p := range CheckVariables(ir, req.Registry) {
				report = append(report, ValidationIssue{Category: CategoryVariable, Detail: p})
			}
		}
	} else {
		report = append(report, ValidationIssue{Category: CategorySyntax, Detail: "template source does not parse"})
	}

	if tpl != nil {
		for _, sec := range requiredSectionsByPurpose[tpl.Purpose] {
			if !hasSection(req.Content, sec) {
				report = append(report, ValidationIssue{Category: CategoryRequiredSection, Detail: sec})
			}
		}
	}

	if req.StateKey != "" && tpl != nil && !tpl.Enabled {
		report = append(report, ValidationIssue{Category: CategoryState, Detail: req.StateKey})
	}

	return orderReport(report)
}

// hasSection reports whether the template source carries a markdown H1
// section with exactly the given (trimmed) title.
func hasSection(content, title string) bool {
	for _, line := range strings.Split(content, "\n") {
		if strings.TrimSpace(line) == "# "+title {
			return true
		}
	}
	return false
}

// orderReport sorts findings by (fixed category order, detail) and de-
// duplicates identical issues, so the report is byte-identical for a given
// input regardless of check interleaving.
func orderReport(in ValidationReport) ValidationReport {
	seen := map[ValidationIssue]bool{}
	out := make(ValidationReport, 0, len(in))
	for _, is := range in {
		if seen[is] {
			continue
		}
		seen[is] = true
		out = append(out, is)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return categoryRank(out[i].Category) < categoryRank(out[j].Category) ||
			(categoryRank(out[i].Category) == categoryRank(out[j].Category) && out[i].Detail < out[j].Detail)
	})
	return out
}

func categoryRank(c ValidationCategory) int {
	for i, k := range categoryOrder {
		if k == c {
			return i
		}
	}
	return len(categoryOrder)
}
