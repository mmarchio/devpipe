package worker

import "context"

// Connector is implemented by workers with an explicit transport
// connection (WBS 12.5). Callers connect before declaring the worker
// usable.
type Connector interface {
	Connect(ctx context.Context) error
	ConnectionState() ConnectionState
}

// Reconciler is implemented by workers that can restore transport and
// check a persisted session (WBS 12.40-12.42). Neither operation creates a
// session or replays a request. A CONFIRMED reconciliation adopts the
// session as the worker's current session.
type Reconciler interface {
	Reconnect(ctx context.Context) error
	ReconcileSession(ctx context.Context, persisted SessionInfo) (SessionReconciliation, error)
}

// ConnectionObserver receives transport state changes (audit, WBS 12.53).
type ConnectionObserver func(from, to ConnectionState, err error)

// Observable is implemented by workers reporting transport changes.
type Observable interface {
	OnConnectionChange(ConnectionObserver)
}
