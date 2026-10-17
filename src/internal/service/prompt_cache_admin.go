package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"overseer/internal/cache"
	"overseer/internal/persistence"
)

// ---------------------------------------------------------------------------
// Explain (WBS 10.40/10.61): read-only.

// CacheExplanation says why a request would hit or miss.
type CacheExplanation struct {
	Hit       bool
	CacheKey  string
	EntryID   *uuid.UUID
	Reference *cache.PromptCacheEntry
	Reason    string
	Changed   []cache.Change
	Unchanged []string
}

// Explain derives the hit/miss explanation from stored semantic metadata
// (no prompt text comparison) and mutates nothing: no events, no hit
// counts, no quarantine.
func (s *PromptCacheService) Explain(ctx context.Context, projectID uuid.UUID, req PrepareRequest) (*CacheExplanation, error) {
	b, err := s.buildCacheInput(ctx, projectID, req)
	if err != nil {
		return nil, err
	}
	out := &CacheExplanation{CacheKey: b.key}
	if e, err := s.entries.GetValid(ctx, projectID, b.key, cache.LayerWorker); err == nil {
		_, reason, verr := s.verify(ctx, projectID, e, &b.metadata)
		if verr != nil {
			return nil, verr
		}
		id := e.ID
		out.EntryID, out.Reference = &id, e
		d := cache.Diff(e.Metadata, b.metadata)
		out.Changed, out.Unchanged = d.Changed, d.Unchanged
		if reason == "" {
			out.Hit, out.Reason = true, "all semantic inputs match a valid entry"
			return out, nil
		}
		out.Reason = "entry exists but is corrupt: " + reason
		return out, nil
	} else if !errors.Is(err, persistence.ErrNotFound) {
		return nil, translate(err)
	}
	ref, err := s.entries.LatestInSlot(ctx, projectID, b.input.StateKey, b.snapshot.WorkUnitID, cache.LayerWorker)
	if errors.Is(err, persistence.ErrNotFound) {
		out.Reason = "no cache entry exists for this state and work unit"
		for _, in := range b.metadata.Inputs {
			out.Changed = append(out.Changed, cache.Change{Name: in.Name, Before: "(none)", After: "(new)"})
		}
		return out, nil
	}
	if err != nil {
		return nil, translate(err)
	}
	d := cache.Diff(ref.Metadata, b.metadata)
	id := ref.ID
	out.EntryID, out.Reference, out.Changed, out.Unchanged = &id, ref, d.Changed, d.Unchanged
	switch {
	case !d.Equal():
		out.Reason = "semantic inputs changed: " + summarize(d)
	default:
		out.Reason = fmt.Sprintf("matching entry is %s", strings.ToLower(string(ref.Status)))
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Inspection (WBS 10.39/10.63).

// ListEntries lists the project's entries (filters; project-scoped).
func (s *PromptCacheService) ListEntries(ctx context.Context, f persistence.PromptCacheFilter) ([]cache.PromptCacheEntry, error) {
	if _, err := s.projSettings.Get(ctx, f.ProjectID.String()); err != nil {
		return nil, translate(err)
	}
	out, err := s.entries.List(ctx, f)
	if err != nil {
		return nil, translate(err)
	}
	return out, nil
}

// GetEntry returns one entry; an entry of another project is ErrNotFound
// (a direct id cannot bypass ownership, T10.63.2).
func (s *PromptCacheService) GetEntry(ctx context.Context, projectID, entryID uuid.UUID) (*cache.PromptCacheEntry, error) {
	e, err := s.entries.Get(ctx, entryID)
	if err != nil {
		return nil, translate(err)
	}
	if e.ProjectID != projectID {
		return nil, ErrNotFound
	}
	return e, nil
}

// ---------------------------------------------------------------------------
// Invalidation (WBS 10.41/10.42): marks entries, never deletes artifacts.

// Invalidate marks one VALID entry INVALIDATED with a reason.
func (s *PromptCacheService) Invalidate(ctx context.Context, projectID, entryID uuid.UUID, reason string) (*cache.PromptCacheEntry, error) {
	e, err := s.GetEntry(ctx, projectID, entryID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(reason) == "" {
		reason = cache.ReasonManual
	}
	ok, err := s.entries.MarkInvalid(ctx, e.ID, reason)
	if err != nil {
		return nil, translate(err)
	}
	if !ok {
		return nil, fmt.Errorf("%w: entry is %s", ErrConflict, e.Status)
	}
	s.emitPlain(ctx, projectID, cache.EventInvalidate, e, reason)
	return s.entries.Get(ctx, e.ID)
}

// InvalidateProject invalidates every VALID entry of the project.
func (s *PromptCacheService) InvalidateProject(ctx context.Context, projectID uuid.UUID, reason string) (int, error) {
	if _, err := s.projSettings.Get(ctx, projectID.String()); err != nil {
		return 0, translate(err)
	}
	if strings.TrimSpace(reason) == "" {
		reason = cache.ReasonManual
	}
	n, err := s.entries.InvalidateProject(ctx, projectID, reason)
	if err != nil {
		return 0, translate(err)
	}
	s.emitPlain(ctx, projectID, cache.EventInvalidate, nil, fmt.Sprintf("project-wide: %s (%d entries)", reason, n))
	return n, nil
}

// ---------------------------------------------------------------------------
// Integrity scan (WBS 10.58).

// IntegrityReport is the scan result.
type IntegrityReport struct {
	Scanned int
	Healthy int
	Corrupt []CorruptEntry
}

// CorruptEntry is one quarantined entry.
type CorruptEntry struct {
	EntryID uuid.UUID
	Reason  string
}

// IntegrityScan verifies every VALID entry's artifact chain and hashes;
// corrupt entries are quarantined (INVALIDATED, reason CORRUPT).
func (s *PromptCacheService) IntegrityScan(ctx context.Context, projectID uuid.UUID) (*IntegrityReport, error) {
	st := cache.StatusValid
	entries, err := s.ListEntries(ctx, persistence.PromptCacheFilter{ProjectID: projectID, Status: &st})
	if err != nil {
		return nil, err
	}
	rep := &IntegrityReport{}
	for i := range entries {
		e := entries[i]
		rep.Scanned++
		_, reason, err := s.verify(ctx, projectID, &e, nil)
		if err != nil {
			return nil, translate(err)
		}
		if reason == "" {
			rep.Healthy++
			continue
		}
		s.quarantine(ctx, projectID, &e, reason)
		rep.Corrupt = append(rep.Corrupt, CorruptEntry{EntryID: e.ID, Reason: reason})
	}
	return rep, nil
}

// ---------------------------------------------------------------------------
// Retention / GC (WBS 10.35/10.36).

// GarbageCollect deletes the cache-entry rows the retention policy allows
// (never prompt artifacts; never referenced or latest-valid entries).
// Idempotent.
func (s *PromptCacheService) GarbageCollect(ctx context.Context, projectID uuid.UUID) (int, error) {
	cs, _, err := s.settings.Get(ctx, projectID)
	if err != nil {
		return 0, translate(err)
	}
	all, err := s.ListEntries(ctx, persistence.PromptCacheFilter{ProjectID: projectID})
	if err != nil {
		return 0, err
	}
	ids := cs.Retention.Eligible(all, s.now(), s.referenced)
	n, err := s.entries.DeleteEntries(ctx, projectID, ids)
	if err != nil {
		return 0, translate(err)
	}
	return n, nil
}

// WithReferenceCheck installs the execution-history reference hook (Phase
// 12+ executions reference worker prompts; GC must never purge those).
func (s *PromptCacheService) WithReferenceCheck(fn func(uuid.UUID) bool) *PromptCacheService {
	s.referenced = fn
	return s
}

// ---------------------------------------------------------------------------
// Stats / activity (WBS 10.37/10.38/10.64/10.65).

// Stats aggregates the project's cache statistics from persisted entries
// and events (reconstructable after restart).
func (s *PromptCacheService) Stats(ctx context.Context, projectID uuid.UUID) (cache.Stats, error) {
	entries, err := s.ListEntries(ctx, persistence.PromptCacheFilter{ProjectID: projectID})
	if err != nil {
		return cache.Stats{}, err
	}
	events, err := s.events.ListByProject(ctx, projectID, 0)
	if err != nil {
		return cache.Stats{}, translate(err)
	}
	return cache.Aggregate(entries, events), nil
}

// ActivityItem is one row of the recent-activity read model (no prompt
// content).
type ActivityItem struct {
	Event      cache.EventType
	Layer      cache.Layer
	StateKey   string
	WorkUnitID *uuid.UUID
	Reason     string
	EntryID    *uuid.UUID
	At         string
}

// Activity returns recent cache activity, newest first (WBS 10.65).
func (s *PromptCacheService) Activity(ctx context.Context, projectID uuid.UUID, limit int) ([]ActivityItem, error) {
	if _, err := s.projSettings.Get(ctx, projectID.String()); err != nil {
		return nil, translate(err)
	}
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	evs, err := s.events.ListByProject(ctx, projectID, 0)
	if err != nil {
		return nil, translate(err)
	}
	var out []ActivityItem
	for _, e := range evs {
		switch e.Type {
		case cache.EventHit, cache.EventMiss, cache.EventBypass, cache.EventSuppressed, cache.EventCorrupt, cache.EventInvalidate, cache.EventBuildFailed:
		default:
			continue
		}
		out = append(out, ActivityItem{Event: e.Type, Layer: e.Layer, StateKey: e.StateKey, WorkUnitID: e.WorkUnitID,
			Reason: e.Reason, EntryID: e.EntryID, At: e.CreatedAt.UTC().Format("2006-01-02T15:04:05.000000Z07:00")})
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Settings.

// CacheSettings returns the project's cache policy (defaults if unstored).
func (s *PromptCacheService) CacheSettings(ctx context.Context, projectID uuid.UUID) (cache.Settings, bool, error) {
	if _, err := s.projSettings.Get(ctx, projectID.String()); err != nil {
		return cache.Settings{}, false, translate(err)
	}
	st, stored, err := s.settings.Get(ctx, projectID)
	if err != nil {
		return cache.Settings{}, false, translate(err)
	}
	return st, stored, nil
}

// UpdateCacheSettings validates and stores the policy.
func (s *PromptCacheService) UpdateCacheSettings(ctx context.Context, projectID uuid.UUID, st cache.Settings) (cache.Settings, error) {
	if _, err := s.projSettings.Get(ctx, projectID.String()); err != nil {
		return cache.Settings{}, translate(err)
	}
	if err := st.Validate(); err != nil {
		return cache.Settings{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if err := s.settings.Upsert(ctx, projectID, st); err != nil {
		return cache.Settings{}, translate(err)
	}
	return st, nil
}

// Fragments lists the project's static prompt fragments.
func (s *PromptCacheService) Fragments(ctx context.Context, projectID uuid.UUID) ([]cache.PromptFragment, error) {
	out, err := s.fragments.ListByProject(ctx, projectID)
	if err != nil {
		return nil, translate(err)
	}
	return out, nil
}
