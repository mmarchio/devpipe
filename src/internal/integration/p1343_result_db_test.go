package integration

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/workerresult"
)

// P13-21..P13-32: schema guards and repository behaviour.
func TestP1343ResultSchemaGuards(t *testing.T) {
	r := p13NewRig(t)
	ctx := context.Background()
	d := r.report(t, p13Incomplete)
	res := r.result(t, d)
	rid := res.ID.String()
	// Immutable result and children (T13.31.3 / T13.47.3).
	for _, q := range []string{"UPDATE worker_results SET status = 'complete' WHERE id = '" + rid + "'",
		"UPDATE worker_result_items SET text = 'x' WHERE result_id = '" + rid + "'",
		"UPDATE worker_result_parse_attempts SET outcome = 'ACCEPTED' WHERE dispatch_id = '" + d.ID.String() + "'"} {
		if _, err := r.h.DBExec(ctx, q); err == nil {
			t.Fatalf("mutable: %s", q)
		}
	}
	// One accepted result per dispatch (T13.43.3) and binding enforced by
	// the database (T13.43.2 / T13.50.2).
	repo := dbrepo.NewWorkerResultRepository(r.h.App.DB().Raw())
	clone := res.Result
	stored, created, err := repo.CreateAccepted(ctx, uuid.New(), &clone, time.Now())
	if err != nil || created || stored.ID != res.ID {
		t.Fatal("second accepted result")
	}
	r.h.DBExec(ctx, "DELETE FROM worker_results WHERE id = '"+rid+"'")
	bad := res.Result
	bad.Metadata.RoleKey = "PROJECT_MANAGER"
	if _, _, err := repo.CreateAccepted(ctx, uuid.New(), &bad, time.Now()); err == nil {
		t.Fatal("result rebound to another role")
	}
	// Cascade: deleting the result removed its children (T13.44.3).
	if n := p9Count(t, r.h, "worker_result_items", "result_id = '"+rid+"'"); n != 0 {
		t.Fatal("orphan items")
	}
	// Settings defaults / persistence (T13.96.3 DB half).
	sr := dbrepo.NewResultProtocolSettingsRepository(r.h.App.DB().Raw())
	if s, stored, _ := sr.Get(ctx, r.pos.ProjectID); stored || s.ParseMode != workerresult.ModeStrict {
		t.Fatal("defaults")
	}
	st := workerresult.DefaultSettings()
	st.Limits.MaxBlocking = 7
	sr.Upsert(ctx, r.pos.ProjectID, st)
	if s, stored, _ := sr.Get(ctx, r.pos.ProjectID); !stored || s.Limits.MaxBlocking != 7 {
		t.Fatal("settings persisted")
	}
	// Dispatch model recorded for compliance grouping (P13-28).
	if d.WorkerModel != "hermes-worker" {
		t.Fatalf("worker model %q", d.WorkerModel)
	}
}
