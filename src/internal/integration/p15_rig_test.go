package integration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/compressionprovider/providertest"
	"overseer/internal/config"
	"overseer/internal/dbrepo"
	"overseer/internal/execution"
	"overseer/internal/gateway/hermes/hermestest"
	"overseer/internal/persistence"
	"overseer/internal/service"
)

// Phase 15 rig: a READY project on the DEFAULT workflow (copied from the
// seeded template), project roles initialized from the global defaults
// with the planning / implementation / validation assignments, the global
// template seeds, the Phase 13 response protocol ensured, the providertest
// fake compression provider and the hermestest fake gateway (whose
// operation log must stay empty — bootstrap never contacts the worker).
// The working directory is a temporary git repository.

type p15Phase struct {
	order  int
	name   string
	status string
	wbs    int // number of WBS versions (0 = none)
}

type p15Rig struct {
	h       *Harness
	g       *hermestest.Gateway
	srv     *providertest.Server
	pid     uuid.UUID
	dir     string
	phases  map[int]uuid.UUID
	roles   map[string]uuid.UUID
	machine uuid.UUID
	states  map[string]uuid.UUID
}

func p15Mutate(srv *providertest.Server, endpoint string, extra func(*config.Config)) func(*config.Config) {
	base := p14Mutate(srv, endpoint, false)
	return func(c *config.Config) {
		base(c)
		c.Bootstrap.RecoveryDisabled = true
		if extra != nil {
			extra(c)
		}
	}
}

// p15Harness boots an app wired for Phase 15.
func p15Harness(t *testing.T, extra func(*config.Config)) (*Harness, *hermestest.Gateway, *providertest.Server) {
	t.Helper()
	srv := providertest.New()
	t.Cleanup(srv.Close)
	g, proxy := p12Gateway(t)
	h := NewHarnessWith(t, p15Mutate(srv, proxy.URL(), extra))
	return h, g, srv
}

