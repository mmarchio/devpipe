package cache

import (
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"
)

// RetentionPolicy selects which cache entries garbage collection may remove
// (WBS 10.35). Only cache-entry rows are ever removed — prompt artifacts
// are immutable audit history.
type RetentionPolicy string

const (
	RetainAll    RetentionPolicy = "RETAIN_ALL"
	RetainDays   RetentionPolicy = "RETAIN_DAYS"
	RetainLatest RetentionPolicy = "RETAIN_LATEST_N"
)

// Retention is a configured policy.
type Retention struct {
	Policy RetentionPolicy
	// Value is N days (RETAIN_DAYS) or N entries per slot (RETAIN_LATEST_N).
	Value int
}

// ErrInvalidRetention rejects an unusable policy.
var ErrInvalidRetention = errors.New("cache: invalid retention policy")

// Validate checks the policy.
func (r Retention) Validate() error {
	switch r.Policy {
	case RetainAll:
		return nil
	case RetainDays, RetainLatest:
		if r.Value < 1 {
			return ErrInvalidRetention
		}
		return nil
	}
	return ErrInvalidRetention
}

// slotKey groups entries of one (state, work unit, layer) slot.
func slotKey(e PromptCacheEntry) string {
	u := ""
	if e.WorkUnitID != nil {
		u = e.WorkUnitID.String()
	}
	return string(e.Layer) + "|" + e.StateKey + "|" + u
}

// Eligible returns the ids of entries the policy allows GC to delete
// (T10.35.1-T10.35.4): never a BUILDING entry, never an entry referenced by
// execution history, never the newest VALID entry of its slot; RETAIN_DAYS
// removes entries last used before the window; RETAIN_LATEST_N keeps the
// newest N entries per slot. Deterministic (ordered by id).
func (r Retention) Eligible(entries []PromptCacheEntry, now time.Time, referenced func(uuid.UUID) bool) []uuid.UUID {
	if r.Validate() != nil || r.Policy == RetainAll {
		return nil
	}
	bySlot := map[string][]PromptCacheEntry{}
	for _, e := range entries {
		bySlot[slotKey(e)] = append(bySlot[slotKey(e)], e)
	}
	var out []uuid.UUID
	for _, group := range bySlot {
		sort.Slice(group, func(i, j int) bool {
			if !group[i].CreatedAt.Equal(group[j].CreatedAt) {
				return group[i].CreatedAt.After(group[j].CreatedAt)
			}
			return group[i].ID.String() > group[j].ID.String()
		})
		latestValid := uuid.Nil
		for _, e := range group {
			if e.Status == StatusValid {
				latestValid = e.ID
				break
			}
		}
		for i, e := range group {
			if e.Status == StatusBuilding || e.ID == latestValid || (referenced != nil && referenced(e.ID)) {
				continue
			}
			switch r.Policy {
			case RetainDays:
				if e.LastUsedAt.Before(now.Add(-time.Duration(r.Value) * 24 * time.Hour)) {
					out = append(out, e.ID)
				}
			case RetainLatest:
				if i >= r.Value {
					out = append(out, e.ID)
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}
