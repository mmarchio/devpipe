package continuation

// Counts are the persisted per-work-unit counters a limit check uses.
type Counts struct {
	// SuccessfulResets counts continuations whose fresh session was
	// confirmed; failed creations are never counted here (WBS 14.39).
	SuccessfulResets int
	// NoProgressStreak counts consecutive materially identical reports.
	NoProgressStreak int
}

// ContinuationLimitReached reports an attempt beyond the configured
// maximum (WBS 14.37); never when the maximum is Unlimited.
func ContinuationLimitReached(attempt int, s Settings) bool {
	return s.MaxContinuationsPerWorkUnit != Unlimited && attempt > s.MaxContinuationsPerWorkUnit
}

// ResetLimitReached reports that no further successful session reset is
// allowed (WBS 14.38); never when the maximum is Unlimited.
func ResetLimitReached(successfulResets int, s Settings) bool {
	return s.MaxSessionResetsPerWorkUnit != Unlimited && successfulResets >= s.MaxSessionResetsPerWorkUnit
}

// CheckLimits returns the recovery reason when a limit stops the
// continuation (WBS 14.37, 14.38, 14.41). The reset limit is independent
// of the continuation count and applies only to a fresh-session strategy.
func CheckLimits(attempt int, c Counts, s Settings, strategy Strategy) (RecoveryReason, bool) {
	switch {
	case ContinuationLimitReached(attempt, s):
		return RecoveryContinuationLimit, true
	case c.NoProgressStreak >= s.MaxNoProgressContinuations:
		return RecoveryNoProgress, true
	case strategy == ContinueFreshSession && ResetLimitReached(c.SuccessfulResets, s):
		return RecoveryResetLimit, true
	}
	return "", false
}
