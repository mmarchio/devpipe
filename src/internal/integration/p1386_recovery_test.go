package integration

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"overseer/internal/persistence"
	"overseer/internal/service"
	"overseer/internal/workerresult"
)

// P13-51 (WBS 13.86, 13.87): deterministic failure, persistence failure
// after a successful parse.
func TestP1386RetryAndPersistenceFailure(t *testing.T) {
	r := p13NewRig(t)
	ctx := context.Background()
	// 13.86: the same invalid response fails identically every time.
	d := r.report(t, p13Prose)
	o1, err1 := r.rs().Reparse(ctx, r.pos.ProjectID, d.ID)
	o2, err2 := r.rs().Reparse(ctx, r.pos.ProjectID, d.ID)
	if err1 != nil || err2 != nil || o1.Err.Code != o2.Err.Code || fmt.Sprint(o1.Err.Errors) != fmt.Sprint(o2.Err.Errors) {
		t.Fatal("T13.86.1")
	}
	if n := len(r.attempts(t, d)); n != 3 || len(r.g.Messages()) != 1 {
		t.Fatalf("T13.86.2/T13.86.3 attempts %d messages %d", n, len(r.g.Messages()))
	}

	// 13.87: the result cannot be stored → no attempt, no result, dispatch
	// stays COMPLETED with its raw response; recovery replays the parse.
	if _, err := r.h.DBExec(ctx, `CREATE OR REPLACE FUNCTION p13_fail() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'injected'; END; $$ LANGUAGE plpgsql;
		CREATE TRIGGER p13_fail BEFORE INSERT ON worker_results FOR EACH ROW EXECUTE FUNCTION p13_fail();`); err != nil {
		t.Fatal(err)
	}
	dc := r.report(t, p13Complete)
	if len(r.attempts(t, dc)) != 0 {
		t.Fatal("persistence failure recorded as a parse outcome")
	}
	if _, err := r.rs().ResultByDispatch(ctx, r.pos.ProjectID, dc.ID); err == nil {
		t.Fatal("partial result persisted")
	}
	r.h.DBExec(ctx, "DROP TRIGGER p13_fail ON worker_results")
	sends := len(r.g.Messages())
	if n, err := r.rs().RecoverUnparsed(ctx); err != nil || n != 1 {
		t.Fatalf("T13.87.2 recovery parsed %d (%v)", n, err)
	}
	if r.result(t, dc).Result.Status != workerresult.StatusComplete || len(r.g.Messages()) != sends {
		t.Fatal("T13.87.1 worker called again")
	}
	if n, _ := r.rs().RecoverUnparsed(ctx); n != 0 || p9Count(t, r.h, "worker_results", "dispatch_id = '"+dc.ID.String()+"'") != 1 {
		t.Fatal("T13.87.3 duplicate accepted result")
	}
}

// P13-51 (WBS 13.88-13.90): restart with unparsed, parsed and rejected
// dispatches.
func TestP1388RestartRecovery(t *testing.T) {
	rr, boot := p12RestartRig(t)
	r := p13From(t, rr, "/repos/a")
	ctx := context.Background()
	dParsed := r.report(t, p13Incomplete)
	dRejected := r.report(t, p13Prose)
	dUnparsed := r.report(t, p13Blocked)
	// Crash before the third dispatch's parse was persisted.
	r.h.DBExec(ctx, "DELETE FROM worker_result_parse_attempts WHERE dispatch_id = '"+dUnparsed.ID.String()+"'")
	r.h.DBExec(ctx, "DELETE FROM worker_results WHERE dispatch_id = '"+dUnparsed.ID.String()+"'")
	hash := r.result(t, dParsed).Result.NormalizedHash
	sends := len(r.g.Messages())
	before := r.boundary(t)

	r.h.Close()
	r.h = boot()
	if res := r.result(t, dUnparsed); res.Result.Status != workerresult.StatusBlocked || len(r.attempts(t, dUnparsed)) != 1 {
		t.Fatal("13.88 unparsed dispatch not parsed from the durable response")
	}
	if res := r.result(t, dParsed); res.Result.NormalizedHash != hash || len(r.attempts(t, dParsed)) != 1 || res.Result.Status != workerresult.StatusIncomplete {
		t.Fatal("13.89 parsed result not reconstructed unchanged")
	}
	if c, _ := r.result(t, dParsed).Result.Continuation(); len(c.Remaining) != 2 {
		t.Fatal("items not reconstructed")
	}
	if a := r.attempts(t, dRejected); len(a) != 1 || a[0].Outcome != workerresult.OutcomeRejected {
		t.Fatal("13.90 rejection reinterpreted")
	}
	if len(r.g.Messages()) != sends || r.boundary(t) != before {
		t.Fatal("restart contacted the worker or changed state")
	}
}

