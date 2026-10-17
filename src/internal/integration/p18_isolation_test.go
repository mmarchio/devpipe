package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"overseer/internal/audit"
	"overseer/internal/execution"
	"overseer/internal/readmodel"
)

// p18TwinProjects runs two projects with the same name, the same plan,
// the same unit keys and the same task text on one instance, to
// completion.
func p18TwinProjects(t *testing.T) (*p17Rig, *p16Rig) {
	t.Helper()
	a := p17NewRig(t)
	b := &p16Rig{p15Rig: p15Build(t, a.h, a.g, a.srv, []p15Phase{{1, "Foundation", "PENDING", 1}})}
	p16Configure(t, b, p16Options{})
	a.exec(t, "UPDATE projects SET name = 'Twin' WHERE id IN ('"+a.pid.String()+"', '"+b.pid.String()+"')")
	for _, r := range []*p16Rig{a.p16Rig, b} {
		r.queuePlan(r.planJSON(p16Unit{key: "shared"}, p16Unit{key: "next", deps: []string{"shared"}}))
		r.queue("complete")
		r.queue("complete")
		r.runUntil(t, execution.StepCompleted, 200)
	}
	return a, b
}

// p18SessionsOf returns a project's worker session ids (database id →
// provider session id).
func p18SessionsOf(t *testing.T, r *p16Rig) map[string]string {
	t.Helper()
	rows, err := r.h.App.DB().Raw().Query(context.Background(),
		"SELECT id::text, provider_session_id FROM worker_sessions WHERE project_id = $1", r.pid)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, provider string
		_ = rows.Scan(&id, &provider)
		out[id] = provider
	}
	return out
}

