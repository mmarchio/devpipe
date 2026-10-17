package prompt

import (
	"errors"
	"fmt"
	"strings"
)

// VariableNamespace is one of the WBS 8.5 explicit template-variable
// namespaces. Only these namespaces may host registered variables; any
// other prefix (an internal Go type or a raw setting) is not reachable.
type VariableNamespace string

const (
	NamespaceProject          VariableNamespace = "project"
	NamespacePhase            VariableNamespace = "phase"
	NamespaceWBS              VariableNamespace = "wbs"
	NamespaceWorkUnit         VariableNamespace = "work_unit"
	NamespaceRuntime          VariableNamespace = "runtime"
	NamespaceState            VariableNamespace = "state"
	NamespaceRole             VariableNamespace = "role"
	NamespaceStateFiles       VariableNamespace = "state_files"
	NamespaceContinuation     VariableNamespace = "continuation"
	NamespaceResponseProtocol VariableNamespace = "response_protocol"
)

// AllNamespaces is the complete WBS 8.5 namespace set (T8.5.1).
var AllNamespaces = []VariableNamespace{
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

// KnownNamespace reports whether ns is a WBS 8.5 namespace.
func KnownNamespace(ns VariableNamespace) bool {
	for _, n := range AllNamespaces {
		if n == ns {
			return true
		}
	}
	return false
}

// ErrUnknownVariable is returned by Resolve for a path that was never
// registered (WBS 8.5, T8.5.2).
var ErrUnknownVariable = errors.New("prompt: unknown variable")

// ErrSensitiveVariable is returned by RegisterPromptSafe when the variable
// path's field classifies sensitive (WBS 8.38, T8.38.1/T8.38.2): the
// value can never enter a template through the prompt-safe surface.
type ErrSensitiveVariable struct {
	Path string
}

func (e ErrSensitiveVariable) Error() string {
	return "prompt: sensitive variable not prompt-safe: " + e.Path
}

// Registry is the explicit, reflection-free set of variables a template
// may reference (WBS 8.5). A variable is available only if it was
// explicitly registered under a known namespace: internal Go struct fields
// and sensitive settings are unreachable by default and become available
// only through an explicit Register (T8.5.3/T8.5.4). No reflection into
// internal Go objects is used; values are plain strings supplied by the
// composer. promptSafeVars is the WBS 8.38 prompt-safe set (the ONLY
// template surface); sensitiveValues tracks unsafe-registered secret
// values for the provenance surface filter (T8.38.5).
type Registry struct {
	vars            map[string]string
	promptSafeVars  map[string]string
	sensitiveValues map[string]bool
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		vars:            map[string]string{},
		promptSafeVars:  map[string]string{},
		sensitiveValues: map[string]bool{},
	}
}

// ParsePath splits a "namespace.field" variable path into its namespace and
// field. A valid path carries exactly one dot, a non-empty KNOWN namespace
// prefix, and a non-empty field. Any other shape is invalid, which is what
// keeps internal Go field names and raw settings out of the namespace.
func ParsePath(p string) (VariableNamespace, string, error) {
	i := strings.Index(p, ".")
	if i <= 0 {
		return "", "", fmt.Errorf("prompt: variable path %q must be namespace.field", p)
	}
	ns := VariableNamespace(p[:i])
	field := p[i+1:]
	if field == "" {
		return "", "", fmt.Errorf("prompt: variable path %q has no field", p)
	}
	if !KnownNamespace(ns) {
		return "", "", fmt.Errorf("prompt: unknown namespace %q", ns)
	}
	return ns, field, nil
}

// Register makes a variable path available with a string value. The path
// must be a known namespace with a non-empty field. Registering the same
// path again overwrites its value.
func (r *Registry) Register(path, value string) error {
	if _, _, err := ParsePath(path); err != nil {
		return err
	}
	r.registerUnsafe(path, value)
	return nil
}

// Resolve returns the registered value for a variable path (T8.5.1). An
// unregistered path is rejected with ErrUnknownVariable (T8.5.2).
func (r *Registry) Resolve(path string) (string, error) {
	v, ok := r.vars[path]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrUnknownVariable, path)
	}
	return v, nil
}

// Has reports whether a path is registered.
func (r *Registry) Has(path string) bool {
	_, ok := r.vars[path]
	return ok
}

// Len reports the number of registered variables.
func (r *Registry) Len() int {
	return len(r.vars)
}
