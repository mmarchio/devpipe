package integration

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"overseer/internal/access"
	"overseer/internal/audit"
	"overseer/internal/dbrepo"
)

func p18AuditEvent(project *uuid.UUID, i int) *audit.Event {
	return &audit.Event{At: time.Now(), ProjectID: project, Actor: audit.Actor{ID: "admin", Name: "Admin", Type: access.PrincipalLocalAdmin},
		Action: access.ActionMaintenance, ResourceKind: "system", ResourceID: fmt.Sprint(i), Result: audit.ResultSucceeded,
		HTTPStatus: 200, RequestID: fmt.Sprintf("req-%d", i), Method: "POST", Path: "/x",
		Metadata: map[string]any{"n": i, "nested": map[string]any{"b": []any{1.5, "x"}, "a": true}}}
}

// p18ChainState verifies every chain and returns event id -> event_hash.
func p18ChainState(t *testing.T, repo *dbrepo.AuditRepository) map[uuid.UUID]string {
	t.Helper()
	ctx := context.Background()
	keys, err := repo.AuditChainKeys(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[uuid.UUID]string{}
	for _, k := range keys {
		rows, pending, err := repo.AuditChainRows(ctx, nil, k)
		if err != nil {
			t.Fatal(err)
		}
		if pending != 0 {
			t.Fatalf("chain %s: %d rows not linked", k, pending)
		}
		if res := audit.VerifyChain(k, rows); res.Status != audit.ChainValid {
			t.Fatalf("chain %s: %+v", k, res)
		}
		for _, r := range rows {
			out[r.Event.ID] = r.EventHash
		}
	}
	return out
}

// T18.105–T18.107 — rows written before the chain existed are linked once
// at startup (Decision 7); new events link to them; every hash is stable
// across restarts and re-verifies from the stored data alone.
func TestT18105AuditChainStableAcrossRestart(t *testing.T) {
	base := baseDSN()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Skipf("test PG unreachable (%v)", err)
	}
	dbName := fmt.Sprintf("overseer_e2e_%d", time.Now().UnixNano())
	if _, err := c.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		c.Close(ctx)
		t.Skipf("create db: %v", err)
	}
	c.Close(ctx)
	t.Cleanup(func() { dropDB(t, base, dbName) })

	h := NewHarnessOn(t, base, dbName)
	project := uuid.New()
	// Legacy rows, as migration 00120 leaves them: chain_key only (the
	// insert-time link requirement is bypassed to create them).
	if _, err := h.DBExec(ctx, "ALTER TABLE audit_events DISABLE TRIGGER audit_events_require_link"); err != nil {
		t.Fatal(err)
	}
	for i, pid := range []string{"'" + project.String() + "'", "NULL", "'" + project.String() + "'"} {
		if _, err := h.DBExec(ctx, fmt.Sprintf(`INSERT INTO audit_events (id, created_at, project_id, actor_id, actor_name, actor_type,
			action, resource_kind, result, request_id, metadata, chain_key)
			VALUES (gen_random_uuid(), now(), %s, 'legacy', 'Legacy', 'SYSTEM', 'MAINTENANCE', 'system', 'SUCCEEDED', 'legacy-%d',
			'{"k": [1, 2.5, "z"], "a": {"y": null}}', CASE WHEN %s IS NULL THEN 'system' ELSE 'project:' || %s END)`, pid, i, pid, pid)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.DBExec(ctx, "ALTER TABLE audit_events ENABLE TRIGGER audit_events_require_link"); err != nil {
		t.Fatal(err)
	}
	h.Close()

	h = NewHarnessOn(t, base, dbName)
	repo := dbrepo.NewAuditRepository(h.App.DB().Raw())
	first := p18ChainState(t, repo)
	if len(first) != 3 {
		t.Fatalf("startup linked %d legacy rows, want 3", len(first))
	}
	for i := 0; i < 3; i++ {
		if err := repo.AppendAudit(ctx, nil, p18AuditEvent(&project, i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.AppendAudit(ctx, nil, p18AuditEvent(nil, 9)); err != nil {
		t.Fatal(err)
	}
	second := p18ChainState(t, repo)
	var n int
	_ = h.DBScan(ctx, `SELECT max(chain_seq) FROM audit_events WHERE chain_key = 'project:`+project.String()+`'`, &n)
	if len(second) != 7 || n != 5 {
		t.Fatalf("events %d, project chain length %d; want 7 / 5", len(second), n)
	}
	h.Close()

	h = NewHarnessOn(t, base, dbName)
	third := p18ChainState(t, dbrepo.NewAuditRepository(h.App.DB().Raw()))
	for id, hash := range second {
		if third[id] != hash {
			t.Fatalf("event %s hash changed across restart: %s -> %s", id, hash, third[id])
		}
	}
	for id, hash := range first {
		if second[id] != hash {
			t.Fatalf("legacy event %s re-linked: %s -> %s", id, hash, second[id])
		}
	}

	// Rows stay immutable once linked; a not-yet-linked row may only gain
	// its chain columns.
	for _, q := range []string{
		`UPDATE audit_events SET reason = 'tampered' WHERE chain_seq = 2`,
		`UPDATE audit_events SET event_hash = repeat('0', 64) WHERE chain_seq = 2`,
		`DELETE FROM audit_events WHERE chain_seq = 1`,
	} {
		if _, err := h.DBExec(ctx, q); err == nil {
			t.Fatalf("history mutated: %s", q)
		}
	}
	if _, err := h.DBExec(ctx, `INSERT INTO audit_events (id, created_at, actor_id, actor_name, actor_type, action, resource_kind,
		result, request_id, chain_key) VALUES (gen_random_uuid(), now(), 'l', 'L', 'SYSTEM', 'MAINTENANCE', 'system', 'SUCCEEDED', 'l', 'system')`); err == nil {
		t.Fatal("an unlinked row was inserted")
	}
	if _, err := h.DBExec(ctx, `ALTER TABLE audit_events DISABLE TRIGGER audit_events_require_link;
		INSERT INTO audit_events (id, created_at, actor_id, actor_name, actor_type, action, resource_kind,
		result, request_id, chain_key) VALUES (gen_random_uuid(), now(), 'l', 'L', 'SYSTEM', 'MAINTENANCE', 'system', 'SUCCEEDED', 'l', 'system');
		ALTER TABLE audit_events ENABLE TRIGGER audit_events_require_link`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DBExec(ctx, `UPDATE audit_events SET reason = 'x', chain_seq = 99, prev_hash = repeat('a', 64), event_hash = repeat('b', 64)
		WHERE event_hash IS NULL`); err == nil {
		t.Fatal("linking an unchained row also changed its content")
	}
}

// T18.106 — concurrent appends to the same chains keep chain_seq gap-free
// and every link valid.
func TestT18106ConcurrentAuditAppendsGapFree(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	repo := dbrepo.NewAuditRepository(h.App.DB().Raw())
	project := uuid.New()
	const workers, each = 8, 10
	var wg sync.WaitGroup
	errs := make(chan error, workers*each)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				pid := &project
				if i%2 == 1 {
					pid = nil
				}
				if err := repo.AppendAudit(ctx, nil, p18AuditEvent(pid, w*100+i)); err != nil {
					errs <- err
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	p18ChainState(t, repo)
	var n, max int
	_ = h.DBScan(ctx, `SELECT count(*), max(chain_seq) FROM audit_events WHERE chain_key = 'project:`+project.String()+`'`, &n, &max)
	if n != workers*each/2 || max != n {
		t.Fatalf("project chain: %d events, max seq %d", n, max)
	}
}
