package hardening

import (
	"fmt"
	"strings"
)

// CheckSignatureCompatibility verifies the members share one type contract
// (WBS 16.97): identical type parameters, parameters, results and
// variadicity, and no type declared in a member's own package (such a type
// differs per package and would drag the package into the shared one).
// Returns "" when compatible, else the reason.
func CheckSignatureCompatibility(members []FunctionInfo) string {
	if len(members) == 0 {
		return "empty group"
	}
	ref := members[0].Signature.String()
	for _, m := range members {
		if s := m.Signature.String(); s != ref {
			return fmt.Sprintf("signature of %s %s differs from %s", m.QualifiedName(), s, ref)
		}
		for _, t := range append(append(append([]string(nil), m.Signature.Params...), m.Signature.Results...), m.Signature.TypeParams...) {
			if mentionsPackage(t, m.ImportPath) {
				return fmt.Sprintf("%s uses package-local type %s", m.QualifiedName(), t)
			}
		}
	}
	return ""
}

// mentionsPackage reports a qualified type string referencing pkg
// ("pkg.T", "[]pkg.T", "map[string]*pkg.T", …).
func mentionsPackage(typ, pkg string) bool {
	i := strings.Index(typ, pkg+".")
	for i >= 0 {
		before := byte(' ')
		if i > 0 {
			before = typ[i-1]
		}
		if !(before == '/' || before == '.' || isIdent(before)) {
			return true
		}
		j := strings.Index(typ[i+1:], pkg+".")
		if j < 0 {
			return false
		}
		i += j + 1
	}
	return false
}

func isIdent(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// Receiver verdicts.
const (
	ReceiverNone       = ""
	ReceiverRequired   = "RECEIVER_REQUIRED"
	ReceiverNoSharedAb = "NO_SHARED_ABSTRACTION"
)

// CheckReceiverCompatibility (WBS 16.98): a method whose body uses its
// receiver is receiver-dependent (never extracted); a method that ignores
// its receiver could become a function but no shared abstraction exists,
// so it requires review. Plain functions pass.
func CheckReceiverCompatibility(members []FunctionInfo) (verdict, reason string) {
	for _, m := range members {
		if m.Receiver != "" && m.ReceiverUsed {
			return ReceiverRequired, fmt.Sprintf("%s depends on its receiver", m.QualifiedName())
		}
	}
	for _, m := range members {
		if m.Receiver != "" {
			return ReceiverNoSharedAb, fmt.Sprintf("%s is a method; no shared abstraction exists for its receiver", m.QualifiedName())
		}
	}
	return ReceiverNone, ""
}

// CheckSideEffects (WBS 16.99): members touching package-level state of
// their own package, or with different effect sets, are not consolidated —
// similar syntax over different state is different behaviour.
func CheckSideEffects(members []FunctionInfo) string {
	if len(members) == 0 {
		return ""
	}
	for _, m := range members {
		for _, g := range append(append([]string(nil), m.Effects.GlobalsRead...), m.Effects.GlobalsWritten...) {
			if strings.HasPrefix(g, m.ImportPath+".") {
				return fmt.Sprintf("%s touches package state %s", m.QualifiedName(), g)
			}
		}
	}
	ref := members[0].Effects.Key()
	for _, m := range members[1:] {
		if m.Effects.Key() != ref {
			return fmt.Sprintf("%s has different side effects than %s", m.QualifiedName(), members[0].QualifiedName())
		}
	}
	return ""
}

// CheckLocalDependencies reports members depending on other declarations
// of their own package: moving them would need those too (type
// dependency).
func CheckLocalDependencies(members []FunctionInfo) string {
	for _, m := range members {
		if len(m.LocalDeps) > 0 {
			return fmt.Sprintf("%s depends on package-local %s", m.QualifiedName(), strings.Join(m.LocalDeps, ", "))
		}
	}
	return ""
}
