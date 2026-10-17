// Package continuation is the Phase 14 domain: the deterministic
// continuation controller's lifecycle, policy, captured worker status,
// progress and limit rules, failure classification, restart
// reconciliation planning and read models. It is infrastructure-free:
// context comes only from Phase 12 telemetry, worker status from the
// normalized Phase 13 result, and nothing here performs side effects.
package continuation
