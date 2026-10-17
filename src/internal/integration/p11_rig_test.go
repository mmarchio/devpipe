package integration

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/compressionprovider/providertest"
	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/precompute"
	"overseer/internal/prompt"
	"overseer/internal/service"
	"overseer/internal/statefile"
	"overseer/internal/wbs"
)

// Phase 11 integration rig (WBS 11.86 shape): a project whose active plan is
//
//	unit (seq 1) DONE · 11.1 DONE · 11.2 DONE · 11.3 EXECUTING (current)
//	11.4 READY · 11.5 PENDING, depends on 11.4
//
// and whose machine is IMPLEMENT → COMPLETE → VALIDATE → PASS → DONE /
// FAIL → FIX → COMPLETE → VALIDATE; IMPLEMENT → INCOMPLETE → CONTINUE.
// Each state references a project-scoped prompt template (purpose drives
// the dynamic-input registry); ENGINEER serves IMPLEMENT / CONTINUE / FIX,
// TESTER serves VALIDATE. The app's own precompute + cache + compression
// services are used, with the providertest fake as compression provider.

type p11Rig struct {
	h       *Harness
	srv     *providertest.Server
	pos     p9Position
	units   map[string]uuid.UUID
	states  map[string]uuid.UUID
	base    string
	machine uuid.UUID
}

func (r *p11Rig) svc() *service.PrecomputeService { return r.h.App.Precompute() }

var p11States = []struct{ key, kind, tmpl, purpose, role string }{
	{"IMPLEMENT", "ACTION", "IMPLEMENT_WORK_UNIT", "IMPLEMENTATION", "ENGINEER"},
	{"VALIDATE", "VALIDATION", "VALIDATE_WORK_UNIT", "VALIDATION", "TESTER"},
	{"CONTINUE", "ACTION", "CONTINUE_WORK_UNIT", "CONTINUATION", "ENGINEER"},
	{"FIX", "ACTION", "FIX_WORK_UNIT", "IMPLEMENTATION", "ENGINEER"},
	{"DONE", "TERMINAL", "", "", ""},
}

var p11Transitions = []service.MachineTransitionDef{
	{From: "IMPLEMENT", Outcome: "COMPLETE", To: "VALIDATE"},
	{From: "IMPLEMENT", Outcome: "INCOMPLETE", To: "CONTINUE"},
	{From: "VALIDATE", Outcome: "PASS", To: "DONE"},
	{From: "VALIDATE", Outcome: "FAIL", To: "FIX"},
	{From: "FIX", Outcome: "COMPLETE", To: "VALIDATE"},
	{From: "CONTINUE", Outcome: "COMPLETE", To: "VALIDATE"},
}

// p11NewRig boots a harness with the fake provider and builds the rig.
// prederive11_4: create 11.4's ACTIVE_TASK file ahead of time (FULLY_KNOWN
// next unit) instead of leaving it to the placeholder mechanism.
func p11NewRig(t *testing.T, srv *providertest.Server, prederive11_4 bool) *p11Rig {
	t.Helper()
	h := p9APIHarness(t, srv)
	return p11Build(t, h, srv, prederive11_4)
}

