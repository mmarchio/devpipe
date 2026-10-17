package dbrepo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/faults"
	"overseer/internal/persistence"
	"overseer/internal/resilience"
)

// Compile-time check that the transaction surface can begin nested
// transactions.
var _ persistence.TxBeginner = (*Tx)(nil)

// executor is the driver-free surface repositories program against: both
// the pool adapter and the transaction adapter satisfy it, so
// repositories are usable identically inside or outside transaction
// boundaries (WBS 2.21) without pgx types leaking into the domain.
type executor = persistence.Executor

// Tx wraps a PostgreSQL transaction and adapts it to the domain-level
// persistence.Transaction surface.
type Tx struct {
	tx pgx.Tx
}

// BeginTx starts a new transaction on the pool and returns the domain
// surface for it (T2.21.1).
func BeginTx(ctx context.Context, pool *pgxpool.Pool) (*Tx, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, TranslateDBError("begin transaction", err)
	}
	return &Tx{tx: tx}, nil
}

// Commit makes the transaction durable (T2.21.1).
func (t *Tx) Commit(ctx context.Context) error {
	if err := t.tx.Commit(ctx); err != nil {
		return TranslateDBError("commit transaction", err)
	}
	return nil
}

// Rollback discards the transaction (T2.21.2). Rolling back an
// already-committed transaction is a driver-level no-op, so a deferred
// Rollback after Commit is safe.
func (t *Tx) Rollback(ctx context.Context) error {
	if err := t.tx.Rollback(ctx); err != nil {
		return TranslateDBError("rollback transaction", err)
	}
	return nil
}

// Exec runs a statement on the transaction (persistence.Transaction).
func (t *Tx) Exec(ctx context.Context, sql string, args ...any) (persistence.CommandTag, error) {
	tag, err := t.tx.Exec(ctx, sql, args...)
	if err != nil {
		return nil, TranslateDBError("exec on transaction", err)
	}
	return tag, nil
}

// Query runs a multi-row query on the transaction; the driver cursor is
// wrapped in the domain-level Rows surface.
func (t *Tx) Query(ctx context.Context, sql string, args ...any) (persistence.Rows, error) {
	rows, err := t.tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, TranslateDBError("query on transaction", err)
	}
	return rowsAdapter{rows}, nil
}

// QueryRow starts a single-row query on the transaction (persistence.
// Transaction). The row is bound to the transaction, so it must be
// scanned before Commit/Rollback.
func (t *Tx) QueryRow(ctx context.Context, sql string, args ...any) persistence.QueryRowScanner {
	return t.tx.QueryRow(ctx, sql, args...)
}

// Begin starts a new database transaction for nested transactional
// operations executed on this transaction's executor (WBS 2.21); it
// reuses the pool behind the transaction.
func (t *Tx) Begin(ctx context.Context) (persistence.Transaction, error) {
	tx, err := t.tx.Begin(ctx)
	if err != nil {
		return nil, TranslateDBError("begin nested transaction", err)
	}
	return &Tx{tx: tx}, nil
}

// Run executes fn inside a transaction: it commits on a nil result,
// rolls back on an error, and also rolls back if fn panics (T2.21.2,
// T2.21.3) — a panicking body never leaves a transaction open and the
// panic is re-raised unchanged.
//
// Transient failures re-run the whole transaction under the db_tx retry
// policy (WBS 18.35 / 18.36): serialization failures and deadlocks, and
// connection loss before COMMIT was sent (the server rolled the
// transaction back). fn must therefore be safe to re-run — its database
// work is rolled back, and it must not accumulate state outside the
// transaction across attempts. A failure while COMMIT may already have
// been applied is AMBIGUOUS and never retried; it is reconciled through
// idempotency keys / unique constraints instead (WBS 18.190).
func Run(ctx context.Context, pool *pgxpool.Pool, fn func(persistence.Transaction) error) error {
	return RunWithReset(ctx, pool, nil, fn)
}

// RunWithReset is Run where reset (if non-nil) runs before every retry,
// restoring in-memory state fn mutates.
func RunWithReset(ctx context.Context, pool *pgxpool.Pool, reset func(), fn func(persistence.Transaction) error) error {
	_, err := retryRegistry().Do(ctx, resilience.PolicyDBTx, func(ctx context.Context, attempt int) error {
		if attempt > 1 && reset != nil {
			reset()
		}
		return runOnce(ctx, pool, fn)
	})
	return err
}

var txRetry atomic.Pointer[resilience.Registry]

// SetRetryRegistry replaces the retry registry transactions use (nil
// restores the default).
func SetRetryRegistry(r *resilience.Registry) {
	txRetry.Store(r)
}

var defaultRetry = sync.OnceValue(resilience.MustDefault)

func retryRegistry() *resilience.Registry {
	if r := txRetry.Load(); r != nil {
		return r
	}
	return defaultRetry()
}

