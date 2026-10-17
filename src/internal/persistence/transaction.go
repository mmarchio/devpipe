package persistence

import "context"

// Transaction is the domain-level transaction surface (WBS 2.21).
// Application code begins a transaction through a provider that exposes
// only this interface, so no database driver type ever crosses the
// repository boundary: repositories execute the same operations against
// either a pooled connection or a transaction, and the caller commits or
// rolls back via Commit/Rollback.
type Transaction interface {
	Executor
	Commit(ctx context.Context) error
	Rollback(ctx context.Context) error
}

// CommandTag reports the outcome of an executed statement without
// exposing the driver's type.
type CommandTag interface {
	RowsAffected() int64
}

// QueryRowScanner is the row-scanning half of a single-row query,
// deliberately minimal so both pooled connections and transactions can
// satisfy it without driver-specific types in this package.
type QueryRowScanner interface {
	Scan(dest ...any) error
}

// Rows is the domain-level cursor over a multi-row query result (WBS
// 2.21). It is satisfied by driver row types, so no pgx type appears in
// this package.
type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Close()
	Err() error
}

// Executor is the driver-free execution surface repositories program
// against: a pooled connection and a transaction both satisfy it, so the
// same repository code runs inside or outside transaction boundaries
// (WBS 2.21).
type Executor interface {
	Exec(ctx context.Context, sql string, args ...any) (CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) QueryRowScanner
}

// TxBeginner is implemented by executors that can start a transaction.
// Nested transaction boundaries (e.g. a phase append executed inside an
// outer transaction) use it so no driver type is exposed (WBS 2.21).
type TxBeginner interface {
	Begin(ctx context.Context) (Transaction, error)
}
