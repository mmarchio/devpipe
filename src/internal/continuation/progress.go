package continuation

// ProgressComparison is the structural comparison of two consecutive
// captured statuses (WBS 14.40-14.43).
type ProgressComparison struct {
	// Progress: completed / remaining / blocking / next action changed.
	Progress bool
	// NoProgress: materially identical status (summary wording ignored).
	NoProgress bool
	// ScopeExpanded: the worker-reported remaining list grew (diagnostic
	// only — never creates work units, WBS 14.43).
	ScopeExpanded bool
}

// CompareProgress compares cur with the previous captured status of the
// same work unit (nil = first report, which counts as progress).
func CompareProgress(prev *CapturedStatus, cur CapturedStatus) ProgressComparison {
	if prev == nil {
		return ProgressComparison{Progress: true}
	}
	same := prev.ProgressKey() == cur.ProgressKey()
	return ProgressComparison{Progress: !same, NoProgress: same, ScopeExpanded: len(cur.Remaining) > len(prev.Remaining)}
}

// NextNoProgressStreak counts consecutive materially identical reports,
// the first report of a run counting as 1.
func NextNoProgressStreak(prevStreak int, cmp ProgressComparison) int {
	if cmp.NoProgress {
		if prevStreak < 1 {
			prevStreak = 1
		}
		return prevStreak + 1
	}
	return 1
}
