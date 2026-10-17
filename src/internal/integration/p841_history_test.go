package integration

// P8-41 (WBS 8.36): the prompt history API over real PostgreSQL. Two read-only
// endpoints: GET /api/v1/projects/{project}/prompts (list, optional conjunctive
// filters phase / work unit / state / role / template) and
// GET /api/v1/projects/{project}/prompts/{prompt} (one prompt, exact content).
// Project isolation is enforced (T8.36.4): a prompt persisted under one
// project is never retrievable through another project's surface.
//
// Task-level choices (stated so TESTER/PM can verify; the WBS does not fix
// them):
//   - FILTERS are conjunctive query-string params: ?phase=, ?work_unit=,
//     ?state=, ?role=, ?template=. state filters on the persisted state_key,
//     role/template on their ids. Unknown values yield an empty list, not an
//     error (a filter is a predicate, not a lookup).
//   - BY-ID SCOPE: the {prompt} id must belong to the URL's project; an id
//     existing in ANOTHER project is a 404 on this project's surface.
//   - LIST ORDER: created_at ASC, id ASC (the repository's existing
//     creation-order contract) — stable and deterministic.
//   - LIST DTO wraps the items under {"prompts":[...],"count":N}; the item
//     DTO carries the full provenance metadata (T8.36.3).

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
)

// p841Position resolves the fixture's position ids (project, role, phase,
// snapshot, template, unit) for the P8-38 composition position.
func p841Position(t *testing.T, h *Harness) (projectID, roleID, phaseID, snapID, templateID, unitID uuid.UUID) {
	t.Helper()
	projectID, roleID, _, _, _ = p718Rig(t, h)
	unitID = p838ProjectUnit(t, h, projectID)
	ctx := context.Background()
	if _, err := h.DBExec(ctx, "UPDATE work_units SET objective = 'Implement the fixture work unit' WHERE id = '"+unitID.String()+"'"); err != nil {
		t.Fatalf("set unit objective: %v", err)
	}
	if _, err := h.DBExec(ctx, "UPDATE runtime_state SET work_unit_id = '"+unitID.String()+"' WHERE project_id = '"+projectID.String()+"'"); err != nil {
		t.Fatalf("bind runtime to unit: %v", err)
	}
	protocolID := p813Protocol(t, h, nil, "impl-response", "IMPLEMENTATION_RESPONSE", "impl schema body")
	raw := h.App.DB().Raw()
	if _, err := dbrepo.NewResponseProtocolRepository(raw).CreateStateReference(ctx, p841CurrentState(t, h, projectID), protocolID, 1); err != nil {
		t.Fatalf("create state reference: %v", err)
	}
	templateID = p811Template(t, h, "p841-impl")
	if _, err := dbrepo.NewPromptTemplateVersionRepository(raw).AppendVersion(ctx, templateID, "Task: {{work_unit.task}}"); err != nil {
		t.Fatalf("append template: %v", err)
	}
	snap, err := p718Svc(t, h).CreateExecutionSnapshot(ctx, projectID.String())
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	snapID = snap.ID
	phaseID = p718PhaseID(t, h, projectID)
	return
}

// p841CurrentState returns the fixture project's current state id.
func p841CurrentState(t *testing.T, h *Harness, projectID uuid.UUID) uuid.UUID {
	t.Helper()
	var stateID uuid.UUID
	if err := h.DBScan(context.Background(),
		"SELECT current_state_id FROM runtime_state WHERE project_id = '"+projectID.String()+"'", &stateID); err != nil {
		t.Fatalf("scan current state: %v", err)
	}
	return stateID
}

// p841Machine resolves the fixture project's runtime machine provenance.
func p841Machine(t *testing.T, h *Harness, projectID uuid.UUID) (id uuid.UUID, version int) {
	t.Helper()
	var v int32
	if err := h.DBScan(context.Background(),
		"SELECT state_machine_id, state_machine_version FROM runtime_state WHERE project_id = '"+projectID.String()+"'", &id, &v); err != nil {
		t.Fatalf("scan machine: %v", err)
	}
	return id, int(v)
}

// p841FixtureRecord builds one canonical-prompt record over the fixture
// position, discriminated by the caller-supplied role/template/state.
func p841FixtureRecord(t *testing.T, h *Harness, projectID, snapID, templateID, unitID, roleID uuid.UUID, templateVersion int, roleVersion int, stateKey, content, hash string) *persistence.CanonicalPromptRecord {
	t.Helper()
	phaseID := p718PhaseID(t, h, projectID)
	machineID, machineVersion := p841Machine(t, h, projectID)
	return &persistence.CanonicalPromptRecord{
		ID:                  uuid.New(),
		ProjectID:           projectID,
		PhaseID:             &phaseID,
		WorkUnitID:          &unitID,
		ContextSnapshotID:   snapID,
		TemplateID:          templateID,
		TemplateVersion:     templateVersion,
		RoleID:              roleID,
		RoleVersion:         roleVersion,
		StateMachineID:      machineID,
		StateMachineVersion: machineVersion,
		StateKey:            stateKey,
		Content:             content,
		Hash:                hash,
	}
}