func p15Git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// p15Repo creates a committed temp git repository.
func p15Repo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p15Git(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("project\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p15Git(t, dir, "add", "README.md")
	p15Git(t, dir, "commit", "-q", "-m", "initial")
	return dir
}

// p15DefaultPhases: one PENDING phase with WBS v1, no plan.
var p15DefaultPhases = []p15Phase{{1, "Foundation", "PENDING", 1}}

func p15NewRig(t *testing.T) *p15Rig { return p15NewRigWith(t, p15DefaultPhases, nil) }

func p15NewRigWith(t *testing.T, phases []p15Phase, extra func(*config.Config)) *p15Rig {
	t.Helper()
	h, g, srv := p15Harness(t, extra)
	return p15Build(t, h, g, srv, phases)
}

func p15Build(t *testing.T, h *Harness, g *hermestest.Gateway, srv *providertest.Server, phases []p15Phase) *p15Rig {
	t.Helper()
	return p15BuildWorkflow(t, h, g, srv, phases, false)
}

// p15BuildWorkflow builds the rig on the composed-prompt workflow, or —
// statefile — on the default statefile workflow with its default role
// assignments.
func p15BuildWorkflow(t *testing.T, h *Harness, g *hermestest.Gateway, srv *providertest.Server, phases []p15Phase, statefile bool) *p15Rig {
	t.Helper()
	ctx := context.Background()
	raw := h.App.DB().Raw()
	r := &p15Rig{h: h, g: g, srv: srv, phases: map[int]uuid.UUID{}, roles: map[string]uuid.UUID{}, states: map[string]uuid.UUID{}}

	// Global seeds (templates + default workflow template).
	if err := service.NewPromptTemplateService(dbrepo.NewPromptTemplateRepository(raw), dbrepo.NewPromptTemplateVersionRepository(raw)).
		WithTxPool(raw).SeedDefaults(ctx); err != nil {
		t.Fatalf("seed templates: %v", err)
	}
	tmplSvc := service.NewStateTemplateService(dbrepo.NewStateMachineRepository(raw), dbrepo.NewWorkflowStateRepository(raw),
		dbrepo.NewStateTransitionRepository(raw), dbrepo.NewProjectRepository(raw)).WithTxPool(raw)
	if _, err := tmplSvc.SeedDefaultTemplate(ctx); err != nil {
		t.Fatalf("seed workflow template: %v", err)
	}

	// Project + settings.
	ps := service.NewProjectService(dbrepo.NewProjectRepository(raw), dbrepo.NewSettingsRepository(raw), raw)
	p, _, err := ps.CreateProject(ctx, "p15 project "+uuid.NewString()[:6], "")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	r.pid = p.ID
	r.dir = p15Repo(t)
	r.exec(t, "UPDATE project_settings SET worker_provider = 'hermes', worker_model = 'hermes-worker', worker_timeout = 10000000000, "+
		"compression_provider = 'fake', compression_model = 'model-A', context_window = 64000, context_threshold = 0.8, "+
		"working_directory = '"+r.dir+"' WHERE project_id = '"+r.pid.String()+"'")

	// Roles.
	roleRecs, err := p718RoleSvc(t, h).InitializeProjectRoles(ctx, r.pid.String())
	if err != nil {
		t.Fatalf("init roles: %v", err)
	}
	for _, rr := range roleRecs {
		r.roles[rr.Key] = rr.ID
	}

	r.installWorkflow(t, statefile)

	// Phases + WBS versions.
	for _, ph := range phases {
		id := seedPhaseStatus(t, h, r.pid, ph.order, ph.name, ph.status)
		r.phases[ph.order] = id
		for v := 1; v <= ph.wbs; v++ {
			r.appendWBS(t, ph.order)
		}
	}
	r.exec(t, "UPDATE projects SET status = 'READY' WHERE id = '"+r.pid.String()+"'")
	return r
}

// installWorkflow copies the composed-prompt workflow (or, statefile, the
// default statefile workflow) into the rig's project, assigns its roles
// (r.roles must be populated) and pins the response protocol on every
// prompted state.
func (r *p15Rig) installWorkflow(t *testing.T, statefile bool) {
	t.Helper()
	ctx := context.Background()
	h := r.h
	raw := h.App.DB().Raw()
	tmplSvc := service.NewStateTemplateService(dbrepo.NewStateMachineRepository(raw), dbrepo.NewWorkflowStateRepository(raw),
		dbrepo.NewStateTransitionRepository(raw), dbrepo.NewProjectRepository(raw)).WithTxPool(raw)
	r.states = map[string]uuid.UUID{}
	// Default workflow copied to the project + role assignments.
	copyWorkflow := tmplSvc.CopyComposedToProject
	if statefile {
		copyWorkflow = tmplSvc.CopyToProject
	}
	mid, err := copyWorkflow(ctx, r.pid.String())
	if err != nil {
		t.Fatalf("copy workflow: %v", err)
	}
	r.machine = uuid.MustParse(mid)
	recs, err := dbrepo.NewWorkflowStateRepository(raw).ListByMachine(ctx, r.machine, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range recs {
		r.states[s.Key] = s.ID
	}
	allowed := map[string][]string{}
	assignments := map[string][]string{"PHASE_PLAN": {"PLANNER"}, "PLAN_VALIDATE": {"ARCHITECT"}, "IMPLEMENT": {"ENGINEER"},
		"CONTINUE": {"ENGINEER"}, "FIX": {"ENGINEER"}, "VALIDATE": {"TESTER"}, "PHASE_VALIDATE": {"TESTER"}}
	if statefile {
		assignments = service.DefaultStateRoles
	}
	for state, roles := range assignments {
		if _, ok := r.states[state]; !ok {
			continue
		}
		for i, role := range roles {
			r.assignAt(t, state, role, i)
			allowed[role] = append(allowed[role], state)
		}
	}
	allowed["PROJECT_MANAGER"] = append(allowed["PROJECT_MANAGER"], "PHASE_VALIDATE")
	for role, states := range allowed {
		if _, err := p718RoleSvc(t, h).SetAllowedStates(ctx, r.roles[role].String(), states); err != nil {
			t.Fatalf("allowed states %s: %v", role, err)
		}
	}

	// Response protocol (Phase 13 readiness) pinned by every prompted
	// state (Phase 8 composition contract).
	ref, err := h.App.Results().EnsureProtocol(ctx, r.pid)
	if err != nil {
		t.Fatalf("ensure protocol: %v", err)
	}
	for _, s := range recs {
		if s.PromptTemplate == "" {
			continue
		}
		if _, err := dbrepo.NewResponseProtocolRepository(raw).CreateStateReference(ctx, s.ID, ref.ProtocolID, ref.Version); err != nil {
			t.Fatalf("protocol reference %s: %v", s.Key, err)
		}
	}
}

func (r *p15Rig) exec(t *testing.T, sql string) {
	t.Helper()
	if _, err := r.h.DBExec(context.Background(), sql); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func (r *p15Rig) count(t *testing.T, sql string) int {
	t.Helper()
	var n int
	if err := r.h.DBScan(context.Background(), sql, &n); err != nil {
		t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

func (r *p15Rig) assign(t *testing.T, stateKey, roleKey string) {
	t.Helper()
	r.assignAt(t, stateKey, roleKey, 0)
}

func (r *p15Rig) assignAt(t *testing.T, stateKey, roleKey string, ordinal int) {
	t.Helper()
	now := time.Now().UTC()
	if err := dbrepo.NewStateRoleAssignmentRepository(r.h.App.DB().Raw()).Create(context.Background(), &persistence.StateRoleAssignmentRecord{
		ID: uuid.New(), StateID: r.states[stateKey], RoleID: r.roles[roleKey], Ordinal: ordinal, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("assign %s→%s: %v", stateKey, roleKey, err)
	}
}

func (r *p15Rig) appendWBS(t *testing.T, order int) int {
	t.Helper()
	rec, err := dbrepo.NewWBSVersionRepository(r.h.App.DB().Raw()).AppendVersion(context.Background(), r.phases[order].String(),
		"# WBS phase "+itoa(order)+"\n\n- deliver "+uuid.NewString()[:8]+"\n")
	if err != nil {
		t.Fatalf("append WBS: %v", err)
	}
	return rec.Version
}

// plan seeds an execution plan for the phase and returns its id.
func (r *p15Rig) plan(t *testing.T, order, wbsVersion int, status string) uuid.UUID {
	t.Helper()
	return seedPlan(t, r.h, r.phases[order], wbsVersion, 1, status)
}

// unit seeds a work unit and returns its id.
func (r *p15Rig) unit(t *testing.T, plan uuid.UUID, seq int, name, status string) uuid.UUID {
	t.Helper()
	u := uuid.New()
	r.exec(t, "INSERT INTO work_units (id, execution_plan_id, sequence, name, description, kind, status, objective, implementation_instructions, "+
		"acceptance_criteria, required_tests, constraints, created_at) VALUES ('"+u.String()+"', '"+plan.String()+"', "+itoa(seq)+", '"+name+
		"', '', 'ATOMIC', '"+status+"', 'Deliver "+name+"', 'Implement "+name+"', '[\"AC-1: "+name+" works.\"]', '[]', '[]', now())")
	p705BackfillUnitProjects(t, r.h, r.pid)
	return u
}

func (r *p15Rig) depend(t *testing.T, unit, on uuid.UUID) {
	t.Helper()
	if err := dbrepo.NewWorkUnitRepository(r.h.App.DB().Raw()).AddDependency(context.Background(), unit, on); err != nil {
		t.Fatalf("dependency: %v", err)
	}
}

func (r *p15Rig) svc() *service.BootstrapService { return r.h.App.Bootstrap() }

func (r *p15Rig) bootstrap(t *testing.T) *service.BootstrapResult {
	t.Helper()
	res, err := r.svc().Bootstrap(context.Background(), r.pid)
	if err != nil {
		var msgs []string
		if res != nil && res.Readiness != nil {
			for _, e := range res.Readiness.Errors {
				msgs = append(msgs, string(e.Code)+": "+e.Message)
			}
		}
		t.Fatalf("bootstrap: %v %v", err, msgs)
	}
	return res
}

func (r *p15Rig) bootstrapErr(t *testing.T) (*service.BootstrapResult, *execution.BootstrapError) {
	t.Helper()
	res, err := r.svc().Bootstrap(context.Background(), r.pid)
	if err == nil {
		t.Fatalf("bootstrap succeeded unexpectedly: %+v", res.Execution)
	}
	be, ok := execution.AsBootstrapError(err)
	if !ok {
		t.Fatalf("not a bootstrap error: %v", err)
	}
	return res, be
}

func (r *p15Rig) projectStatus(t *testing.T) string {
	t.Helper()
	var s string
	if err := r.h.DBScan(context.Background(), "SELECT status FROM projects WHERE id = '"+r.pid.String()+"'", &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func (r *p15Rig) where() string { return "project_id = '" + r.pid.String() + "'" }

// noWorker asserts zero worker transport activity (WBS 15.77/15.113).
func (r *p15Rig) noWorker(t *testing.T) {
	t.Helper()
	if n := r.count(t, "SELECT count(*) FROM worker_dispatches WHERE "+r.where()); n != 0 {
		t.Fatalf("worker dispatches = %d", n)
	}
	if n := r.count(t, "SELECT count(*) FROM worker_sessions WHERE "+r.where()); n != 0 {
		t.Fatalf("worker sessions = %d", n)
	}
	if n := r.count(t, "SELECT count(*) FROM continuations WHERE "+r.where()); n != 0 {
		t.Fatalf("continuations = %d", n)
	}
	if n := len(r.g.Sessions()) + len(r.g.Messages()); n != 0 {
		t.Fatalf("gateway sessions/messages = %d", n)
	}
}

// stateKey returns a workflow state's key by id.
func (r *p15Rig) stateKey(t *testing.T, id uuid.UUID) string {
	t.Helper()
	var k string
	if err := r.h.DBScan(context.Background(), "SELECT key FROM workflow_states WHERE id = '"+id.String()+"'", &k); err != nil {
		t.Fatal(err)
	}
	return k
}

// boundarySnapshot captures the rows bootstrap must never change.
func (r *p15Rig) boundarySnapshot(t *testing.T) string {
	t.Helper()
	var out []string
	q := func(sql string) {
		rows, err := r.h.App.DB().Raw().Query(context.Background(), sql)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			vals, err := rows.Values()
			if err != nil {
				t.Fatal(err)
			}
			var parts []string
			for _, v := range vals {
				parts = append(parts, strings.TrimSpace(strings.ReplaceAll(fmt.Sprint(v), "\n", " ")))
			}
			out = append(out, strings.Join(parts, "|"))
		}
	}
	pid := r.pid.String()
	q("SELECT id, phase_order, name, status, updated_at FROM phases WHERE project_id = '" + pid + "' ORDER BY phase_order")
	q("SELECT ep.id, ep.wbs_version, ep.version, ep.status, ep.updated_at FROM execution_plans ep JOIN phases p ON p.id = ep.phase_id WHERE p.project_id = '" + pid + "' ORDER BY ep.id")
	q("SELECT id, sequence, status, updated_at, completed_at FROM work_units WHERE project_id = '" + pid + "' ORDER BY id")
	q("SELECT v.phase_id, v.version, v.content_hash FROM wbs_versions v JOIN phases p ON p.id = v.phase_id WHERE p.project_id = '" + pid + "' ORDER BY v.phase_id, v.version")
	return strings.Join(out, "\n")
}
