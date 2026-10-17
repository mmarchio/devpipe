package dbrepo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"overseer/internal/audit"
	"overseer/internal/persistence"
)

// Audit hash chain (P18-56, WBS 18.105–18.107). Appends to one chain are
// serialized by a transaction-scoped advisory lock on the chain key, so
// chain_seq is gap-free and every row links to its predecessor's hash.
// Rows stored before the chain existed (chain_key only) are linked first,
// in seq order, by whichever transaction next holds the chain's lock — the
// startup backfill (ChainPending) or the next append (Decision 7).

// chainBackfillBatch bounds one backfill transaction.
const chainBackfillBatch = 1000

func lockAuditChain(ctx context.Context, ex executor, chainKey string) error {
	_, err := ex.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('overseer-audit-chain:' || $1, 0))`, chainKey)
	return tr("lock audit chain", err)
}

// auditChainTail returns the last linked position of a chain (0 and the
// genesis hash when nothing is linked yet).
func auditChainTail(ctx context.Context, ex executor, chainKey string) (int64, string, error) {
	var seq int64
	var hash string
	err := ex.QueryRow(ctx, `SELECT chain_seq, event_hash FROM audit_events
		WHERE chain_key = $1 AND event_hash IS NOT NULL ORDER BY chain_seq DESC LIMIT 1`, chainKey).Scan(&seq, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, audit.Genesis(chainKey), nil
	}
	if err != nil {
		return 0, "", tr("read audit chain tail", err)
	}
	return seq, hash, nil
}

// linkPendingAudit links up to limit (0 = all) not-yet-chained rows of a
// chain, in seq order. The caller holds the chain lock. It returns how many
// rows it linked.
func linkPendingAudit(ctx context.Context, ex executor, chainKey string, limit int) (int, error) {
	q := `SELECT ` + auditColumns + ` FROM audit_events WHERE chain_key = $1 AND event_hash IS NULL ORDER BY seq`
	args := []any{chainKey}
	if limit > 0 {
		q += ` LIMIT $2`
		args = append(args, limit)
	}
	rows, err := ex.Query(ctx, q, args...)
	if err != nil {
		return 0, tr("list unchained audit events", err)
	}
	var pending []*audit.Event
	for rows.Next() {
		e, err := scanAudit(rows)
		if err != nil {
			rows.Close()
			return 0, tr("scan unchained audit event", err)
		}
		pending = append(pending, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, tr("list unchained audit events", err)
	}
	if len(pending) == 0 {
		return 0, nil
	}
	seq, prev, err := auditChainTail(ctx, ex, chainKey)
	if err != nil {
		return 0, err
	}
	for _, e := range pending {
		seq++
		hash, err := audit.ChainLink(chainKey, seq, prev, *e)
		if err != nil {
			return 0, err
		}
		tag, err := ex.Exec(ctx, `UPDATE audit_events SET chain_seq = $2, prev_hash = $3, event_hash = $4
			WHERE id = $1 AND event_hash IS NULL`, e.ID, seq, prev, hash)
		if err != nil {
			return 0, tr("link audit event", err)
		}
		if tag.RowsAffected() != 1 {
			return 0, fmt.Errorf("dbrepo: audit event %s was linked concurrently", e.ID)
		}
		prev = hash
	}
	return len(pending), nil
}

// appendChained inserts e as the next link of its chain inside ex (a
// transaction).
func (r *AuditRepository) appendChained(ctx context.Context, ex executor, e *audit.Event) error {
	key := audit.ChainKey(e.ProjectID)
	if err := lockAuditChain(ctx, ex, key); err != nil {
		return err
	}
	if _, err := linkPendingAudit(ctx, ex, key, 0); err != nil {
		return err
	}
	seq, prev, err := auditChainTail(ctx, ex, key)
	if err != nil {
		return err
	}
	seq++
	hash, err := audit.ChainLink(key, seq, prev, *e)
	if err != nil {
		return err
	}
	md := e.Metadata
	if md == nil {
		md = map[string]any{}
	}
	b, err := json.Marshal(md)
	if err != nil {
		return err
	}
	err = ex.QueryRow(ctx, `INSERT INTO audit_events (id, created_at, project_id, project_name, execution_id, phase_id, work_unit_id,
		actor_id, actor_name, actor_type, action, resource_kind, resource_id, result, http_status, reason, request_id, method, path, metadata,
		chain_key, chain_seq, prev_hash, event_hash)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24) RETURNING seq`,
		e.ID, e.At, e.ProjectID, e.ProjectName, e.ExecutionID, e.PhaseID, e.WorkUnitID, e.Actor.ID, e.Actor.Name,
		string(e.Actor.Type), string(e.Action), e.ResourceKind, e.ResourceID, string(e.Result), e.HTTPStatus, e.Reason,
		e.RequestID, e.Method, e.Path, b, key, seq, prev, hash).Scan(&e.Seq)
	return tr("insert audit event", err)
}

// ChainPending links every not-yet-chained audit row (rows written before
// the chain existed), chain by chain, in bounded transactions. It is run
// once at startup; appends link any remainder themselves. It returns the
// number of rows linked.
func (r *AuditRepository) ChainPending(ctx context.Context) (int, error) {
	if r.pool == nil {
		return 0, errors.New("dbrepo: audit chain backfill needs a pool")
	}
	rows, err := r.exec.Query(ctx, `SELECT DISTINCT chain_key FROM audit_events WHERE event_hash IS NULL ORDER BY 1`)
	if err != nil {
		return 0, tr("list unchained audit chains", err)
	}
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			rows.Close()
			return 0, tr("scan audit chain key", err)
		}
		keys = append(keys, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, tr("list unchained audit chains", err)
	}
	total := 0
	for _, key := range keys {
		for {
			var n int
			err := Run(ctx, r.pool, func(tx persistence.Transaction) error {
				if err := lockAuditChain(ctx, tx, key); err != nil {
					return err
				}
				var err error
				n, err = linkPendingAudit(ctx, tx, key, chainBackfillBatch)
				return err
			})
			if err != nil {
				return total, err
			}
			total += n
			if n < chainBackfillBatch {
				break
			}
		}
	}
	return total, nil
}

// AuditChainKeys lists every chain with at least one event (ex nil = the
// pool).
func (r *AuditRepository) AuditChainKeys(ctx context.Context, ex executor) ([]string, error) {
	if ex == nil {
		ex = r.exec
	}
	rows, err := ex.Query(ctx, `SELECT DISTINCT chain_key FROM audit_events ORDER BY 1`)
	if err != nil {
		return nil, tr("list audit chains", err)
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, tr("scan audit chain key", err)
		}
		keys = append(keys, k)
	}
	return keys, tr("list audit chains", rows.Err())
}

// AuditChainRows returns the linked rows of one chain in chain_seq order,
// plus the number of rows not linked yet (ex nil = the pool).
func (r *AuditRepository) AuditChainRows(ctx context.Context, ex executor, chainKey string) ([]audit.ChainRow, int, error) {
	if ex == nil {
		ex = r.exec
	}
	rows, err := ex.Query(ctx, `SELECT `+auditColumns+`, chain_seq, prev_hash, event_hash FROM audit_events
		WHERE chain_key = $1 AND event_hash IS NOT NULL ORDER BY chain_seq`, chainKey)
	if err != nil {
		return nil, 0, tr("list audit chain", err)
	}
	var out []audit.ChainRow
	for rows.Next() {
		var cr audit.ChainRow
		e, err := scanAudit(chainScanner{rows, &cr})
		if err != nil {
			rows.Close()
			return nil, 0, tr("scan audit chain row", err)
		}
		cr.Event = *e
		out = append(out, cr)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, tr("list audit chain", err)
	}
	var pending int
	if err := ex.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE chain_key = $1 AND event_hash IS NULL`, chainKey).Scan(&pending); err != nil {
		return nil, 0, tr("count unchained audit events", err)
	}
	return out, pending, nil
}

