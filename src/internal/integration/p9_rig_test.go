package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/compression"
	"overseer/internal/compressionprovider"
	"overseer/internal/compressionprovider/providertest"
	"overseer/internal/config"
	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/prompt"
	"overseer/internal/service"
)

// Phase 9 integration rig. A configurable implementation position (phase,
// active work unit, ENGINEER role at IMPLEMENT, the four required state
// files, a pinned response protocol, a template) composed through the
// P8-38 composer WITH component persistence (P9-14), plus a compression
// service over the harness pool whose provider is a test double (scripted
// mock or the httptest OpenAI-compatible fake). No real model is called.

const (
	p9AC1 = "AC-1: The exact preservation validator verifies every PRESERVE_EXACT fragment without an LLM."
	p9AC2 = "AC-2: A missing acceptance criterion fails validation."
	p9AC3 = "AC-3: A modified command fails validation."
	p9AC4 = "AC-4: A modified status enum fails validation."
	p9T1  = "TEST-1 go test ./internal/compression/..."
	p9T2  = "TEST-2 go test ./..."
	p9T3  = "TEST-3 go test -race ./internal/compression/..."
	p9C1  = "CONSTRAINT-1: Do not modify the public API."
	p9C2  = "CONSTRAINT-2: Do not add external dependencies."

	p9Protocol = `{
  "status": "complete|incomplete|blocked|failed",
  "summary": "...",
  "completed": [],
  "remaining": [],
  "blocking": [],
  "next_action": "..."
}`
	p9System          = "You are the ENGINEER role of the overseer team.\nImplement only the active work unit.\nNever modify files outside the active task scope."
	p9RoleInstruction = "Follow test-driven development: write the failing test first.\nDo not skip or weaken required tests."
	p9StateInstr      = "IMPLEMENT: implement the active work unit and report status using the required response format."
	p9ActiveTask      = "ACTIVE_TASK: implement work unit 9.17 (exact preservation validator)."
)

// p9Background builds a large, compressible narrative (PROJECT_STATE /
// ARCHITECTURE background) of roughly n lines.
func p9Background(title string, n int) string {
	var b strings.Builder
	b.WriteString(title + ": background context for the overseer project.\n")
	for i := 1; i <= n; i++ {
		b.WriteString("Earlier phase " + itoa(i) + " delivered persistence, roles, state machines, state files and canonical prompts; the history is retained for reference only.\n")
	}
	return b.String()
}

type p9Spec struct {
	UnitName     string
	Acceptance   []string
	Tests        []string
	Constraints  []string
	SystemPrompt string
	RoleInstr    string
	ProjectState string
	Architecture string
	PhaseState   string
	ActiveTask   string
	Protocol     string
}

func p9DefaultSpec() p9Spec {
	return p9Spec{
		UnitName:     "9.17",
		Acceptance:   []string{p9AC1, p9AC2, p9AC3, p9AC4},
		Tests:        []string{p9T1, p9T2, p9T3},
		Constraints:  []string{p9C1, p9C2},
		SystemPrompt: p9System,
		RoleInstr:    p9RoleInstruction,
		ProjectState: p9Background("PROJECT_STATE", 120),
		Architecture: p9Background("ARCHITECTURE", 120),
		PhaseState:   p9Background("PHASE_STATE", 40),
		ActiveTask:   p9ActiveTask,
		Protocol:     p9Protocol,
	}
}

type p9Position struct {
	ProjectID, PhaseID, UnitID, SnapID, TemplateID, StateID uuid.UUID
	// Phase 10 additions: source ids the cache tests mutate.
	RoleID, InstructionID, ProtocolID, PlanID, MachineID uuid.UUID
	Files                                                map[string]uuid.UUID
}

