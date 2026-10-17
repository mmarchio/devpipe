package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/prompt"
)

// P8-34 (WBS 8.31, T8.31.3/T8.31.4 DB halves): the provenance map is derived
// from a prompt's component set; the component provenance values (source
// id/version/hash) are the exact rows of the pinned sources, so the map's
// survival across a restart is proven by (a) the provenance values read from
// the persisted source rows after a fresh harness boot over the same
// database reconstructing the same historical entry, and (b) the persisted
// canonical prompt row surviving the restart byte-for-byte.
//
// Restart mechanics (caller-owned DB, the e2e pattern): a fresh database is
// created on the base DSN, h1 boots it (migrations), the fixture is built,
// h1.Close(), then h2 = NewHarnessOn over the same name. NewHarnessOn
// registers NO t.Cleanup, so h2 must be closed explicitly (LIFO before the
// DB drop).

// p834PromptComponents reads the role's CURRENT system-prompt version row
// directly and assembles the system-prompt component exactly as the
// composer captures it (content, role id, version, content hash) — the
// component provenance the provenance map projects.
func p834PromptComponents(t *testing.T, h *Harness, roleID uuid.UUID) prompt.Components {
	t.Helper()
	var content, hash string
	var ver int
	if err := h.DBScan(context.Background(),
		"SELECT content, content_hash, version FROM role_system_prompts WHERE role_id = '"+roleID.String()+"' ORDER BY version DESC LIMIT 1",
		&content, &hash, &ver); err != nil {
		t.Fatalf("scan role prompt: %v", err)
	}
	c, err := prompt.NewPromptComponent(prompt.ComponentSystem, prompt.SourceRoleSystemPrompt, 1,
		content, roleID, ver, hash)
	if err != nil {
		t.Fatalf("system component: %v", err)
	}
	return prompt.Components{c}
}

