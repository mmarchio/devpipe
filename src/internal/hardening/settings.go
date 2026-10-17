package hardening

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// DuplicateFunctionSettings configures duplicate-function consolidation
// (WBS 16.90).
type DuplicateFunctionSettings struct {
	Enabled                 bool `json:"enabled"`
	ConsolidateTests        bool `json:"consolidate_tests"`
	RequireZeroImportCycles bool `json:"require_zero_import_cycles"`
	MaxRounds               int  `json:"max_rounds"`
}

// Settings is the final_hardening configuration (WBS 16.90), captured in
// the execution configuration snapshot.
type Settings struct {
	Enabled            bool                      `json:"enabled"`
	DuplicateFunctions DuplicateFunctionSettings `json:"duplicate_functions"`
	// MinStatements: groups whose functions have fewer statements are
	// LOW_VALUE (WBS 16.107).
	MinStatements int `json:"min_statements"`
	// TransitiveTests also runs tests of packages importing affected ones.
	TransitiveTests bool `json:"transitive_tests"`
	// GroupCheckpointPolicy: REQUIRED / BEST_EFFORT / DISABLED (16.133).
	GroupCheckpointPolicy string `json:"group_checkpoint_policy"`
	// Exclusions are extra module-relative glob patterns (path.Match per
	// path segment prefix) never analysed or modified.
	Exclusions []string `json:"exclusions"`
	// DomainPackages / InfraPackages are module-relative package
	// directories forming the project's architecture layers (WBS 16.102,
	// 16.106); empty = no layer rules.
	DomainPackages []string `json:"domain_packages,omitempty"`
	InfraPackages  []string `json:"infra_packages,omitempty"`
}

// DefaultSettings returns the WBS defaults.
func DefaultSettings() Settings {
	return Settings{
		Enabled:               true,
		DuplicateFunctions:    DuplicateFunctionSettings{Enabled: true, ConsolidateTests: true, RequireZeroImportCycles: true, MaxRounds: 3},
		MinStatements:         3,
		GroupCheckpointPolicy: "DISABLED", // git is opt-in per project
		Exclusions:            []string{},
	}
}

// Validate reports every problem.
func (s Settings) Validate() error {
	var errs []error
	if s.DuplicateFunctions.MaxRounds < 1 || s.DuplicateFunctions.MaxRounds > 20 {
		errs = append(errs, fmt.Errorf("duplicate_functions.max_rounds must be in [1, 20]"))
	}
	if s.MinStatements < 0 || s.MinStatements > 1000 {
		errs = append(errs, fmt.Errorf("min_statements must be in [0, 1000]"))
	}
	switch s.GroupCheckpointPolicy {
	case "REQUIRED", "BEST_EFFORT", "DISABLED":
	default:
		errs = append(errs, fmt.Errorf("group_checkpoint_policy must be REQUIRED, BEST_EFFORT or DISABLED"))
	}
	for i, g := range s.Exclusions {
		if _, err := path.Match(g, "x"); err != nil || strings.TrimSpace(g) == "" {
			errs = append(errs, fmt.Errorf("exclusions[%d]: invalid pattern %q", i, g))
		}
	}
	return errors.Join(errs...)
}

// Exclusion reasons.
const (
	ExcludedVendor     = "vendor"
	ExcludedThirdParty = "third_party"
	ExcludedGenerated  = "generated source"
	ExcludedMock       = "generated mock"
	ExcludedExternal   = "external dependency"
	ExcludedConfigured = "configured exclusion"
)

// IsExcluded reports whether a source file must never be analysed or
// modified (WBS 16.91) and why. relPath is module-relative with '/'
// separators ("../" = outside the module); header is the file's leading
// comment text (the "Code generated … DO NOT EDIT." marker).
func IsExcluded(relPath, header string, extra []string) (bool, string) {
	p := path.Clean(relPath)
	if p == ".." || strings.HasPrefix(p, "../") || path.IsAbs(p) {
		return true, ExcludedExternal
	}
	segs := strings.Split(p, "/")
	for _, s := range segs[:len(segs)-1] {
		switch s {
		case "vendor":
			return true, ExcludedVendor
		case "third_party":
			return true, ExcludedThirdParty
		case "mocks", "mock", "fakes":
			return true, ExcludedMock
		}
	}
	base := segs[len(segs)-1]
	if strings.HasPrefix(base, "mock_") || strings.HasSuffix(base, "_mock.go") || strings.HasSuffix(base, "_mock_test.go") {
		return true, ExcludedMock
	}
	if IsGeneratedHeader(header) {
		return true, ExcludedGenerated
	}
	for _, g := range extra {
		if ok, _ := path.Match(g, p); ok {
			return true, ExcludedConfigured
		}
		for i := 1; i < len(segs); i++ {
			if ok, _ := path.Match(g, strings.Join(segs[:i], "/")); ok {
				return true, ExcludedConfigured
			}
		}
	}
	return false, ""
}

// IsGeneratedHeader reports the Go generated-code marker
// (`^// Code generated .* DO NOT EDIT\.$`) on any line of the header.
func IsGeneratedHeader(header string) bool {
	for _, line := range strings.Split(header, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "// Code generated ") && strings.HasSuffix(line, " DO NOT EDIT.") {
			return true
		}
	}
	return false
}
