// Package access is the pure authorization model of the web control plane
// (WBS 17.126–17.129): principals, actions, resources and the authorization
// policy. It has no HTTP, database or gateway dependency.
package access

import (
	"errors"
	"fmt"
	"strings"
)

// PrincipalType classifies who acts (WBS 17.127).
type PrincipalType string

const (
	// PrincipalLocalAdmin is the local / internal administrator of v1.
	PrincipalLocalAdmin PrincipalType = "LOCAL_ADMIN"
	// PrincipalSystem is the overseer itself (scheduler, recovery).
	PrincipalSystem PrincipalType = "SYSTEM"
	// PrincipalUser is a future named user; v1 grants it read access only.
	PrincipalUser PrincipalType = "USER"
)

// ErrInvalidPrincipal is returned for a malformed principal.
var ErrInvalidPrincipal = errors.New("access: invalid principal")

// ParsePrincipalType parses a principal type.
func ParsePrincipalType(s string) (PrincipalType, error) {
	switch t := PrincipalType(s); t {
	case PrincipalLocalAdmin, PrincipalSystem, PrincipalUser:
		return t, nil
	}
	return "", fmt.Errorf("%w: unknown type %q", ErrInvalidPrincipal, s)
}

// Principal is an authenticated actor (WBS 17.127).
type Principal struct {
	ID   string
	Name string
	Type PrincipalType
}

// IsZero reports whether p is the unauthenticated zero value.
func (p Principal) IsZero() bool { return p == Principal{} }

// Validate checks the principal is complete and typed.
func (p Principal) Validate() error {
	if strings.TrimSpace(p.ID) == "" {
		return fmt.Errorf("%w: id required", ErrInvalidPrincipal)
	}
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("%w: name required", ErrInvalidPrincipal)
	}
	if _, err := ParsePrincipalType(string(p.Type)); err != nil {
		return err
	}
	return nil
}

// LocalAdmin is the v1 local administrator principal.
func LocalAdmin() Principal {
	return Principal{ID: "local-admin", Name: "Local administrator", Type: PrincipalLocalAdmin}
}

// System is the overseer's own principal.
func System() Principal {
	return Principal{ID: "system", Name: "Overseer", Type: PrincipalSystem}
}
