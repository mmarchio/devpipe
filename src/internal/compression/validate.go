package compression

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"overseer/internal/prompt"
)

// Stage names one validation-pipeline stage (WBS 9.25), in pipeline order.
type Stage string

const (
	StageStructural  Stage = "STRUCTURAL"
	StageExact       Stage = "EXACT"
	StageAcceptance  Stage = "ACCEPTANCE_CRITERIA"
	StageTests       Stage = "REQUIRED_TESTS"
	StageIdentifiers Stage = "IDENTIFIERS"
	StageSchema      Stage = "RESPONSE_SCHEMA"
	StageConstraints Stage = "CONSTRAINTS"
	StageSemantic    Stage = "SEMANTIC"
	StageSize        Stage = "SIZE"
)

// Failure codes.
const (
	CodeEmpty           = "EMPTY"
	CodeInvalidUTF8     = "INVALID_UTF8"
	CodeFramingLeak     = "FRAMING_LEAK"
	CodeMissing         = "MISSING"
	CodeChanged         = "CHANGED"
	CodeReversed        = "REVERSED"
	CodeAdded           = "ADDED"
	CodeOverTarget      = "OVER_TARGET"
	CodeIneffective     = "INEFFECTIVE"
	CodeExpanded        = "EXPANDED"
	CodeSemanticError   = "SEMANTIC_ERROR"
	CodeSemanticTimeout = "SEMANTIC_TIMEOUT"
)

// Failure is one validation failure.
type Failure struct {
	Stage  Stage  `json:"stage"`
	Code   string `json:"code"`
	ItemID string `json:"item_id,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// ItemResult reports whether one preserved item / exact fragment survived.
type ItemResult struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Preserved bool   `json:"preserved"`
}

// ValidationResult is the outcome of the validation pipeline.
type ValidationResult struct {
	Status    ValidationStatus          `json:"status"`
	Failures  []Failure                 `json:"failures"`
	Items     []ItemResult              `json:"items"`
	Stages    []Stage                   `json:"stages"`
	Semantic  *SemanticValidationResult `json:"semantic,omitempty"`
	Savings   SavingsClass              `json:"savings,omitempty"`
	Canonical int                       `json:"canonical_tokens"`
	Output    int                       `json:"compressed_tokens"`
}

// Passed reports whether the output was accepted.
func (r ValidationResult) Passed() bool { return r.Status == ValidationPassed }

// ValidationInput is everything the deterministic validators need. All
// reference data comes from the CANONICAL prompt (components with effective
// policies + manifest); the compressed output is only ever the subject.
type ValidationInput struct {
	// Components are the effective-policy components sent for compression.
	Components []CompressionComponent
	// Canonical is the full canonical component set (before optional
	// drops), the semantic reference.
	Canonical []CompressionComponent
	Manifest  Manifest
	// Boundary is the structured-input boundary token of the request.
	Boundary string

	Compressed string

	// AllowPlaceholders permits placeholder markers (only when compressing
	// a pending precomputed template, Phase 11 Decision 5); a final worker
	// prompt never carries one.
	AllowPlaceholders bool

	CanonicalTokens int
	TargetTokens    int
	MinimumSavings  float64
}

// ValidateStructure: non-empty, valid UTF-8, and no leaked framing (the
// boundary token or a section header) (PLAN P9-28).
func ValidateStructure(in ValidationInput) []Failure {
	var out []Failure
	if strings.TrimSpace(in.Compressed) == "" {
		return []Failure{{Stage: StageStructural, Code: CodeEmpty, Detail: "compressed output is empty"}}
	}
	if !utf8.ValidString(in.Compressed) {
		out = append(out, Failure{Stage: StageStructural, Code: CodeInvalidUTF8, Detail: "compressed output is not valid UTF-8"})
	}
	if !in.AllowPlaceholders && ContainsPlaceholder(in.Compressed) {
		out = append(out, Failure{Stage: StageStructural, Code: CodePlaceholder, Detail: "compressed output contains an unresolved placeholder"})
	}
	if (in.Boundary != "" && strings.Contains(in.Compressed, in.Boundary)) || headerLeakRE.MatchString(in.Compressed) {
		out = append(out, Failure{Stage: StageStructural, Code: CodeFramingLeak, Detail: "compressed output contains structured-input framing"})
	}
	return out
}

var headerLeakRE = regexp.MustCompile(`<<<CB[0-9A-F]{16} (SECTION|END) `)

// ValidateExact: every exact fragment (PRESERVE_EXACT components and
// <preserve> regions) appears verbatim (WBS 9.17; no LLM).
func ValidateExact(in ValidationInput) []Failure {
	var out []Failure
	for _, f := range ExactFragments(in.Components) {
		if !strings.Contains(in.Compressed, f.Text) {
			out = append(out, Failure{Stage: StageExact, Code: CodeMissing, ItemID: f.ID,
				Detail: string(f.Source) + " " + string(f.Type) + " content not preserved verbatim"})
		}
	}
	return out
}

// verbatimItems checks that every manifest item of the kinds appears
// verbatim, keyed by its overseer-assigned ID.
func verbatimItems(in ValidationInput, stage Stage, kinds ...ItemKind) []Failure {
	var out []Failure
	for _, k := range kinds {
		for _, it := range in.Manifest.ByKind(k) {
			if !strings.Contains(in.Compressed, it.Text) {
				out = append(out, Failure{Stage: stage, Code: CodeMissing, ItemID: it.ID, Detail: truncate(it.Text, 120)})
			}
		}
	}
	return out
}

// ValidateAcceptanceCriteria: every AC-n survives verbatim; order is not
// enforced; IDs come only from the canonical manifest (WBS 9.18).
func ValidateAcceptanceCriteria(in ValidationInput) []Failure {
	return verbatimItems(in, StageAcceptance, ItemAcceptance)
}

// ValidateRequiredTests: every TEST-n command survives verbatim; extra
// tests in the output never substitute for a required one (WBS 9.19).
func ValidateRequiredTests(in ValidationInput) []Failure {
	return verbatimItems(in, StageTests, ItemTest)
}

// ValidateIdentifiers: every included identifier survives verbatim; a
// hallucinated replacement leaves the original missing (WBS 9.20).
func ValidateIdentifiers(in ValidationInput) []Failure {
	return verbatimItems(in, StageIdentifiers,
		ItemProjectID, ItemPhaseID, ItemWorkUnitID, ItemWorkUnitName, ItemDependencyID, ItemStateKey, ItemRoleKey, ItemContextBoundary)
}

var outputStatusRE = regexp.MustCompile(`"status"\s*:\s*"([^"]+)"`)