// chainScanner appends the chain columns to scanAudit's destinations.
type chainScanner struct {
	row interface{ Scan(...any) error }
	cr  *audit.ChainRow
}

func (s chainScanner) Scan(dest ...any) error {
	return s.row.Scan(append(dest, &s.cr.ChainSeq, &s.cr.PrevHash, &s.cr.EventHash)...)
}

// VerifyAuditChains verifies one chain (chainKey) or every chain ("") in a
// single read-only, repeatable-read snapshot (WBS 18.108–18.110): it
// writes nothing. Rows never linked to their chain make it INCOMPLETE.
func (r *AuditRepository) VerifyAuditChains(ctx context.Context, chainKey string) ([]audit.ChainResult, error) {
	if r.pool == nil {
		return nil, errors.New("dbrepo: audit verification needs a pool")
	}
	ptx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, TranslateDBError("begin audit verification", err)
	}
	tx := &Tx{tx: ptx}
	defer func() { _ = tx.Rollback(ctx) }()
	keys := []string{chainKey}
	if chainKey == "" {
		if keys, err = r.AuditChainKeys(ctx, tx); err != nil {
			return nil, err
		}
	}
	out := make([]audit.ChainResult, 0, len(keys))
	for _, k := range keys {
		rows, pending, err := r.AuditChainRows(ctx, tx, k)
		if err != nil {
			return nil, err
		}
		res := audit.VerifyChain(k, rows)
		if pending > 0 && res.Status == audit.ChainValid {
			res.Status, res.FailureReason = audit.ChainIncomplete, fmt.Sprintf("%d events not linked to the chain", pending)
			res.FirstFailing = int64(len(rows)) + 1
		}
		out = append(out, res)
	}
	return out, nil
}

// AuditFailureAcknowledged reports whether a failure fingerprint has been
// acknowledged.
func (r *AuditRepository) AuditFailureAcknowledged(ctx context.Context, fingerprint string) (bool, error) {
	var ok bool
	err := r.exec.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM audit_failure_acknowledgements WHERE fingerprint = $1)`, fingerprint).Scan(&ok)
	return ok, tr("read audit failure acknowledgement", err)
}

// AcknowledgeAuditFailure records an acknowledgement (idempotent per
// fingerprint: the first one is kept and returned).
func (r *AuditRepository) AcknowledgeAuditFailure(ctx context.Context, ack *audit.FailureAck) error {
	failure, err := json.Marshal(ack.Failure)
	if err != nil {
		return err
	}
	if _, err := r.exec.Exec(ctx, `INSERT INTO audit_failure_acknowledgements (id, fingerprint, status, failure, reason,
		acknowledged_by, acknowledged_by_name, request_id, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (fingerprint) DO NOTHING`, ack.ID, ack.Fingerprint, string(ack.Status), failure, ack.Reason, ack.By, ack.ByName,
		ack.RequestID, ack.At.UTC()); err != nil {
		return tr("insert audit failure acknowledgement", err)
	}
	var status string
	err = r.exec.QueryRow(ctx, `SELECT id, status, reason, acknowledged_by, acknowledged_by_name, request_id, created_at
		FROM audit_failure_acknowledgements WHERE fingerprint = $1`, ack.Fingerprint).Scan(&ack.ID, &status, &ack.Reason, &ack.By,
		&ack.ByName, &ack.RequestID, &ack.At)
	ack.Status = audit.VerifyStatus(status)
	return tr("read audit failure acknowledgement", err)
}