// p841Persist seeds the given records directly through the P8-30 repository
// (the canonical persistence path) and returns their ids in order.
func p841Persist(t *testing.T, h *Harness, recs ...*persistence.CanonicalPromptRecord) []uuid.UUID {
	t.Helper()
	repo := dbrepo.NewCanonicalPromptRepository(h.App.DB().Raw())
	ids := make([]uuid.UUID, 0, len(recs))
	for _, rec := range recs {
		if err := repo.Create(context.Background(), rec); err != nil {
			t.Fatalf("persist canonical prompt: %v", err)
		}
		ids = append(ids, rec.ID)
	}
	return ids
}

// p841PromptDTO is the expected wire shape of one history item.
type p841PromptDTO struct {
	ID                  string  `json:"id"`
	PhaseID             *string `json:"phase_id"`
	WorkUnitID          *string `json:"work_unit_id"`
	ContextSnapshotID   string  `json:"context_snapshot_id"`
	TemplateID          string  `json:"template_id"`
	TemplateVersion     int     `json:"template_version"`
	RoleID              string  `json:"role_id"`
	RoleVersion         int     `json:"role_version"`
	StateMachineID      string  `json:"state_machine_id"`
	StateMachineVersion int     `json:"state_machine_version"`
	StateKey            string  `json:"state_key"`
	Content             string  `json:"content"`
	Hash                string  `json:"hash"`
	EstimatedTokens     int     `json:"estimated_tokens"`
	CreatedAt           string  `json:"created_at"`
}