// p9Rig builds the configurable implementation position.
func p9Rig(t *testing.T, h *Harness, spec p9Spec) p9Position {
	t.Helper()
	ctx := context.Background()
	raw := h.App.DB().Raw()

	proj, phaseID, _ := p705Rig(t, h)
	planID := p710PlanOfPhase(t, h, proj)
	unit := p710UnitWithSpec(t, h, planID, 1, spec.UnitName, "EXECUTING", spec.Acceptance, spec.Tests, spec.Constraints)

	mid := p617SeedMachine(t, h, proj.String(), "p9-machine",
		[]service.MachineStateDef{{Key: "IMPLEMENT", Kind: "ACTION"}}, nil, "IMPLEMENT")
	var stateID uuid.UUID
	if err := h.DBScan(ctx, "SELECT id FROM workflow_states WHERE machine_id = '"+mid+"' AND key = 'IMPLEMENT'", &stateID); err != nil {
		t.Fatalf("scan state: %v", err)
	}

	roleSvc := service.NewRoleService(dbrepo.NewRoleRepository(raw), dbrepo.NewSystemPromptRepository(raw)).
		WithStateFiles(dbrepo.NewRoleStateFileRepository(raw))
	pid := proj.String()
	role, err := roleSvc.CreateRole(ctx, &pid, "P9-ENGINEER", "P9 Engineer", "", spec.SystemPrompt)
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	instr := newInstructionRecord(t, &proj, "p9-tdd", "TDD", spec.RoleInstr)
	if err := dbrepo.NewInstructionRepository(raw).Create(ctx, instr); err != nil {
		t.Fatalf("create instruction: %v", err)
	}
	if err := p718RoleSvc(t, h).AssignInstruction(ctx, role.ID.String(), instr.ID.String()); err != nil {
		t.Fatalf("assign instruction: %v", err)
	}
	p712RequireRoleFiles(t, h, role.ID, []string{"PROJECT_STATE", "ARCHITECTURE", "PHASE_STATE", "ACTIVE_TASK"})
	if err := dbrepo.NewStateRoleAssignmentRepository(raw).Create(ctx, &persistence.StateRoleAssignmentRecord{
		ID: uuid.New(), StateID: stateID, RoleID: role.ID, Ordinal: 0,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create assignment: %v", err)
	}

	files := p708Service(t, h)
	write := func(id uuid.UUID, content string) {
		if _, err := files.UpdateContent(ctx, id.String(), content, 0); err != nil {
			t.Fatalf("write state file: %v", err)
		}
	}
	fileIDs := map[string]uuid.UUID{
		"PROJECT_STATE": p713ProjectFile(t, h, proj, p709DefaultTypeID(t, h, "PROJECT_STATE")),
		"ARCHITECTURE":  p713ProjectFile(t, h, proj, p709DefaultTypeID(t, h, "ARCHITECTURE")),
		"PHASE_STATE":   p713PhaseFile(t, h, proj, phaseID, p709DefaultTypeID(t, h, "PHASE_STATE")),
		"ACTIVE_TASK":   p710ActiveTaskFile(t, h, files, proj, unit),
	}
	write(fileIDs["PROJECT_STATE"], spec.ProjectState)
	write(fileIDs["ARCHITECTURE"], spec.Architecture)
	write(fileIDs["PHASE_STATE"], spec.PhaseState)
	write(fileIDs["ACTIVE_TASK"], spec.ActiveTask)

	if _, err := dbrepo.NewStateInstructionRepository(raw).AppendVersion(ctx, stateID, p9StateInstr); err != nil {
		t.Fatalf("append state instruction: %v", err)
	}
	protocolID := p813Protocol(t, h, &proj, "p9-impl-schema", prompt.KindImplementationResponse, spec.Protocol)
	if _, err := dbrepo.NewResponseProtocolRepository(raw).CreateStateReference(ctx, stateID, protocolID, 1); err != nil {
		t.Fatalf("state protocol reference: %v", err)
	}
	templateID := p811Template(t, h, "p9-impl-"+uuid.NewString()[:8])
	if _, err := dbrepo.NewPromptTemplateVersionRepository(raw).AppendVersion(ctx, templateID, "Implement the active work unit.\n"); err != nil {
		t.Fatalf("append template: %v", err)
	}
	if _, err := p622Service(h, true).Initialize(ctx, proj.String(), mid); err != nil {
		t.Fatalf("initialize runtime: %v", err)
	}
	if _, err := h.DBExec(ctx, "UPDATE runtime_state SET phase_id = '"+phaseID.String()+"', work_unit_id = '"+unit.String()+"' WHERE project_id = '"+proj.String()+"'"); err != nil {
		t.Fatalf("bind runtime: %v", err)
	}
	snap, err := p718Svc(t, h).CreateExecutionSnapshot(ctx, proj.String())
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	return p9Position{ProjectID: proj, PhaseID: phaseID, UnitID: unit, SnapID: snap.ID, TemplateID: templateID, StateID: stateID,
		RoleID: role.ID, InstructionID: instr.ID, ProtocolID: protocolID, PlanID: planID, MachineID: uuid.MustParse(mid), Files: fileIDs}
}

// p9Composer is the P8-38 composer with Phase 9 component persistence.
func p9Composer(t *testing.T, h *Harness) *service.PromptComposerService {
	t.Helper()
	return p838Composer(t, h).WithComponents(dbrepo.NewCanonicalPromptComponentRepository(h.App.DB().Raw()))
}

// p9Compose composes + persists the canonical prompt at the position.
func p9Compose(t *testing.T, h *Harness, pos p9Position) *persistence.CanonicalPromptRecord {
	t.Helper()
	rec, err := p9Composer(t, h).ComposePrompt(context.Background(), prompt.ComposePromptRequest{
		ContextSnapshotID: pos.SnapID, TemplateID: pos.TemplateID, TemplateVersion: 1,
	})
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	return rec
}

// p9Source is a CompressorSource over test doubles.
type p9Source struct {
	compressor compression.Compressor
	semantic   compression.SemanticValidator
}

func (s p9Source) Compressor(name string) (compression.Compressor, error) {
	if s.compressor == nil {
		return nil, compression.NewError(compression.ErrorConfiguration, "no compressor")
	}
	return s.compressor, nil
}

func (s p9Source) SemanticValidator(name, model string) (compression.SemanticValidator, error) {
	if s.semantic == nil {
		return nil, compression.NewError(compression.ErrorConfiguration, "no semantic validator")
	}
	return s.semantic, nil
}

type p9NoSleep struct{}

func (p9NoSleep) Sleep(ctx context.Context, d time.Duration) error { return ctx.Err() }

// p9Service builds the compression service over the harness pool.
func p9Service(h *Harness, src service.CompressorSource) *service.CompressionService {
	raw := h.App.DB().Raw()
	return service.NewCompressionService(
		dbrepo.NewCanonicalPromptRepository(raw),
		dbrepo.NewCanonicalPromptComponentRepository(raw),
		dbrepo.NewSettingsRepository(raw),
		dbrepo.NewCompressionSettingsRepository(raw),
		dbrepo.NewCompressionInstructionRepository(raw),
		dbrepo.NewCompressionRepository(raw),
		src,
	).WithTxPool(raw).WithSleeper(p9NoSleep{})
}

// p9FakeFactory is a real provider factory pointed at the fake server.
func p9FakeFactory(srv *providertest.Server, keyEnv string) *compressionprovider.Factory {
	return compressionprovider.NewFactory(map[string]config.CompressionProviderConfig{
		"fake": {Kind: config.CompressionKindOpenAICompatible, BaseURL: srv.URL + "/v1", APIKeyEnv: keyEnv},
	})
}

// p9Configure sets the project's compression provider/model/context window
// and stores compression settings.
func p9Configure(t *testing.T, h *Harness, svc *service.CompressionService, projectID uuid.UUID, contextWindow int, st compression.Settings) compression.Settings {
	t.Helper()
	if _, err := h.DBExec(context.Background(), "UPDATE project_settings SET compression_provider = 'fake', compression_model = 'compressor-7b', context_window = "+itoa(contextWindow)+" WHERE project_id = '"+projectID.String()+"'"); err != nil {
		t.Fatalf("configure project settings: %v", err)
	}
	out, err := svc.UpdateCompressionSettings(context.Background(), projectID, st)
	if err != nil {
		t.Fatalf("store compression settings: %v", err)
	}
	return out
}

// p9Settings returns AUTO settings with an absolute target.
func p9Settings(target, attempts int) compression.Settings {
	s := compression.DefaultSettings()
	s.TargetTokens, s.MaxAttempts = target, attempts
	return s
}

// p9Tokens is the canonical estimate used for policy decisions.
func p9Tokens(rec *persistence.CanonicalPromptRecord) int {
	return compression.EstimateTokens(rec.Content)
}

// p9Snapshot captures the canonical row bytes for unchanged-ness checks.
func p9CanonicalRow(t *testing.T, h *Harness, id uuid.UUID) string {
	t.Helper()
	var sig string
	if err := h.DBScan(context.Background(), "SELECT md5(content) || hash || estimated_tokens::text || created_at::text FROM canonical_prompts WHERE id = '"+id.String()+"'", &sig); err != nil {
		t.Fatalf("canonical row: %v", err)
	}
	return sig
}

// p9RuntimeRow captures the project's runtime/workflow state.
func p9RuntimeRow(t *testing.T, h *Harness, projectID uuid.UUID) string {
	t.Helper()
	var sig string
	if err := h.DBScan(context.Background(), "SELECT row_to_json(r)::text FROM runtime_state r WHERE project_id = '"+projectID.String()+"'", &sig); err != nil {
		t.Fatalf("runtime row: %v", err)
	}
	return sig
}

func p9Count(t *testing.T, h *Harness, table, where string) int {
	t.Helper()
	var n int
	if err := h.DBScan(context.Background(), "SELECT count(*) FROM "+table+" WHERE "+where, &n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func p9MustContainAll(t *testing.T, label, content string, items ...string) {
	t.Helper()
	for _, it := range items {
		if !strings.Contains(content, it) {
			t.Fatalf("%s: missing %q", label, it)
		}
	}
}

func p9ComponentRepo(h *Harness) *dbrepo.CanonicalPromptComponentRepository {
	return dbrepo.NewCanonicalPromptComponentRepository(h.App.DB().Raw())
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