// T18.172 / T18.174 / T18.175 / T18.176 — two look-alike projects never
// share prompts, state files, sessions, validation, checkpoints, events,
// cache or recovery data; a worker session cannot migrate to the other
// project; project B never receives project A's prompt even with identical
// task text; project-scoped audit reads stay in their project.
func TestT18172TwinProjectsIsolated(t *testing.T) {
	a, b := p18TwinProjects(t)
	ctx := context.Background()
	pa, pb := a.pid.String(), b.pid.String()

	// 18.172: every cross-table reference stays inside its project — the
	// invariant scanner checks ownership (dispatch / unit / plan / step /
	// validation / result / session) and the composite FKs of 00116.
	rep, err := a.h.App.Integrity().Scan(ctx, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() {
		t.Fatalf("cross-project references: %v", p18Violations(rep))
	}
	for _, q := range []string{
		// Prompts: a dispatch's worker / canonical prompt and snapshot.
		`SELECT count(*) FROM worker_dispatches d JOIN worker_prompts wp ON wp.id = d.worker_prompt_id WHERE wp.project_id <> d.project_id`,
		`SELECT count(*) FROM worker_prompts wp JOIN canonical_prompts cp ON cp.id = wp.canonical_prompt_id WHERE cp.project_id <> wp.project_id`,
		`SELECT count(*) FROM canonical_prompts cp JOIN context_snapshots cs ON cs.id = cp.context_snapshot_id WHERE cs.project_id <> cp.project_id`,
		// Cache.
		`SELECT count(*) FROM prompt_cache_entries e JOIN canonical_prompts cp ON cp.id = e.canonical_prompt_id WHERE cp.project_id <> e.project_id`,
		// State files and their snapshots.
		`SELECT count(*) FROM context_snapshot_files f JOIN context_snapshots cs ON cs.id = f.snapshot_id
			JOIN state_files sf ON sf.id = f.state_file_id WHERE sf.project_id IS NOT NULL AND sf.project_id <> cs.project_id`,
		// Sessions.
		`SELECT count(*) FROM worker_dispatches d JOIN worker_sessions s ON s.id = d.session_id WHERE s.project_id <> d.project_id`,
		// Validation and checkpoints.
		`SELECT count(*) FROM validation_runs v JOIN executions e ON e.id = v.execution_id WHERE e.project_id <> v.project_id`,
		`SELECT count(*) FROM repository_checkpoints c JOIN executions e ON e.id = c.execution_id WHERE e.project_id <> c.project_id`,
		// Events and recovery.
		`SELECT count(*) FROM execution_events ev JOIN executions e ON e.id = ev.execution_id WHERE e.project_id <> ev.project_id`,
		`SELECT count(*) FROM recovery_plans rp JOIN executions e ON e.id = rp.execution_id WHERE e.project_id <> rp.project_id`,
	} {
		if n := a.count(t, q); n != 0 {
			t.Errorf("%d cross-project rows: %s", n, q)
		}
	}
	for _, r := range []*p16Rig{a.p16Rig, b} {
		if n := r.count(t, "SELECT count(*) FROM worker_dispatches WHERE project_id = '"+r.pid.String()+"'"); n < 3 {
			t.Fatalf("project %s: %d dispatches (setup)", r.pid, n)
		}
	}

	// 18.174: sessions are disjoint; every message went to a session of
	// the dispatching project; a session cannot be moved to the other
	// project (its results pin it).
	sa, sb := p18SessionsOf(t, a.p16Rig), p18SessionsOf(t, b)
	if len(sa) == 0 || len(sb) == 0 {
		t.Fatal("setup: no sessions")
	}
	for id := range sa {
		if _, shared := sb[id]; shared {
			t.Fatalf("session %s shared", id)
		}
	}
	providerProject := map[string]string{}
	for _, p := range sa {
		providerProject[p] = pa
	}
	for _, p := range sb {
		if providerProject[p] != "" {
			t.Fatalf("provider session %s used by both projects", p)
		}
		providerProject[p] = pb
	}
	sent := map[string]int{}
	for _, m := range a.g.Messages() {
		if providerProject[m.SessionID] == "" {
			t.Fatalf("message on an unknown session %s", m.SessionID)
		}
		sent[providerProject[m.SessionID]]++
	}
	if sent[pa] == 0 || sent[pb] == 0 {
		t.Fatalf("messages per project: %v", sent)
	}
	for id := range sa {
		if _, err := a.h.DBExec(ctx, "UPDATE worker_sessions SET project_id = '"+pb+"' WHERE id = '"+id+"'"); err == nil {
			if rep, _ := a.h.App.Integrity().Scan(ctx, nil, nil); rep.OK() {
				t.Fatal("a worker session silently moved to another project")
			}
			t.Log("session move detected by the integrity scan")
		}
		break
	}

	// 18.175: identical task text, yet B's prompts are B's own: the
	// prompt identity includes the project, and project A's repository
	// path never appears in project B's prompts (and vice versa).
	var shared int
	_ = a.h.DBScan(ctx, `SELECT count(*) FROM worker_prompts x JOIN worker_prompts y ON x.id = y.id
		WHERE x.project_id = '`+pa+`' AND y.project_id = '`+pb+`'`, &shared)
	if shared != 0 {
		t.Fatal("a worker prompt is shared by both projects")
	}
	for _, m := range a.g.Messages() {
		own, other := a.dir, b.dir
		if providerProject[m.SessionID] == pb {
			own, other = b.dir, a.dir
		}
		if strings.Contains(m.Content, own) && strings.Contains(m.Content, other) {
			t.Fatalf("a prompt carries the other project's repository path %s", other)
		}
		if !strings.Contains(m.Content, own) && strings.Contains(m.Content, other) {
			t.Fatalf("a prompt for one project names only the other project's repository %s", other)
		}
	}

	// 18.176: project-scoped audit reads stay in their project; the
	// project chain verifies only that project's events.
	for _, r := range []*p16Rig{a.p16Rig, b} {
		code, _ := a.b.api(http.MethodPost, "/api/v1/projects/"+r.pid.String()+"/pause", nil)
		_ = code
	}
	code, raw := a.b.api(http.MethodGet, "/api/v1/audit?project="+pa, nil)
	var page readmodel.AuditPage
	if code != http.StatusOK || json.Unmarshal(raw, &page) != nil || len(page.Rows) == 0 {
		t.Fatalf("audit list: %d %.300s", code, raw)
	}
	for _, row := range page.Rows {
		if row.ProjectID != pa {
			t.Fatalf("project A's audit page returned %+v", row)
		}
	}
	v := p18Verify(t, a, "?project="+pb)
	if len(v.Chains) != 1 || v.Chains[0].Chain != audit.ChainKey(&b.pid) {
		t.Fatalf("project chain scope: %+v", v)
	}
}

// T18.173 — project A's working directory swapped for project B's
// repository (a symlink to B's checkout): the repository identity check
// stops A before any request is sent there.
func TestT18173OtherProjectDirectoryBlocksDispatch(t *testing.T) {
	a := p16NewRig(t)
	b := &p16Rig{p15Rig: p15Build(t, a.h, a.g, a.srv, []p15Phase{{1, "Foundation", "PENDING", 1}})}
	p16Configure(t, b, p16Options{})
	a.queuePlan(a.planJSON(p16Unit{key: "a"}))
	a.queue("complete")
	p18BreakRepoAt(t, a, execution.ActionDispatch, func() {
		if err := os.RemoveAll(a.dir); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(b.dir, a.dir); err != nil {
			t.Fatal(err)
		}
	}, "REPOSITORY_IDENTITY_MISMATCH", 1)
}
