package persistence

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// StatusEvent is one recorded lifecycle transition (WBS 3.7). It is the
// audit trail that lets an observer determine when project state changed.
type StatusEvent struct {
	ID         uuid.UUID
	ProjectID  uuid.UUID
	FromStatus string
	ToStatus   string
	Reason     string
	CreatedAt  time.Time
}

// StatusEventRepository persists lifecycle transition events (WBS 3.7).
type StatusEventRepository interface {
	// Record inserts one event. It must join the caller's transaction when
	// bound via WithTx so the event and the project update commit
	// atomically (T3.7.6).
	Record(ctx context.Context, e *StatusEvent) error
	// ListByProject returns the project's events in ascending created_at
	// (then id) order.
	ListByProject(ctx context.Context, projectID uuid.UUID) ([]*StatusEvent, error)
	WithTx(tx Transaction) StatusEventRepository
}
