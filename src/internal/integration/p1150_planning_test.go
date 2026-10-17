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
)

// Planning rig: phase 1 (current, WBS v1) with an EMPTY active plan,
// phase 2 (optionally with a WBS); machine PHASE_PLAN → PLANNED →
// VALIDATE_PLAN → ACCEPTED → IMPLEMENT; PLANNER serves the planning states,
// ENGINEER serves IMPLEMENT; runtime at PHASE_PLAN (the first planning
// request is in flight), bound to phase 1, no work unit.
type p11PlanRig struct {
	*p11Rig
	phase2 uuid.UUID
}

func p11NewPlanRig(t *testing.T, srv *providertest.Server, phase2WBS bool) *p11PlanRig {
	t.Helper()
	h := p9APIHarness(t, srv)
	ctx := context.Background()
	raw := h.App.DB().Raw()
	r := &p11Rig{h: h, srv: srv, units: map[string]uuid.UUID{}, states: map[string]uuid.UUID{}}
	proj, phaseID, seedUnit := p705Rig(t, h)
	planID := p710PlanOfPhase(t, h, proj)
	if _, err := h.DBExec(ctx, "DELETE FROM work_units WHERE id = '"+seedUnit.String()+"'"); err != nil {
		t.Fatal(err)
	}
	phase2 := seedPhaseStatus(t, h, proj, 2, "Phase two", "PENDING")
	wbsRepo := dbrepo.NewWBSVersionRepository(raw)
	if _, err := wbsRepo.AppendVersion(ctx, phaseID.String(), "Phase 1 WBS: 1.1 foundation, 1.2 storage, 1.3 composition."); err != nil {
		t.Fatal(err)
	}
	if phase2WBS {
		if _, err := wbsRepo.AppendVersion(ctx, phase2.String(), "Phase 2 WBS: 2.1 api, 2.2 ui."); err != nil {
			t.Fatal(err)
		}
	}
	states := []struct{ key, kind, tmpl, purpose, role string }{
		{"PHASE_PLAN", "ACTION", "PLAN_PHASE", "PHASE_PLANNING", "PLANNER"},
		{"VALIDATE_PLAN", "VALIDATION", "VALIDATE_PLAN", "PHASE_PLANNING", "PLANNER"},
		{"IMPLEMENT", "ACTION", "IMPLEMENT_WORK_UNIT", "IMPLEMENTATION", "ENGINEER"},
	}
	var defs []service.MachineStateDef
	for _, s := range states {
		defs = append(defs, service.MachineStateDef{Key: s.key, Kind: s.kind})
	}
	mid := p617SeedMachine(t, h, proj.String(), "p11-plan", defs, []service.MachineTransitionDef{
		{From: "PHASE_PLAN", Outcome: "PLANNED", To: "VALIDATE_PLAN"}, {From: "VALIDATE_PLAN", Outcome: "ACCEPTED", To: "IMPLEMENT"},
	}, "PHASE_PLAN")
	roleSvc := service.NewRoleService(dbrepo.NewRoleRepository(raw), dbrepo.NewSystemPromptRepository(raw)).WithStateFiles(dbrepo.NewRoleStateFileRepository(raw))
	pid := proj.String()
	roles := map[string]uuid.UUID{}
	for _, k := range []string{"PLANNER", "ENGINEER"} {
		rec, err := roleSvc.CreateRole(ctx, &pid, "P11P-"+k, k, "", "You are the "+k+".")
		if err != nil {
			t.Fatal(err)
		}
		roles[k] = rec.ID
		p712RequireRoleFiles(t, h, rec.ID, []string{"PROJECT_STATE", "ARCHITECTURE", "PHASE_STATE"})
	}
	protocolID := p813Protocol(t, h, &proj, "p11p-schema", prompt.KindImplementationResponse, p9Protocol)
	for _, s := range states {
		var stateID uuid.UUID
		h.DBScan(ctx, "SELECT id FROM workflow_states WHERE machine_id = '"+mid+"' AND key = '"+s.key+"'", &stateID)
		r.states[s.key] = stateID
		rec := p810Rec(t, &proj, s.tmpl, prompt.PromptPurpose(s.purpose))
		if err := dbrepo.NewPromptTemplateRepository(raw).Create(ctx, rec); err != nil {
			t.Fatal(err)
		}
		dbrepo.NewPromptTemplateVersionRepository(raw).AppendVersion(ctx, rec.ID, s.key+".\n")
		h.DBExec(ctx, "UPDATE workflow_states SET prompt_template = '"+s.tmpl+"' WHERE id = '"+stateID.String()+"'")
		dbrepo.NewStateRoleAssignmentRepository(raw).Create(ctx, &persistence.StateRoleAssignmentRecord{
			ID: uuid.New(), StateID: stateID, RoleID: roles[s.role], CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()})
		dbrepo.NewResponseProtocolRepository(raw).CreateStateReference(ctx, stateID, protocolID, 1)
	}
	files := p708Service(t, h)
	for typ, scope := range map[string]string{"PROJECT_STATE": "P", "ARCHITECTURE": "P", "PHASE_STATE": "PH"} {
		var id uuid.UUID
		if scope == "P" {
			id = p713ProjectFile(t, h, proj, p709DefaultTypeID(t, h, typ))
		} else {
			id = p713PhaseFile(t, h, proj, phaseID, p709DefaultTypeID(t, h, typ))
		}
		files.UpdateContent(ctx, id.String(), typ+": planning context.", 0)
	}
	// Phase 2's PHASE_STATE so its planning projection resolves.
	p2 := p713PhaseFile(t, h, proj, phase2, p709DefaultTypeID(t, h, "PHASE_STATE"))
	files.UpdateContent(ctx, p2.String(), "PHASE_STATE (phase 2): not started.", 0)
	if _, err := p622Service(h, true).Initialize(ctx, pid, mid); err != nil {
		t.Fatal(err)
	}
	h.DBExec(ctx, "UPDATE runtime_state SET phase_id = '"+phaseID.String()+"' WHERE project_id = '"+pid+"'")
	h.DBExec(ctx, "UPDATE project_settings SET compression_provider = 'fake', compression_model = 'model-A', context_window = 64000 WHERE project_id = '"+pid+"'")
	r.base = h.BaseURL + "/api/v1/projects/" + pid
	p9HTTP(t, http.MethodPut, r.base+"/settings/compression", `{"mode":"AUTO","target_tokens":4000,"max_attempts":2}`)
	r.pos = p9Position{ProjectID: proj, PhaseID: phaseID, PlanID: planID, MachineID: uuid.MustParse(mid)}
	return &p11PlanRig{p11Rig: r, phase2: phase2}
}

