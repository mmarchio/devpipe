package integration

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"overseer/internal/compressionprovider/providertest"
	"overseer/internal/config"
	"overseer/internal/continuation"
	"overseer/internal/gateway/hermes/hermestest"
	"overseer/internal/persistence"
	"overseer/internal/service"
	"overseer/internal/worker"
)

// Phase 14 rig: the Phase 13 rig (session S1 in the project directory, a
// prepared IMPLEMENT prompt) on a RUNNING project with the background
// continuation runner disabled — tests drive the controller explicitly
// (ProcessProject) so every external operation is deterministic.

const p14Window = 64000

func p14Report(status string, completed, remaining []string, next string) string {
	q := func(xs []string) string {
		if len(xs) == 0 {
			return "[]"
		}
		return `["` + strings.Join(xs, `","`) + `"]`
	}
	blocking := "[]"
	if status == "blocked" {
		blocking = `["external dependency missing"]`
	}
	return `{"protocol_version":"1.0","status":"` + status + `","summary":"Worker report ` + status + `.","completed":` + q(completed) +
		`,"remaining":` + q(remaining) + `,"blocking":` + blocking + `,"next_action":"` + next + `"}`
}

type p14Rig struct {
	*p13Rig
}

func p14Mutate(srv *providertest.Server, endpoint string, auto bool) func(*config.Config) {
	base := p12Mutate(srv, endpoint)
	return func(c *config.Config) {
		base(c)
		c.Continuation.Disabled = !auto
		c.Continuation.PollInterval = 50_000_000
	}
}

func p14Build(t *testing.T, r *p12Rig) *p14Rig {
	t.Helper()
	rr := &p14Rig{p13Rig: p13From(t, r, "/repos/project-a")}
	if _, err := r.h.DBExec(context.Background(), "UPDATE projects SET status = 'RUNNING' WHERE id = '"+r.pos.ProjectID.String()+"'"); err != nil {
		t.Fatal(err)
	}
	return rr
}

func p14NewRig(t *testing.T) *p14Rig { return p14NewRigWith(t, false) }

func p14NewRigWith(t *testing.T, auto bool) *p14Rig {
	t.Helper()
	srv := providertest.New()
	t.Cleanup(srv.Close)
	g, proxy := p12Gateway(t)
	h := NewHarnessWith(t, p14Mutate(srv, proxy.URL(), auto))
	r := p12Configure(t, p11Build(t, h, srv, true), "/repos/project-a")
	r.g, r.proxy = g, proxy
	return p14Build(t, r)
}

func (r *p14Rig) cs() *service.ContinuationService { return r.h.App.Continuations() }

func p14Usage(tokens int64) *hermestest.Usage {
	return &hermestest.Usage{InputTokens: hermestest.I64(tokens / 2), OutputTokens: hermestest.I64(100),
		ContextTokens: hermestest.I64(tokens), ContextWindow: hermestest.I64(p14Window)}
}

// reply queues the worker's next reply with authoritative context usage.
func (r *p14Rig) reply(content string, tokens int64) {
	c := content
	r.g.Script(hermestest.Behavior{Content: &c, Usage: p14Usage(tokens)})
}

// initial dispatches the ordinary IMPLEMENT prompt with the given reply.
func (r *p14Rig) initial(t *testing.T, content string, tokens int64) worker.WorkerDispatch {
	t.Helper()
	return r.report(t, content, hermestest.Behavior{Usage: p14Usage(tokens)})
}

func (r *p14Rig) process(t *testing.T) {
	t.Helper()
	if err := r.cs().ProcessProject(context.Background(), r.pos.ProjectID); err != nil {
		t.Fatalf("process continuations: %v", err)
	}
}

