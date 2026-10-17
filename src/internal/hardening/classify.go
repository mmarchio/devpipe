package hardening

import (
	"fmt"
	"strings"
)

// Classification is a duplicate group's consolidation verdict (WBS 16.108).
type Classification string

// Classifications.
const (
	ClassSafe                 Classification = "SAFE"
	ClassUnsafeImportCycle    Classification = "UNSAFE_IMPORT_CYCLE"
	ClassUnsafeArchitecture   Classification = "UNSAFE_ARCHITECTURE"
	ClassUnsafeTypeDependency Classification = "UNSAFE_TYPE_DEPENDENCY"
	ClassUnsafeSideEffect     Classification = "UNSAFE_SIDE_EFFECT"
	ClassLowValue             Classification = "LOW_VALUE"
	ClassRequiresReview       Classification = "REQUIRES_REVIEW"
)

// AllClassifications returns every classification.
func AllClassifications() []Classification {
	return []Classification{ClassSafe, ClassUnsafeImportCycle, ClassUnsafeArchitecture, ClassUnsafeTypeDependency, ClassUnsafeSideEffect, ClassLowValue, ClassRequiresReview}
}

// Checks are the per-group findings classification is decided from. An
// empty reason means the check passed.
type Checks struct {
	SideEffect      string
	Signature       string
	ReceiverVerdict string
	Receiver        string
	LocalDeps       string
	Destination     *Destination
	DestinationWhy  string
	Layer           string
	Cycle           []string
	LowValue        string
	Retained        string
}

// Classify assigns exactly one classification and a reason to every group
// (WBS 16.108/16.109) with fixed precedence: behavioural safety first
// (side effects, type / receiver dependencies), then placement (no
// destination → review, architecture, import cycle), then value. A group
// already in the accepted-duplication baseline stays LOW_VALUE.
func Classify(c Checks) (Classification, string) {
	switch {
	case c.SideEffect != "":
		return ClassUnsafeSideEffect, c.SideEffect
	case c.Signature != "":
		return ClassUnsafeTypeDependency, c.Signature
	case c.ReceiverVerdict == ReceiverRequired:
		return ClassUnsafeTypeDependency, c.Receiver
	case c.LocalDeps != "":
		return ClassUnsafeTypeDependency, c.LocalDeps
	case c.ReceiverVerdict == ReceiverNoSharedAb:
		return ClassRequiresReview, c.Receiver
	case c.Destination == nil:
		why := c.DestinationWhy
		if why == "" {
			why = "no destination package"
		}
		return ClassRequiresReview, why
	case c.Layer != "":
		return ClassUnsafeArchitecture, c.Layer
	case len(c.Cycle) > 0:
		return ClassUnsafeImportCycle, fmt.Sprintf("import cycle %s", strings.Join(c.Cycle, " → "))
	case c.Retained != "":
		return ClassLowValue, "accepted duplication: " + c.Retained
	case c.LowValue != "":
		return ClassLowValue, c.LowValue
	}
	return ClassSafe, "safe to consolidate into " + c.Destination.ImportPath + "." + c.Destination.Symbol
}

// AnalysisContext is what group assessment needs beyond the group.
type AnalysisContext struct {
	ModulePath string
	NewBase    string
	Packages   []PackageInfo
	Graph      *ImportGraph
	Layers     Layers
	Settings   Settings
	// Retained maps accepted fingerprints to their recorded reason
	// (WBS 16.148).
	Retained map[string]string
}

// Assess runs every check over a group and classifies it (composition of
// P16-26…P16-34). The destination is a domain owner when one exists
// (16.102), else a utility package (16.100/16.101). The prospective graph
// adds dest → member imports and member packages + referencing packages →
// dest (16.104/16.105).
func Assess(g DuplicateGroup, ctx AnalysisContext) (Checks, Classification, string) {
	c := Checks{
		SideEffect: CheckSideEffects(g.Members),
		Signature:  CheckSignatureCompatibility(g.Members),
		LocalDeps:  CheckLocalDependencies(g.Members),
		LowValue:   AssessValue(g.Members, ctx.Settings),
		Retained:   ctx.Retained[g.Fingerprint],
	}
	c.ReceiverVerdict, c.Receiver = CheckReceiverCompatibility(g.Members)
	dest := ChooseDomainOwner(g.Members, ctx.Packages, ctx.Layers.Domain)
	if dest == nil {
		dest, c.DestinationWhy = ChooseUtilityPackage(g.Members, ctx.Packages, ctx.ModulePath, ctx.NewBase)
	}
	c.Destination = dest
	if dest != nil {
		destImports := g.Members[0].Imports
		c.Layer = CheckLayer(*dest, g.Members, destImports, ctx.Layers)
		if ctx.Graph != nil && ctx.Settings.DuplicateFunctions.RequireZeroImportCycles {
			c.Cycle = ctx.Graph.Prospective(dest.ImportPath, destImports, CallerPackages(g)).FindCycle()
		}
	}
	cl, why := Classify(c)
	return c, cl, why
}

// CallerPackages are the packages whose references migrate: every member
// package plus every package referencing a member, sorted.
func CallerPackages(g DuplicateGroup) []string {
	set := map[string]bool{}
	for _, m := range g.Members {
		set[m.ImportPath] = true
	}
	for _, r := range g.References {
		set[r.ImportPath] = true
	}
	return keys(set)
}