func runOnce(ctx context.Context, pool *pgxpool.Pool, fn func(persistence.Transaction) error) (err error) {
	// Fault point (WBS 18.141): no connection could be obtained — a
	// transient failure retried under the db_tx policy.
	if ferr := faults.Hit(ctx, faults.DBConnect); ferr != nil {
		if errors.Is(ferr, faults.ErrCrash) {
			return ferr
		}
		return resilience.Mark(fmt.Errorf("database connect: %w", ferr), resilience.Transient)
	}
	t, berr := BeginTx(ctx, pool)
	if berr != nil {
		return classifyTxError(txBegin, berr)
	}
	defer func() {
		if r := recover(); r != nil {
			_ = t.Rollback(ctx)
			err = fmt.Errorf("dbrepo: transaction panic: %v", r)
			panic(r)
		}
		if err != nil {
			_ = t.Rollback(ctx)
		}
	}()
	if err = fn(t); err != nil {
		return classifyTxError(txBody, err)
	}
	if err = faults.Hit(ctx, faults.BeforeDBCommit); err != nil {
		return classifyTxError(txBody, err)
	}
	if err = t.Commit(ctx); err != nil {
		return classifyTxError(txCommit, err)
	}
	if ferr := faults.Hit(ctx, faults.AfterDBCommit); ferr != nil {
		// The transaction is durable: whatever the injected failure is,
		// re-running it would apply the work twice.
		return resilience.Mark(ferr, resilience.Ambiguous)
	}
	return nil
}

// txStage is where a transaction error arose.
type txStage int

const (
	txBegin txStage = iota
	txBody
	txCommit
)

// classifyTxError marks connection loss by stage (WBS 18.35): before
// COMMIT the server discards the transaction, so a lost connection is
// TRANSIENT; during COMMIT it is AMBIGUOUS unless the driver proves the
// COMMIT was never sent. Serialization / deadlock failures are TRANSIENT
// at any stage (PostgreSQL reports them only for a rolled-back
// transaction); everything else keeps its own classification.
func classifyTxError(stage txStage, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return err // SQLSTATE drives the classification
	}
	if !connectionLost(err) {
		return err
	}
	if stage == txCommit && !pgconn.SafeToRetry(err) {
		return resilience.Mark(err, resilience.Ambiguous)
	}
	return resilience.Mark(err, resilience.Transient)
}

// connectionLost reports a driver / network failure of the connection
// itself (not a server-side SQL error).
func connectionLost(err error) bool {
	if pgconn.SafeToRetry(err) || pgconn.Timeout(err) {
		return true
	}
	var ne net.Error
	var ce *pgconn.ConnectError
	return errors.As(err, &ne) || errors.As(err, &ce) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed)
}

// rowsAdapter exposes a driver cursor through the domain-level Rows
// surface.
type rowsAdapter struct {
	rows pgx.Rows
}

func (a rowsAdapter) Next() bool             { return a.rows.Next() }
func (a rowsAdapter) Scan(dest ...any) error { return a.rows.Scan(dest...) }
func (a rowsAdapter) Close()                 { a.rows.Close() }
func (a rowsAdapter) Err() error             { return a.rows.Err() }

// poolAdapter exposes the pool through the same driver-free surface as a
// transaction, so repositories cannot tell the two apart.
type poolAdapter struct {
	pool *pgxpool.Pool
}

func (a poolAdapter) Exec(ctx context.Context, sql string, args ...any) (persistence.CommandTag, error) {
	return a.pool.Exec(ctx, sql, args...)
}

func (a poolAdapter) Query(ctx context.Context, sql string, args ...any) (persistence.Rows, error) {
	rows, err := a.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return rowsAdapter{rows}, nil
}

// Begin starts a transaction on the pool (persistence.TxBeginner).
func (a poolAdapter) Begin(ctx context.Context) (persistence.Transaction, error) {
	return BeginTx(ctx, a.pool)
}

func (a poolAdapter) QueryRow(ctx context.Context, sql string, args ...any) persistence.QueryRowScanner {
	return a.pool.QueryRow(ctx, sql, args...)
}

// txExecutor adapts the domain-level transaction surface to the private
// executor interface, so a repository WithTx can run on a transaction
// without ever seeing a driver type.
type txExecutor struct {
	tx persistence.Transaction
}

func (e txExecutor) Exec(ctx context.Context, sql string, args ...any) (persistence.CommandTag, error) {
	return e.tx.Exec(ctx, sql, args...)
}

func (e txExecutor) Query(ctx context.Context, sql string, args ...any) (persistence.Rows, error) {
	return e.tx.Query(ctx, sql, args...)
}

func (e txExecutor) QueryRow(ctx context.Context, sql string, args ...any) persistence.QueryRowScanner {
	return e.tx.QueryRow(ctx, sql, args...)
}