// P11-50 + P11-49 (WBS 11.32-11.34, 11.59, 11.60, 11.83, 11.84).
func TestP1150PlanningPrecompute(t *testing.T) {
	srv := providertest.New()
	defer srv.Close()
	r := p11NewPlanRig(t, srv, true)
	ctx := context.Background()
	before := r.workflowRows(t)

	// First project iteration: the planning request is dispatched.
	job := r.svc().NotifyWorkerDispatched(ctx, r.pos.ProjectID)
	r.svc().Wait()
	j, recs, _ := r.svc().GetJob(ctx, r.pos.ProjectID, job.ID)
	by := map[string]precompute.CandidateRecord{}
	for _, c := range recs {
		k := c.StateKey
		if c.PhaseID != nil && *c.PhaseID == r.phase2 {
			k += "@phase2"
		}
		by[k] = c
	}
	if j.Status != precompute.JobCompleted {
		t.Fatalf("T11.83 job %+v", j)
	}
	if v := by["VALIDATE_PLAN"]; v.Result != precompute.ResultFragment || len(v.Missing) != 1 || v.Missing[0] != precompute.InputGeneratedPlan {
		t.Fatalf("T11.59.1 plan-validation static material: %+v", v)
	}
	if impl := by["IMPLEMENT"]; impl.Readiness != precompute.Unknown || impl.Result != precompute.ResultSkipped {
		t.Fatalf("T11.59.2/11.83 implementation finalized before the plan exists: %+v", impl)
	}
	if next := by["PHASE_PLAN@phase2"]; next.Readiness != precompute.FullyKnown || next.Result != precompute.ResultBuilt {
		t.Fatalf("T11.34.1/T11.32.2 next-phase planning prompt: %+v", next)
	}
	var planStatus string
	r.h.DBScan(ctx, "SELECT status FROM phases WHERE id = '"+r.pos.PhaseID.String()+"'", &planStatus)
	if planStatus == "DONE" || r.workflowRows(t) != before {
		t.Fatal("T11.32.3/T11.34.2 phase or workflow changed by precompute")
	}

	// Plan accepted: 1.1 READY, 1.2 depends on 1.1, 1.3 on 1.1 + 1.2.
	units := map[string]uuid.UUID{}
	for i, u := range []struct{ name, status string }{{"1.1", "READY"}, {"1.2", "PENDING"}, {"1.3", "PENDING"}} {
		units[u.name] = p710UnitWithSpec(t, r.h, r.pos.PlanID, i+1, u.name, u.status, []string{"AC-1: " + u.name}, []string{"TEST-1 go test ./..."}, nil)
	}
	wu := dbrepo.NewWorkUnitRepository(r.h.App.DB().Raw())
	wu.AddDependency(ctx, units["1.2"], units["1.1"])
	wu.AddDependency(ctx, units["1.3"], units["1.1"])
	wu.AddDependency(ctx, units["1.3"], units["1.2"])
	r.h.DBExec(ctx, "UPDATE runtime_state SET current_state_id = '"+r.states["VALIDATE_PLAN"].String()+"', version = version + 1 WHERE project_id = '"+r.pos.ProjectID.String()+"'")
	job2 := r.svc().OnTrigger(ctx, r.pos.ProjectID, precompute.TriggerPlanAccepted)
	r.svc().Wait()
	_, recs2, _ := r.svc().GetJob(ctx, r.pos.ProjectID, job2.ID)
	var first *precompute.CandidateRecord
	for i, c := range recs2 {
		if c.WorkUnitID != nil && (*c.WorkUnitID == units["1.2"] || *c.WorkUnitID == units["1.3"]) {
			t.Fatal("11.84 dependency-blocked 1.2/1.3 treated as executable")
		}
		if c.NextUnit {
			first = &recs2[i]
		}
	}
	if first == nil || *first.WorkUnitID != units["1.1"] || first.Result != precompute.ResultBuilt {
		t.Fatalf("T11.60.1-T11.60.3/11.84 IMPLEMENT 1.1 not precomputed: %+v", first)
	}

	// Changed project context invalidates the cached next-phase planning
	// prompt (T11.34.3): its key moves.
	nextKey := by["PHASE_PLAN@phase2"].CacheKey
	(&p10Env{h: r.h, pos: p9Position{ProjectID: r.pos.ProjectID, Files: map[string]uuid.UUID{"ARCHITECTURE": p9FileID(t, r.h, r.pos.ProjectID, "ARCHITECTURE")}}}).writeFile(t, "ARCHITECTURE", "ARCHITECTURE: evolved.")
	job3 := r.svc().OnTrigger(ctx, r.pos.ProjectID, precompute.TriggerManual)
	r.svc().Wait()
	_, recs3, _ := r.svc().GetJob(ctx, r.pos.ProjectID, job3.ID)
	for _, c := range recs3 {
		if c.PhaseID != nil && *c.PhaseID == r.phase2 && c.CacheKey == nextKey {
			t.Fatal("T11.34.3 stale planning prompt key survived a context change")
		}
	}
}

