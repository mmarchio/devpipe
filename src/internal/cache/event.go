package cache

import (
	"time"

	"github.com/google/uuid"
)

// EventType is a structured cache audit event (WBS 10.66).
type EventType string

const (
	EventLookup         EventType = "CACHE_LOOKUP"
	EventHit            EventType = "CACHE_HIT"
	EventMiss           EventType = "CACHE_MISS"
	EventStore          EventType = "CACHE_STORE"
	EventInvalidate     EventType = "CACHE_INVALIDATE"
	EventCorrupt        EventType = "CACHE_CORRUPT"
	EventBuildStarted   EventType = "CACHE_BUILD_STARTED"
	EventBuildCompleted EventType = "CACHE_BUILD_COMPLETED"
	EventBuildFailed    EventType = "CACHE_BUILD_FAILED"
	// EventBypass records a preparation that skipped the cache (policy or a
	// recoverable cache-layer error, WBS 10.57/10.69).
	EventBypass EventType = "CACHE_BYPASS"
	// EventSuppressed records a negative-cache suppression (WBS 10.34).
	EventSuppressed EventType = "CACHE_SUPPRESSED"
)

// Event is one cache audit event. It carries identities, the cache key and
// a short reason — never prompt content and never secrets.
type Event struct {
	ID        uuid.UUID
	ProjectID uuid.UUID
	EntryID   *uuid.UUID
	CacheKey  string
	Layer     Layer
	Type      EventType

	StateKey   string
	WorkUnitID *uuid.UUID

	Reason string

	// Estimated savings of a hit (distinct from measured provider usage).
	AvoidedCompressionCalls int
	AvoidedTokens           int
	AvoidedLatency          time.Duration

	CreatedAt time.Time
}

// NewEvent builds an event.
func NewEvent(projectID uuid.UUID, typ EventType, layer Layer, key string, now time.Time) Event {
	return Event{ID: uuid.New(), ProjectID: projectID, Type: typ, Layer: layer, CacheKey: key, CreatedAt: now}
}

// Stats aggregates cache effectiveness (WBS 10.37/10.38/10.64). Savings are
// ESTIMATES (from the recorded cost of the original preparation), kept
// distinct from measured provider usage.
type Stats struct {
	Entries     int
	Valid       int
	Invalidated int
	Failed      int
	Building    int

	Lookups int
	Hits    int
	Misses  int
	HitRate float64

	CanonicalHits   int
	CompressionHits int
	WorkerHits      int
	FragmentHits    int

	Bypasses     int
	Suppressions int

	CompressionCallsAvoided int
	EstimatedTokensAvoided  int
	EstimatedLatencyAvoided time.Duration
}

// Aggregate computes stats from entries and events (pure, empty-safe).
// Hit rate = worker-layer hits / worker-layer lookups. Invalidated or
// failed entries never contribute hits (only HIT events do, and those are
// emitted only for dispatchable entries).
func Aggregate(entries []PromptCacheEntry, events []Event) Stats {
	var s Stats
	for _, e := range entries {
		s.Entries++
		switch e.Status {
		case StatusValid:
			s.Valid++
		case StatusInvalidated:
			s.Invalidated++
		case StatusFailed:
			s.Failed++
		case StatusBuilding:
			s.Building++
		}
	}
	for _, ev := range events {
		switch ev.Type {
		case EventLookup:
			if ev.Layer == LayerWorker {
				s.Lookups++
			}
		case EventMiss:
			if ev.Layer == LayerWorker {
				s.Misses++
			}
		case EventBypass:
			s.Bypasses++
		case EventSuppressed:
			s.Suppressions++
		case EventHit:
			switch ev.Layer {
			case LayerWorker:
				s.Hits++
				s.WorkerHits++
			case LayerCanonical:
				s.CanonicalHits++
			case LayerCompression:
				s.CompressionHits++
			case LayerFragment:
				s.FragmentHits++
			}
			s.CompressionCallsAvoided += ev.AvoidedCompressionCalls
			s.EstimatedTokensAvoided += ev.AvoidedTokens
			s.EstimatedLatencyAvoided += ev.AvoidedLatency
		}
	}
	if s.Lookups > 0 {
		s.HitRate = float64(s.Hits) / float64(s.Lookups)
	}
	return s
}