func p11Build(t *testing.T, h *Harness, srv *providertest.Server, prederive11_4 bool) *p11Rig {
	t.Helper()
	ctx := context.Background()
	raw := h.App.DB().Raw()
	r := &p11Rig{h: h, srv: srv, units: map[string]uuid.UUID{}, states: map[string]uuid.UUID{}}
	proj, phaseID, seedUnit := p705Rig(t, h)
	planID := p710PlanOfPhase(t, h, proj)
	if _, err := h.DBExec(ctx, "UPDATE work_units SET status = 'DONE' WHERE id = '"+seedUnit.String()+"'"); err != nil {
		t.Fatal(err)
	}
	spec := p9DefaultSpec()
	for i, u := range []struct{ name, status string }{{"11.1", "DONE"}, {"11.2", "DONE"}, {"11.3", "EXECUTING"}, {"11.4", "READY"}, {"11.5", "PENDING"}} {
		acc := []string{"AC-1: " + u.name + " criterion one.", "AC-2: " + u.name + " criterion two."}
		r.units[u.name] = p710UnitWithSpec(t, h, planID, i+2, u.name, u.status, acc, []string{"TEST-1 go test ./internal/" + u.name + "/..."}, []string{p9C1, p9C2})
	}
	if err := dbrepo.NewWorkUnitRepository(raw).AddDependency(ctx, r.units["11.5"], r.units["11.4"]); err != nil {
		t.Fatalf("dependency: %v", err)
	}

	var defs []service.MachineStateDef
	for _, s := range p11States {
		defs = append(defs, service.MachineStateDef{Key: s.key, Kind: s.kind})
	}
	mid := p617SeedMachine(t, h, proj.String(), "p11-machine", defs, p11Transitions, "IMPLEMENT")
	r.machine = uuid.MustParse(mid)

	roleSvc := service.NewRoleService(dbrepo.NewRoleRepository(raw), dbrepo.NewSystemPromptRepository(raw)).
		WithStateFiles(dbrepo.NewRoleStateFileRepository(raw))
	pid := proj.String()
	roles := map[string]uuid.UUID{}
	for _, k := range []string{"ENGINEER", "TESTER"} {
		rec, err := roleSvc.CreateRole(ctx, &pid, "P11-"+k, k, "", "You are the "+k+" role.\nNever modify files outside the active task scope.")
		if err != nil {
			t.Fatalf("role %s: %v", k, err)
		}
		roles[k] = rec.ID
	}
	instr := newInstructionRecord(t, &proj, "p11-tdd", "TDD", p9RoleInstruction)
	if err := dbrepo.NewInstructionRepository(raw).Create(ctx, instr); err != nil {
		t.Fatal(err)
	}
	if err := p718RoleSvc(t, h).AssignInstruction(ctx, roles["ENGINEER"].String(), instr.ID.String()); err != nil {
		t.Fatal(err)
	}
	p712RequireRoleFiles(t, h, roles["ENGINEER"], []string{"PROJECT_STATE", "ARCHITECTURE", "PHASE_STATE", "ACTIVE_TASK"})

	protocolID := p813Protocol(t, h, &proj, "p11-schema", prompt.KindImplementationResponse, p9Protocol)
	tmpls := dbrepo.NewPromptTemplateRepository(raw)
	tver := dbrepo.NewPromptTemplateVersionRepository(raw)
	for _, s := range p11States {
		var stateID uuid.UUID
		if err := h.DBScan(ctx, "SELECT id FROM workflow_states WHERE machine_id = '"+mid+"' AND key = '"+s.key+"'", &stateID); err != nil {
			t.Fatal(err)
		}
		r.states[s.key] = stateID
		if s.tmpl == "" {
			continue
		}
		rec := p810Rec(t, &proj, s.tmpl, prompt.PromptPurpose(s.purpose))
		if err := tmpls.Create(ctx, rec); err != nil {
			t.Fatalf("template %s: %v", s.tmpl, err)
		}
		if _, err := tver.AppendVersion(ctx, rec.ID, s.key+" the active work unit.\n"); err != nil {
			t.Fatal(err)
		}
		if _, err := h.DBExec(ctx, "UPDATE workflow_states SET prompt_template = '"+s.tmpl+"' WHERE id = '"+stateID.String()+"'"); err != nil {
			t.Fatal(err)
		}
		if err := dbrepo.NewStateRoleAssignmentRepository(raw).Create(ctx, &persistence.StateRoleAssignmentRecord{
			ID: uuid.New(), StateID: stateID, RoleID: roles[s.role], Ordinal: 0, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := dbrepo.NewStateInstructionRepository(raw).AppendVersion(ctx, stateID, s.key+" state instruction: report status in the required format."); err != nil {
			t.Fatal(err)
		}
		if _, err := dbrepo.NewResponseProtocolRepository(raw).CreateStateReference(ctx, stateID, protocolID, 1); err != nil {
			t.Fatal(err)
		}
	}

	files := p708Service(t, h)
	fileIDs := map[string]uuid.UUID{
		"PROJECT_STATE": p713ProjectFile(t, h, proj, p709DefaultTypeID(t, h, "PROJECT_STATE")),
		"ARCHITECTURE":  p713ProjectFile(t, h, proj, p709DefaultTypeID(t, h, "ARCHITECTURE")),
		"PHASE_STATE":   p713PhaseFile(t, h, proj, phaseID, p709DefaultTypeID(t, h, "PHASE_STATE")),
	}
	write := func(id uuid.UUID, content string) {
		if _, err := files.UpdateContent(ctx, id.String(), content, 0); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	write(fileIDs["PROJECT_STATE"], spec.ProjectState)
	write(fileIDs["ARCHITECTURE"], spec.Architecture)
	write(fileIDs["PHASE_STATE"], spec.PhaseState)
	at := service.NewActiveTaskService(files, dbrepo.NewPhaseRepository(raw), dbrepo.NewPlanRepository(raw), dbrepo.NewWorkUnitRepository(raw))
	if _, err := at.DeriveActiveTask(ctx, pid); err != nil { // 11.3 (current unit)
		t.Fatalf("derive ACTIVE_TASK 11.3: %v", err)
	}
	if prederive11_4 {
		u4, _ := dbrepo.NewWorkUnitRepository(raw).Get(ctx, r.units["11.4"])
		f := p710ActiveTaskFile(t, h, files, proj, r.units["11.4"])
		write(f, statefile.RenderActiveTask(u4))
	}
	if _, err := p622Service(h, true).Initialize(ctx, pid, mid); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DBExec(ctx, "UPDATE runtime_state SET phase_id = '"+phaseID.String()+"', work_unit_id = '"+r.units["11.3"].String()+"' WHERE project_id = '"+pid+"'"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DBExec(ctx, "UPDATE project_settings SET compression_provider = 'fake', compression_model = 'model-A', context_window = 64000 WHERE project_id = '"+pid+"'"); err != nil {
		t.Fatal(err)
	}
	r.base = h.BaseURL + "/api/v1/projects/" + pid
	if code, raw, _ := p9HTTP(t, http.MethodPut, r.base+"/settings/compression", `{"mode":"AUTO","target_tokens":4000,"max_attempts":2}`); code != 200 {
		t.Fatalf("compression settings: %s", raw)
	}
	r.pos = p9Position{ProjectID: proj, PhaseID: phaseID, UnitID: r.units["11.3"], StateID: r.states["IMPLEMENT"], PlanID: planID,
		MachineID: r.machine, RoleID: roles["ENGINEER"], InstructionID: instr.ID, ProtocolID: protocolID, Files: fileIDs}
	return r
}

// warm runs a synchronous precompute job for the current position.
func (r *p11Rig) warm(t *testing.T, trigger precompute.Trigger) precompute.Result {
	t.Helper()
	rt, err := r.svc().RuntimeID(context.Background(), r.pos.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.svc().WarmUpcomingPrompts(context.Background(), precompute.Request{ProjectID: r.pos.ProjectID, RuntimeStateID: rt, Trigger: trigger})
	if err != nil {
		t.Fatalf("warm: %v", err)
	}
	return res
}

func (r *p11Rig) candidates(t *testing.T, jobID uuid.UUID) map[string]precompute.CandidateRecord {
	t.Helper()
	_, recs, err := r.svc().GetJob(context.Background(), r.pos.ProjectID, jobID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]precompute.CandidateRecord{}
	for _, c := range recs {
		k := c.StateKey
		if c.NextUnit {
			k += "+next"
		}
		out[k] = c
	}
	return out
}

// moveTo simulates the normal state machine moving the runtime to state
// (bound to unit), advancing the runtime version as a real transition does.
func (r *p11Rig) moveTo(t *testing.T, state string, unit uuid.UUID) {
	t.Helper()
	if _, err := r.h.DBExec(context.Background(), "UPDATE runtime_state SET current_state_id = '"+r.states[state].String()+"', work_unit_id = '"+unit.String()+
		"', version = version + 1 WHERE project_id = '"+r.pos.ProjectID.String()+"'"); err != nil {
		t.Fatal(err)
	}
}

// completeCurrent marks unit DONE through the work-unit repository (the
// normal status op — never the precompute engine).
func (r *p11Rig) markDone(t *testing.T, unit uuid.UUID) {
	t.Helper()
	if err := dbrepo.NewWorkUnitRepository(r.h.App.DB().Raw()).MarkDone(context.Background(), unit); err != nil {
		t.Fatalf("mark done: %v", err)
	}
}

func (r *p11Rig) prepareNext(t *testing.T, req service.NextRequest) *service.PrepareResult {
	t.Helper()
	res, err := r.svc().PrepareNext(context.Background(), r.pos.ProjectID, req)
	if err != nil {
		t.Fatalf("prepare next: %v", err)
	}
	return res
}

// p11Rows fingerprints the authoritative workflow rows (runtime, units,
// state files, file versions, project) — the WBS 11.35 boundary.
func (r *p11Rig) workflowRows(t *testing.T) string {
	t.Helper()
	pid := r.pos.ProjectID.String()
	return p10Rows(t, r.h, "runtime_state", "project_id = '"+pid+"'") + "#" +
		p10Rows(t, r.h, "work_units", "execution_plan_id = '"+r.pos.PlanID.String()+"'") + "#" +
		p10Rows(t, r.h, "state_files", "project_id = '"+pid+"'") + "#" +
		p10ProjectRow(t, r.h, pid)
}

// p10ProjectRow is the project row without config_version (P17-46: the
// configuration-version counter moves on every settings write; it is
// configuration metadata, not workflow state — accepted test change).
func p10ProjectRow(t *testing.T, h *Harness, pid string) string {
	t.Helper()
	var s string
	if err := h.DBScan(context.Background(), "SELECT coalesce(string_agg((to_jsonb(x) - 'config_version')::text, '|' ORDER BY x.id), '') FROM projects x WHERE id = '"+pid+"'", &s); err != nil {
		t.Fatal(err)
	}
	return s
}

var _ = wbs.WorkUnitStatusDONE