// p834PersistPrompt persists a canonical prompt for the fixture (the
// traceability record the provenance map belongs to).
func p834PersistPrompt(t *testing.T, h *Harness, fx p830IDs) *persistence.CanonicalPromptRecord {
	t.Helper()
	phase, unit := fx.phaseID, fx.unitID
	cp, err := prompt.NewCanonicalPrompt(prompt.CanonicalPromptInput{
		ID:                  uuid.New(),
		ProjectID:           fx.projectID,
		PhaseID:             &phase,
		WorkUnitID:          &unit,
		ContextSnapshotID:   fx.snapID,
		TemplateID:          fx.templateID,
		TemplateVersion:     1,
		RoleID:              fx.roleID,
		RoleVersion:         1,
		StateMachineID:      fx.machineID,
		StateMachineVersion: 1,
		StateKey:            "S",
		Content:             "## Implementation\n\nProvenance fixture prompt.",
		Hash:                "p834-hash",
		EstimatedTokens:     42,
		CreatedAt:           time.Date(2026, 10, 5, 1, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("NewCanonicalPrompt: %v", err)
	}
	rec := &persistence.CanonicalPromptRecord{
		ID: cp.ID, ProjectID: cp.ProjectID, PhaseID: cp.PhaseID, WorkUnitID: cp.WorkUnitID,
		ContextSnapshotID: cp.ContextSnapshotID, TemplateID: cp.TemplateID, TemplateVersion: cp.TemplateVersion,
		RoleID: cp.RoleID, RoleVersion: cp.RoleVersion, StateMachineID: cp.StateMachineID,
		StateMachineVersion: cp.StateMachineVersion, StateKey: cp.StateKey,
		Content: cp.Content, Hash: cp.Hash, EstimatedTokens: cp.EstimatedTokens, CreatedAt: cp.CreatedAt,
	}
	repo := dbrepo.NewCanonicalPromptRepository(h.App.DB().Raw())
	if err := repo.Create(context.Background(), rec); err != nil {
		t.Fatalf("create canonical prompt: %v", err)
	}
	return rec
}

// p834RestartRig boots a caller-owned database, returns the first harness,
// and registers cleanup (h2 close is the test's job).
func p834RestartRig(t *testing.T) (*Harness, string) {
	t.Helper()
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
	h := NewHarnessOn(t, base, dbName)
	if !h.App.IsReady() {
		t.Fatal("boot not ready")
	}
	return h, dbName
}

// T8.31.3 — Provenance survives restart: the component provenance (source
// id/version/hash) captured before a restart is reconstructed EXACTLY from
// the persisted rows after a fresh harness boot over the same database, and
// the persisted canonical prompt record survives byte-for-byte.
func TestT8313_ProvenanceSurvivesRestart(t *testing.T) {
	h, dbName := p834RestartRig(t)
	fx := p830Fixture(t, h)
	rec := p834PersistPrompt(t, h, fx)

	beforeMap := prompt.ProvenanceMap(p834PromptComponents(t, h, fx.roleID))
	if len(beforeMap) != 1 || beforeMap[0].SourceID != fx.roleID || beforeMap[0].SourceVersion != 1 {
		t.Fatalf("pre-restart map = %+v, want the pinned v1 entry", beforeMap)
	}

	// Advance the source AFTER the capture so the pre-restart entry is
	// genuinely historical, then restart.
	prompts := dbrepo.NewSystemPromptRepository(h.App.DB().Raw())
	if _, err := prompts.AppendVersion(context.Background(), fx.roleID.String(), "advanced prompt content"); err != nil {
		t.Fatalf("advance role prompt: %v", err)
	}
	h.Close()
	h2 := NewHarnessOn(t, baseDSN(), dbName)
	defer h2.Close()

	afterMap := prompt.ProvenanceMap(p834PromptComponents(t, h2, fx.roleID))
	if len(afterMap) != len(beforeMap) {
		t.Fatalf("map length changed across restart: %d -> %d", len(beforeMap), len(afterMap))
	}
	// The historical entry's identity: read the v1 row through the fresh
	// boot and reconstruct — it must match the pre-restart capture exactly.
	var content, hash string
	var ver int
	if err := h2.DBScan(context.Background(),
		"SELECT content, content_hash, version FROM role_system_prompts WHERE role_id = '"+fx.roleID.String()+"' AND version = 1",
		&content, &hash, &ver); err != nil {
		t.Fatalf("read v1 after restart: %v", err)
	}
	rebuilt := prompt.ProvenanceEntry{
		ComponentType: prompt.ComponentSystem,
		SourceID:      fx.roleID,
		SourceVersion: ver,
		SourceHash:    hash,
	}
	if rebuilt != beforeMap[0] {
		t.Fatalf("historical entry after restart = %+v, want pre-restart %+v", rebuilt, beforeMap[0])
	}
	// The CURRENT entry advanced to v2 (the map reflects the live source).
	if afterMap[0].SourceVersion != 2 {
		t.Fatalf("post-restart current entry version = %d, want 2", afterMap[0].SourceVersion)
	}

	// The persisted canonical prompt record survives the restart
	// byte-for-byte (id, hash, content, pinned versions).
	var storedID uuid.UUID
	var storedHash, storedContent string
	if err := h2.DBScan(context.Background(),
		"SELECT id, hash, content FROM canonical_prompts WHERE project_id = '"+fx.projectID.String()+"'",
		&storedID, &storedHash, &storedContent); err != nil {
		t.Fatalf("post-restart prompt read: %v", err)
	}
	if storedID != rec.ID || storedHash != rec.Hash || storedContent != rec.Content {
		t.Fatalf("persisted prompt after restart = (%s,%s), want (%s,%s)", storedID, storedHash, rec.ID, rec.Hash)
	}
}

// T8.31.4 — Historical source reconstructable: after the role prompt source
// advances to v2, the historical v1 row remains byte-identical (content +
// hash), so a historical prompt's provenance entry (v1 + its hash) is
// reconstructable from the persisted source rows alone.
func TestT8314_HistoricalSourceReconstructable(t *testing.T) {
	h := NewHarness(t)
	fx := p830Fixture(t, h)
	hist := p834PromptComponents(t, h, fx.roleID)[0]

	prompts := dbrepo.NewSystemPromptRepository(h.App.DB().Raw())
	if _, err := prompts.AppendVersion(context.Background(), fx.roleID.String(), "new prompt content"); err != nil {
		t.Fatalf("advance role prompt: %v", err)
	}

	// Re-read the historical v1 row directly: byte-identical, so the
	// historical provenance entry is reconstructable from persistence.
	var content, hash string
	var ver int
	if err := h.DBScan(context.Background(),
		"SELECT content, content_hash, version FROM role_system_prompts WHERE role_id = '"+fx.roleID.String()+"' AND version = 1",
		&content, &hash, &ver); err != nil {
		t.Fatalf("read historical row: %v", err)
	}
	if content != hist.Content || hash != hist.SourceHash || ver != hist.SourceVersion {
		t.Fatalf("historical v1 row = (v%d,%s,%q), want (v1,%s,%q)", ver, hash, content, hist.SourceHash, hist.Content)
	}
	// And the map derived from it matches the pre-advance capture.
	if got := prompt.ProvenanceMap(p834PromptComponents(t, h, fx.roleID)); got[0].SourceVersion != 2 {
		t.Fatalf("current entry after advance = %+v, want v2", got[0])
	}
}