// ValidateResponseSchema: every schema field survives in schema form
// (`"field":`), and every allowed status value survives inside a status
// enumeration (`"status": "a|b|c"`) — prose mentions never substitute
// (WBS 9.21).
func ValidateResponseSchema(in ValidationInput) []Failure {
	var out []Failure
	for _, it := range in.Manifest.ByKind(ItemSchemaField) {
		re := regexp.MustCompile(regexp.QuoteMeta(it.Text) + `\s*:`)
		if !re.MatchString(in.Compressed) {
			out = append(out, Failure{Stage: StageSchema, Code: CodeMissing, ItemID: it.ID, Detail: "schema field " + it.Text + " missing or renamed"})
		}
	}
	statuses := in.Manifest.ByKind(ItemStatusValue)
	if len(statuses) > 0 {
		allowed := map[string]bool{}
		for _, m := range outputStatusRE.FindAllStringSubmatch(in.Compressed, -1) {
			for _, v := range strings.Split(m[1], "|") {
				allowed[strings.TrimSpace(v)] = true
			}
		}
		for _, it := range statuses {
			if !allowed[it.Text] {
				out = append(out, Failure{Stage: StageSchema, Code: CodeMissing, ItemID: it.ID, Detail: "allowed status value " + it.Text + " missing from the schema"})
			}
		}
	}
	return out
}

// negationPrefixRE finds the prohibition marker of a constraint (anywhere,
// so an ID label such as "CONSTRAINT-2:" may precede it).
var negationPrefixRE = regexp.MustCompile(`(?i)\b(do not|don't|never|must not|must never|shall not|no)\s+`)

// ValidateConstraints: every CONSTRAINT-n survives verbatim. A missing
// prohibition whose positive core appears un-negated in the output is
// reported REVERSED (WBS 9.22/9.54).
func ValidateConstraints(in ValidationInput) []Failure {
	var out []Failure
	for _, it := range in.Manifest.ByKind(ItemConstraint) {
		if strings.Contains(in.Compressed, it.Text) {
			continue
		}
		code := CodeMissing
		if loc := negationPrefixRE.FindStringIndex(it.Text); loc != nil {
			core := strings.ToLower(strings.TrimRight(strings.TrimSpace(it.Text[loc[1]:]), ".!;"))
			if core != "" {
				for _, sent := range splitSentences(in.Compressed) {
					ls := strings.ToLower(sent)
					if strings.Contains(ls, core) && !hasNegation(ls) {
						code = CodeReversed
						break
					}
				}
			}
		}
		out = append(out, Failure{Stage: StageConstraints, Code: code, ItemID: it.ID, Detail: truncate(it.Text, 120)})
	}
	return out
}

// itemResults reports per-item preservation for previews (WBS 9.45).
func itemResults(in ValidationInput, failures []Failure) []ItemResult {
	failed := map[string]bool{}
	for _, f := range failures {
		if f.ItemID != "" {
			failed[f.ItemID] = true
		}
	}
	var out []ItemResult
	for _, f := range ExactFragments(in.Components) {
		out = append(out, ItemResult{ID: f.ID, Kind: string(f.Source), Preserved: !failed[f.ID]})
	}
	for _, it := range in.Manifest {
		out = append(out, ItemResult{ID: it.ID, Kind: string(it.Kind), Preserved: !failed[it.ID]})
	}
	return out
}

var sentenceSplitRE = regexp.MustCompile(`[.!?;\n]+`)

func splitSentences(s string) []string {
	var out []string
	for _, p := range sentenceSplitRE.Split(s, -1) {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

var negationRE = regexp.MustCompile(`(?i)(\bnot\b|\bnever\b|\bno\b|n't\b|\bwithout\b|\bavoid\b|\bprohibited\b|\bforbidden\b|\bmust not\b|✗|!=)`)

func hasNegation(s string) bool { return negationRE.MatchString(s) }

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// componentTypesOf is a small helper used by semantic checks.
func componentsWithPolicy(comps []CompressionComponent, p prompt.CompressionPolicy) []CompressionComponent {
	var out []CompressionComponent
	for _, c := range comps {
		if c.Policy == p {
			out = append(out, c)
		}
	}
	return out
}