// p841HTTP runs one REST call against the harness server.
func p841HTTP(t *testing.T, h *Harness, method, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, h.BaseURL+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func p841List(t *testing.T, h *Harness, path string) []p841PromptDTO {
	t.Helper()
	code, body := p841HTTP(t, h, "GET", path)
	if code != 200 {
		t.Fatalf("list %q code = %d, body %s", path, code, body)
	}
	var list struct {
		Prompts []p841PromptDTO `json:"prompts"`
		Count   int             `json:"count"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("list %q decode: %v", path, err)
	}
	return list.Prompts
}

// T8.36.1 — Prompt retrieved: the list endpoint returns the project's persisted
// prompts and the by-id endpoint returns one.
func TestT8361PromptRetrieved(t *testing.T) {
	h := NewHarness(t)
	projectID, roleID, _, snapID, templateID, unitID := p841Position(t, h)
	rec := p841FixtureRecord(t, h, projectID, snapID, templateID, unitID, roleID, 1, 1, "S", "c1", "hash-c1")
	ids := p841Persist(t, h, rec)

	items := p841List(t, h, "/api/v1/projects/"+projectID.String()+"/prompts")
	if len(items) != 1 || items[0].ID != ids[0].String() {
		t.Fatalf("T8.36.1 list = %d items, want 1 with id %s", len(items), ids[0].String())
	}

	code, body := p841HTTP(t, h, "GET", "/api/v1/projects/"+projectID.String()+"/prompts/"+ids[0].String())
	if code != 200 {
		t.Fatalf("T8.36.1 by-id code = %d, body %s", code, body)
	}
	var one p841PromptDTO
	if err := json.Unmarshal([]byte(body), &one); err != nil {
		t.Fatalf("T8.36.1 decode by-id: %v", err)
	}
	if one.ID != ids[0].String() {
		t.Fatalf("T8.36.1 by-id id = %s, want %s", one.ID, ids[0].String())
	}
}

// T8.36.2 — Content retrieved exactly: the persisted bytes come back
// byte-identical through BOTH endpoints (no re-rendering, no normalization).
func TestT8362ContentExact(t *testing.T) {
	h := NewHarness(t)
	projectID, roleID, _, snapID, templateID, unitID := p841Position(t, h)
	content := "exact-content:\nline two\nwith special chars: \"quoted\" and \\backslash\\ and unicode — é"
	rec := p841FixtureRecord(t, h, projectID, snapID, templateID, unitID, roleID, 1, 1, "S", content, "hash-x")
	ids := p841Persist(t, h, rec)

	code, body := p841HTTP(t, h, "GET", "/api/v1/projects/"+projectID.String()+"/prompts/"+ids[0].String())
	if code != 200 {
		t.Fatalf("T8.36.2 by-id code = %d, body %s", code, body)
	}
	var one p841PromptDTO
	if err := json.Unmarshal([]byte(body), &one); err != nil {
		t.Fatalf("T8.36.2 decode: %v", err)
	}
	if one.Content != content {
		t.Fatalf("T8.36.2 by-id content not byte-exact:\n got %q\nwant %q", one.Content, content)
	}

	items := p841List(t, h, "/api/v1/projects/"+projectID.String()+"/prompts")
	if len(items) != 1 || items[0].Content != content {
		t.Fatalf("T8.36.2 list content not byte-exact")
	}
}

// T8.36.3 — Metadata included: every provenance field rides on the wire
// (snapshot/template/role/machine identities + versions, state key, hash,
// estimated tokens).
func TestT8363MetadataIncluded(t *testing.T) {
	h := NewHarness(t)
	projectID, roleID, _, snapID, templateID, unitID := p841Position(t, h)
	rec := p841FixtureRecord(t, h, projectID, snapID, templateID, unitID, roleID, 1, 1, "S", "c3", "hash-c3")
	ids := p841Persist(t, h, rec)

	code, body := p841HTTP(t, h, "GET", "/api/v1/projects/"+projectID.String()+"/prompts/"+ids[0].String())
	if code != 200 {
		t.Fatalf("T8.36.3 code = %d, body %s", code, body)
	}
	var one p841PromptDTO
	if err := json.Unmarshal([]byte(body), &one); err != nil {
		t.Fatalf("T8.36.3 decode: %v", err)
	}
	if one.ContextSnapshotID != snapID.String() {
		t.Fatalf("T8.36.3 snapshot id = %s, want %s", one.ContextSnapshotID, snapID.String())
	}
	if one.TemplateID != templateID.String() || one.TemplateVersion != 1 {
		t.Fatalf("T8.36.3 template = %s v%d, want %s v1", one.TemplateID, one.TemplateVersion, templateID.String())
	}
	if one.RoleID != roleID.String() || one.RoleVersion != 1 {
		t.Fatalf("T8.36.3 role = %s v%d, want %s v1", one.RoleID, one.RoleVersion, roleID.String())
	}
	if one.StateKey != "S" {
		t.Fatalf("T8.36.3 state key = %q, want S", one.StateKey)
	}
	if one.Hash != "hash-c3" {
		t.Fatalf("T8.36.3 hash = %q", one.Hash)
	}
	if one.StateMachineID == "" || one.StateMachineVersion <= 0 {
		t.Fatalf("T8.36.3 machine provenance missing: %q v%d", one.StateMachineID, one.StateMachineVersion)
	}
	if one.CreatedAt == "" {
		t.Fatalf("T8.36.3 created_at missing")
	}
}

// T8.36.4 — Project isolation enforced: prompts persisted under project A are
// never visible through project B's surface (list is empty; by-id is 404).
func TestT8364ProjectIsolation(t *testing.T) {
	h := NewHarness(t)
	projectA, roleA, _, snapA, templateA, unitA := p841Position(t, h)
	rec := p841FixtureRecord(t, h, projectA, snapA, templateA, unitA, roleA, 1, 1, "S", "iso", "hash-iso")
	ids := p841Persist(t, h, rec)

	projB := p841FreshProject(t, h)
	items := p841List(t, h, "/api/v1/projects/"+projB.String()+"/prompts")
	if len(items) != 0 {
		t.Fatalf("T8.36.4 B list sees %d prompts, want 0", len(items))
	}
	code, _ := p841HTTP(t, h, "GET", "/api/v1/projects/"+projB.String()+"/prompts/"+ids[0].String())
	if code != http.StatusNotFound {
		t.Fatalf("T8.36.4 B by-id code = %d, want 404", code)
	}
	code, _ = p841HTTP(t, h, "GET", "/api/v1/projects/"+projectA.String()+"/prompts/"+ids[0].String())
	if code != 200 {
		t.Fatalf("T8.36.4 A by-id code = %d, want 200", code)
	}
}

// T8.36.5 — Filters work: each filter (phase / work unit / state / role /
// template) selects exactly the matching rows; combined filters are
// conjunctive; unknown values yield an empty list.
func TestT8365FiltersWork(t *testing.T) {
	h := NewHarness(t)
	projectID, roleA, phaseID, snapID, templateID, unitID := p841Position(t, h)
	roleB := p712ProjectRole(t, h, projectID, "P841-ROLE-B")
	otherTemplateID := p811Template(t, h, "p841-second-tpl")
	tmplVer := dbrepo.NewPromptTemplateVersionRepository(h.App.DB().Raw())
	if _, err := tmplVer.AppendVersion(context.Background(), otherTemplateID, "second"); err != nil {
		t.Fatalf("append: %v", err)
	}

	p1 := p841FixtureRecord(t, h, projectID, snapID, templateID, unitID, roleA, 1, 1, "S", "p1", "hash-p1")
	p2 := p841FixtureRecord(t, h, projectID, snapID, templateID, unitID, roleB, 1, 1, "S", "p2", "hash-p2")
	p3 := p841FixtureRecord(t, h, projectID, snapID, templateID, unitID, roleA, 1, 1, "T", "p3", "hash-p3")
	p4 := p841FixtureRecord(t, h, projectID, snapID, otherTemplateID, unitID, roleB, 1, 1, "T", "p4", "hash-p4")
	p841Persist(t, h, p1, p2, p3, p4)

	basePath := "/api/v1/projects/" + projectID.String() + "/prompts"
	idsOf := func(items []p841PromptDTO) map[string]bool {
		m := map[string]bool{}
		for _, it := range items {
			m[it.ID] = true
		}
		return m
	}
	expect := func(name string, q url.Values, want ...uuid.UUID) {
		t.Helper()
		path := basePath
		if len(q) > 0 {
			path += "?" + q.Encode()
		}
		got := idsOf(p841List(t, h, path))
		wantSet := map[string]bool{}
		for _, w := range want {
			wantSet[w.String()] = true
		}
		if len(got) != len(wantSet) {
			t.Fatalf("%s: got %v, want %v", name, got, wantSet)
		}
		for k := range wantSet {
			if !got[k] {
				t.Fatalf("%s: missing %s; got %v", name, k, got)
			}
		}
	}

	// role=roleA selects p1+p3 (roleB rows excluded).
	expect("role A", url.Values{"role": {roleA.String()}}, p1.ID, p3.ID)
	// role=roleB selects p2+p4.
	expect("role B", url.Values{"role": {roleB.String()}}, p2.ID, p4.ID)
	// template selects p1-p3 (other template row excluded).
	expect("template", url.Values{"template": {templateID.String()}}, p1.ID, p2.ID, p3.ID)
	// state=T selects p3+p4.
	expect("state T", url.Values{"state": {"T"}}, p3.ID, p4.ID)
	// work unit selects all four (all bound to the fixture unit).
	expect("work unit", url.Values{"work_unit": {unitID.String()}}, p1.ID, p2.ID, p3.ID, p4.ID)
	// phase selects all four (all bound to the fixture phase).
	expect("phase", url.Values{"phase": {phaseID.String()}}, p1.ID, p2.ID, p3.ID, p4.ID)
	// combined (conjunctive): role B + state T -> p4 only.
	expect("role B + state T", url.Values{"role": {roleB.String()}, "state": {"T"}}, p4.ID)
	// combined: role A + other template -> none (no row matches both).
	expect("role A + template other", url.Values{"role": {roleA.String()}, "template": {otherTemplateID.String()}})
	// unknown filter values -> empty list, not an error.
	expect("unknown role", url.Values{"role": {uuid.New().String()}})
	expect("unknown template", url.Values{"template": {uuid.New().String()}})
}

// p841FreshProject creates a second, independent project (isolation target).
func p841FreshProject(t *testing.T, h *Harness) uuid.UUID {
	t.Helper()
	idStr := createPhaseProject(t, h)
	id, err := uuid.Parse(idStr)
	if err != nil {
		t.Fatalf("parse project id: %v", err)
	}
	return id
}

// T8.36.x REST contract — negative surface: unknown prompt id on the correct
// project is a 404 with the structured error body; a malformed filter param is
// a 400; a malformed id in a filter is a 400.
func TestT836RESTContractNegatives(t *testing.T) {
	h := NewHarness(t)
	projectID, roleID, _, snapID, templateID, unitID := p841Position(t, h)
	rec := p841FixtureRecord(t, h, projectID, snapID, templateID, unitID, roleID, 1, 1, "S", "c", "h")
	p841Persist(t, h, rec)

	// Unknown id, correct project: 404.
	code, body := p841HTTP(t, h, "GET", "/api/v1/projects/"+projectID.String()+"/prompts/"+uuid.New().String())
	if code != http.StatusNotFound {
		t.Fatalf("unknown id code = %d, body %s", code, body)
	}

	// Malformed id in a filter param: 400.
	code, body = p841HTTP(t, h, "GET", "/api/v1/projects/"+projectID.String()+"/prompts?role=not-a-uuid")
	if code != http.StatusBadRequest {
		t.Fatalf("malformed filter code = %d, body %s", code, body)
	}

	// Malformed project path segment: 400 (invalid input from the service).
	code, _ = p841HTTP(t, h, "GET", "/api/v1/projects/not-a-uuid/prompts")
	if code != http.StatusBadRequest {
		t.Fatalf("malformed project code = %d", code)
	}
}
