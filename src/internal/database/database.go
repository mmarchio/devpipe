package database

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"overseer/internal/logging"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Connector is the domain-facing contract for database connectivity.
// It deliberately exposes only context, bool, string, and error so that
// domain packages never see PostgreSQL driver types (T1.4.6).
type Connector interface {
	Ping(ctx context.Context) (bool, error)
	Close()
}

// Pool wraps the pgx connection pool.
type Pool struct {
	*pgxpool.Pool
	counter *queryCounter
}

// queryCounter counts executed statements (an atomic add per query); the
// web read-model tests use it to prove bounded query counts (WBS
// 17.158–17.160).
type queryCounter struct {
	n atomic.Int64
	// lostAt (unix nanos) is when a query last failed because its
	// connection was lost (WBS 18.37).
	lostAt atomic.Int64
}

func (q *queryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	q.n.Add(1)
	if c, ok := ctx.Value(counterKey{}).(*atomic.Int64); ok {
		c.Add(1)
	}
	return ctx
}

type counterKey struct{}

// WithQueryCounter returns a context whose statements are counted into c
// (bounded-query tests of the web read models).
func WithQueryCounter(ctx context.Context, c *atomic.Int64) context.Context {
	return context.WithValue(ctx, counterKey{}, c)
}

func (q *queryCounter) TraceQueryEnd(_ context.Context, _ *pgx.Conn, d pgx.TraceQueryEndData) {
	if d.Err != nil && connectionLost(d.Err) {
		q.lostAt.Store(time.Now().UnixNano())
	}
}

// shouldPing pings a pooled connection before reuse when it has been idle
// over a second (the pgx default) or went idle before the most recent
// observed connection loss — after a database restart every such
// connection is likely dead, and a failed ping replaces it instead of
// failing the caller's first statement (WBS 18.37).
func (q *queryCounter) shouldPing(_ context.Context, p pgxpool.ShouldPingParams) bool {
	if p.IdleDuration > time.Second {
		return true
	}
	lost := q.lostAt.Load()
	return lost != 0 && time.Now().Add(-p.IdleDuration).UnixNano() <= lost
}

// connectionLost reports a failure of the connection itself (not a SQL
// error reported by the server).
func connectionLost(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var ne net.Error
	return pgconn.SafeToRetry(err) || errors.As(err, &ne) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed)
}

// QueryCount returns the number of statements executed through the pool.
func (p *Pool) QueryCount() int64 {
	if p == nil || p.counter == nil {
		return 0
	}
	return p.counter.n.Load()
}

var _ Connector = (*Pool)(nil)

// New opens the pool, configures it, and verifies real connectivity with a
// ping. Any failure returns an error and no pool (T1.4.3/T1.4.4).
func New(ctx context.Context, dsn string, lg *logging.Logger) (*Pool, error) {
	return NewWithOptions(ctx, dsn, lg, PoolOptions{})
}

// PoolOptions sizes the pool explicitly (WBS 18.91); zero values select
// the defaults (max 10 connections, lifetime 1 h, idle 15 min).
type PoolOptions struct {
	MaxConns        int
	MinConns        int
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
	ApplicationName string
}

// NewWithOptions opens a pool with explicit sizing.
func NewWithOptions(ctx context.Context, dsn string, lg *logging.Logger, o PoolOptions) (*Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("database: parse DSN: %w", err)
	}
	cfg.MaxConns = 10
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 15 * time.Minute
	if o.MaxConns > 0 {
		cfg.MaxConns = int32(o.MaxConns)
	}
	if o.MinConns > 0 {
		cfg.MinConns = int32(o.MinConns)
	}
	if o.MaxConnLifetime > 0 {
		cfg.MaxConnLifetime = o.MaxConnLifetime
	}
	if o.MaxConnIdleTime > 0 {
		cfg.MaxConnIdleTime = o.MaxConnIdleTime
	}
	if o.ApplicationName != "" {
		cfg.ConnConfig.RuntimeParams["application_name"] = o.ApplicationName
	}
	cfg.HealthCheckPeriod = time.Minute
	counter := &queryCounter{}
	cfg.ConnConfig.Tracer = counter
	cfg.ShouldPing = counter.shouldPing

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("database: open pool: %w", err)
	}

	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pctx); err != nil {
		pool.Close()
		if lg != nil {
			lg.WithComponent("database").Error("database unreachable", "error", err.Error())
		}
		return nil, fmt.Errorf("database: ping: %w", err)
	}
	return &Pool{Pool: pool, counter: counter}, nil
}

// Ping verifies actual connectivity; it does not create connections on a
// closed pool (T1.4.4).
func (p *Pool) Ping(ctx context.Context) (bool, error) {
	if p.Pool == nil {
		return false, errors.New("database: pool closed")
	}
	if err := p.Pool.Ping(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// Close releases pool resources. Safe to call more than once (T1.4.5).
func (p *Pool) Close() {
	if p.Pool == nil {
		return
	}
	p.Pool.Close()
}

// Raw exposes the underlying pool for the migration framework. Domain code
// must not call this; it lives in the infrastructure layer.
func (p *Pool) Raw() *pgxpool.Pool { return p.Pool }

// rawPing is a driver-level probe used by tests to decide skip vs run.
func rawPing(ctx context.Context, dsn string) error {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	return pool.Ping(ctx)
}