// WBS 11.33: an appended phase is discovered through persisted ordering
// once its WBS exists; current candidates are unaffected.
func TestP1133AppendedPhase(t *testing.T) {
	srv := providertest.New()
	defer srv.Close()
	r := p11NewPlanRig(t, srv, false)
	ctx := context.Background()
	preview := func() map[string]service.CandidateView {
		cs, err := r.svc().PreviewCandidates(ctx, r.pos.ProjectID)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]service.CandidateView{}
		for _, c := range cs {
			k := c.StateKey
			if c.PhaseID != nil && *c.PhaseID == r.phase2 {
				k += "@phase2"
			}
			out[k] = c
		}
		return out
	}
	p1 := preview()
	if _, ok := p1["PHASE_PLAN@phase2"]; ok {
		t.Fatal("phase without a WBS discovered as plannable")
	}
	var order int
	r.h.DBScan(ctx, "SELECT phase_order FROM phases WHERE id = '"+r.phase2.String()+"'", &order)
	if _, err := dbrepo.NewWBSVersionRepository(r.h.App.DB().Raw()).AppendVersion(ctx, r.phase2.String(), "Phase 2 WBS appended."); err != nil {
		t.Fatal(err)
	}
	p2 := preview()
	if _, ok := p2["PHASE_PLAN@phase2"]; !ok {
		t.Fatal("T11.33.3 appended phase not discoverable")
	}
	if p1["VALIDATE_PLAN"].Readiness != p2["VALIDATE_PLAN"].Readiness {
		t.Fatal("T11.33.2 current candidate affected")
	}
}

func p9FileID(t *testing.T, h *Harness, projectID uuid.UUID, typ string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := h.DBScan(context.Background(), `SELECT sf.id FROM state_files sf JOIN state_file_types ty ON ty.id = sf.type_id WHERE sf.project_id = '`+projectID.String()+`' AND ty.key = '`+typ+`' LIMIT 1`, &id); err != nil {
		t.Fatal(err)
	}
	return id
}