// P13-52 (WBS 13.91): two instances parse the same dispatch concurrently.
func TestP1391ConcurrentParsers(t *testing.T) {
	rr, boot := p12RestartRig(t)
	r := p13From(t, rr, "/repos/a")
	ctx := context.Background()
	hB := boot()
	resB := hB.App.Results()
	for round := 0; round < 5; round++ {
		d := r.report(t, p13Incomplete)
		r.h.DBExec(ctx, "DELETE FROM worker_result_parse_attempts WHERE dispatch_id = '"+d.ID.String()+"'")
		r.h.DBExec(ctx, "DELETE FROM worker_results WHERE dispatch_id = '"+d.ID.String()+"'")
		var wg sync.WaitGroup
		outs := make([]*service.ParseOutcome, 2)
		for i, svc := range []*service.ResultService{r.rs(), resB} {
			wg.Add(1)
			go func(i int, svc *service.ResultService) {
				defer wg.Done()
				o, err := svc.ParseDispatch(ctx, r.pos.ProjectID, d.ID)
				if err != nil {
					t.Errorf("parse: %v", err)
				}
				outs[i] = o
			}(i, svc)
		}
		wg.Wait()
		if outs[0] == nil || outs[1] == nil || outs[0].Result.ID != outs[1].Result.ID || outs[0].Created == outs[1].Created {
			t.Fatal("13.91 not exactly one authoritative result observed by both")
		}
		if outs[0].Result.Result.NormalizedHash != outs[1].Result.Result.NormalizedHash {
			t.Fatal("13.91 inconsistent hashes")
		}
		if p9Count(t, r.h, "worker_results", "dispatch_id = '"+d.ID.String()+"'") != 1 ||
			p9Count(t, r.h, "worker_result_items", "result_id = '"+outs[0].Result.ID.String()+"'") != 4 {
			t.Fatal("13.91 duplicate result or children")
		}
	}
}

// P13-53 (WBS 13.92): a stored 1.0 result and response remain readable and
// reparseable after the parser configuration moves on (compatible minor).
func TestP1392Compatibility(t *testing.T) {
	r := p13NewRig(t)
	ctx := context.Background()
	d := r.report(t, p13Complete)
	orig := r.result(t, d)
	st := workerresult.DefaultSettings()
	st.ProtocolVersion = workerresult.ProtocolVersion{Major: 1, Minor: 1}
	st.ParseMode = workerresult.ModeTolerant
	if _, err := r.rs().UpdateProtocolSettings(ctx, r.pos.ProjectID, st); err != nil {
		t.Fatal(err)
	}
	out, err := r.rs().Reparse(ctx, r.pos.ProjectID, d.ID)
	if err != nil || out.Attempt.Outcome != workerresult.OutcomeDuplicate || out.Attempt.DiffersFromAccepted || out.Result.ID != orig.ID {
		t.Fatalf("13.92 reparse %+v %v", out, err)
	}
	if got := r.result(t, d); got.Result.ProtocolVersion != workerresult.ProtocolV1_0 || got.Result.NormalizedHash != orig.Result.NormalizedHash {
		t.Fatal("historical result changed")
	}
	st.ProtocolVersion = workerresult.ProtocolVersion{Major: 2}
	if _, err := r.rs().UpdateProtocolSettings(ctx, r.pos.ProjectID, st); err == nil {
		t.Fatal("unsupported protocol configured")
	}
	list, _, _ := r.rs().Results(ctx, persistence.WorkerResultFilter{ProjectID: r.pos.ProjectID})
	if len(list) != 1 {
		t.Fatal("history")
	}
}
