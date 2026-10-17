package viewmodels

import "time"

// NavItem is one primary navigation entry (WBS 17.3).
type NavItem struct {
	Label   string
	Href    string
	Current bool
}

// ProjectContext is the shell's current project context.
type ProjectContext struct {
	ID              string
	Name            string
	Status          string
	ExecutionID     string
	ExecutionStatus string
	Recovery        bool
}

// Shell is the persistent application shell (WBS 17.3, 17.136, 17.137).
type Shell struct {
	Title       string
	Nav         []NavItem
	Project     *ProjectContext
	CSRF        string
	Version     string
	LiveProject string
	LiveAfter   int64
	Flash       string
	FlashKind   string
	Now         time.Time
}

// Page is the data of every rendered page.
type Page struct {
	Shell Shell
	Data  any
	// Form re-renders submitted values (escaped by the templates).
	Form map[string]string
	// Errors are backend field errors shown beside their fields (WBS 17.9).
	Errors map[string]string
	// Message is a page-level error / conflict message.
	Message string
	// Conflict marks a 409 (stale) submission: the page offers reload
	// instead of overwriting newer state (WBS 17.133).
	Conflict bool
}

// Primary is the primary navigation (WBS 17.3).
func Primary(active string) []NavItem {
	items := []NavItem{
		{Label: "Projects", Href: "/projects"},
		{Label: "Executions", Href: "/executions"},
		{Label: "Workers", Href: "/workers"},
		{Label: "Configuration", Href: "/configuration"},
		{Label: "Audit", Href: "/audit"},
		{Label: "System", Href: "/system"},
	}
	for i := range items {
		items[i].Current = items[i].Label == active
	}
	return items
}