func (r *p14Rig) unitContinuations(t *testing.T) []continuation.Continuation {
	t.Helper()
	cs, _, err := r.cs().Continuations(context.Background(), persistence.ContinuationFilter{ProjectID: r.pos.ProjectID, Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	// oldest first
	for i, j := 0, len(cs)-1; i < j; i, j = i+1, j-1 {
		cs[i], cs[j] = cs[j], cs[i]
	}
	return cs
}

func (r *p14Rig) cont(t *testing.T, id uuid.UUID) *continuation.Continuation {
	t.Helper()
	c, err := r.cs().Continuation(context.Background(), r.pos.ProjectID, id)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (r *p14Rig) history(t *testing.T, id uuid.UUID) []string {
	t.Helper()
	h, err := r.cs().History(context.Background(), r.pos.ProjectID, id)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, tr := range h.Transitions {
		out = append(out, string(tr.To))
	}
	return out
}

func (r *p14Rig) dispatchOf(t *testing.T, id uuid.UUID) *service.DispatchDetail {
	t.Helper()
	d, err := r.ws().GetDispatch(context.Background(), r.pos.ProjectID, id)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func (r *p14Rig) setSettings(t *testing.T, mut func(*continuation.Settings)) {
	t.Helper()
	st := continuation.DefaultSettings()
	mut(&st)
	if _, err := r.cs().UpdateContinuationSettings(context.Background(), r.pos.ProjectID, st); err != nil {
		t.Fatal(err)
	}
}

// ops counts operations by kind: "new" sessions the overseer created
// (chat completions create no gateway-side session, so they are counted
// from worker_sessions), "cd" session-start prompts and "msg" messages
// received by the gateway.
func (r *p14Rig) ops(kind string) int {
	if kind == "new" {
		var n int
		if err := r.h.DBScan(context.Background(), "SELECT count(*) FROM worker_sessions WHERE project_id = '"+r.pos.ProjectID.String()+"'", &n); err != nil {
			panic(err)
		}
		return n
	}
	n := 0
	for _, op := range r.g.Ops() {
		if op == kind {
			n++
		}
	}
	return n
}

// opLog returns the ordered gateway operations (new / cd / msg) from index.
func (r *p14Rig) opLog(from int) []string {
	ops := r.g.Ops()
	if from > len(ops) {
		return nil
	}
	return ops[from:]
}

func (r *p14Rig) activeSession(t *testing.T) *persistence.WorkerSessionRecord {
	t.Helper()
	s, err := r.ws().ActiveSession(context.Background(), r.pos.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// lastContent is the last message content sent to the gateway.
func (r *p14Rig) lastContent(t *testing.T) string {
	t.Helper()
	ms := r.g.Messages()
	if len(ms) == 0 {
		t.Fatal("no message sent")
	}
	return ms[len(ms)-1].Content
}

// p14TemplateKey returns the prompt template key a worker prompt was
// composed from.
func p14TemplateKey(t *testing.T, r *p14Rig, workerPromptID uuid.UUID) string {
	t.Helper()
	var key string
	if err := r.h.DBScan(context.Background(), `SELECT pt.key FROM worker_prompts wp JOIN canonical_prompts cp ON cp.id = wp.canonical_prompt_id
		JOIN prompt_templates pt ON pt.id = cp.template_id WHERE wp.id = '`+workerPromptID.String()+`'`, &key); err != nil {
		t.Fatal(err)
	}
	return key
}

// telemetry replaces a dispatch's Phase 12 usage observations: source ""
// leaves none (no usable telemetry); otherwise one observation is
// recorded with the given source and tokens.
func (r *p14Rig) telemetry(t *testing.T, d worker.WorkerDispatch, source worker.UsageSource, tokens int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := r.h.DBExec(ctx, "DELETE FROM worker_usage_observations WHERE dispatch_id = '"+d.ID.String()+"'"); err != nil {
		t.Fatal(err)
	}
	if source == "" {
		return
	}
	est := "false"
	if source == worker.SourceEstimated {
		est = "true"
	}
	if _, err := r.h.DBExec(ctx, "INSERT INTO worker_usage_observations (id, project_id, session_id, dispatch_id, context_tokens, context_window, estimated, source, observed_at) VALUES ('"+
		uuid.NewString()+"', '"+d.ProjectID.String()+"', '"+d.SessionID.String()+"', '"+d.ID.String()+"', "+itoa64(tokens)+", 64000, "+est+", '"+string(source)+"', now())"); err != nil {
		t.Fatal(err)
	}
}

func itoa64(v int64) string { return strconv.FormatInt(v, 10) }

// fail dispatches the ordinary prompt with a scripted failure.
func (r *p14Rig) fail(t *testing.T, b hermestest.Behavior) worker.WorkerDispatch {
	t.Helper()
	r.g.Script(b)
	r.attempt++
	return r.dispatch(t, r.wp, r.attempt).Dispatch
}

// p14RestartRig boots a Phase 14 rig on a dedicated database; boot()
// starts another overseer instance on the same database and gateway
// (restart or second instance), with the background runner disabled.
func p14RestartRig(t *testing.T) (*p14Rig, func() *Harness) {
	t.Helper()
	srv := providertest.New()
	t.Cleanup(srv.Close)
	g, proxy := p12Gateway(t)
	base := baseDSN()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
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
	boot := func() *Harness {
		h := NewHarnessOnWith(t, base, dbName, p14Mutate(srv, proxy.URL(), false))
		t.Cleanup(h.Close)
		return h
	}
	h := boot()
	r := p12Configure(t, p11Build(t, h, srv, true), "/repos/project-a")
	r.g, r.proxy = g, proxy
	return p14Build(t, r), boot
}

// force runs SQL with the continuation / dispatch guards disabled — the
// test's way to reproduce a crash window (a state the process was in
// when it died).
func (r *p14Rig) force(t *testing.T, stmts ...string) {
	t.Helper()
	ctx := context.Background()
	all := append([]string{"ALTER TABLE continuations DISABLE TRIGGER continuations_binding",
		"ALTER TABLE worker_dispatches DISABLE TRIGGER worker_dispatches_guard"}, stmts...)
	all = append(all, "ALTER TABLE continuations ENABLE TRIGGER continuations_binding",
		"ALTER TABLE worker_dispatches ENABLE TRIGGER worker_dispatches_guard")
	for _, s := range all {
		if _, err := r.h.DBExec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

func p14UUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	u, err := uuid.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func p14Behavior(content string, tokens int64) hermestest.Behavior {
	c := content
	return hermestest.Behavior{Content: &c, Usage: p14Usage(tokens)}
}
