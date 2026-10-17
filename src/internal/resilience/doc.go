// Package resilience is the production failure taxonomy and retry policy
// (Phase 18, WBS 18.188–18.193). It is pure: subsystems mark or register
// their errors, and every retry loop in the overseer goes through Do so
// counts, backoff and budgets stay consistent. Ambiguous side effects are
// never retried (WBS 18.190).
package resilience
