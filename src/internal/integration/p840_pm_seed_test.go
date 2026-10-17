package integration

// PM live-seeder for P8-40: builds a complete composition-ready position
// (p718Rig + unit objective + runtime binding + protocol + template) against
// the live binary's database (a shared "overseer_p840_live" db) and prints the
// ids the live binary's REST surface needs. Gated behind P840_SEED=1 so the
// normal suite never runs it.
import (
	"context"
	"fmt"
	"os"
	"testing"

	"overseer/internal/dbrepo"
)

func TestP840Seeder(t *testing.T) {
	if os.Getenv("P840_SEED") != "1" {
		t.Skip("P840_SEED unset")
	}
	h := NewHarnessOn(t, baseDSN(), "overseer_p840_live")
	t.Cleanup(func() {
		ctx := context.Background()
		_ = h.App.Shutdown(ctx)
	})
	projectID, _, stateID, _, _ := p718Rig(t, h)
	ctx := context.Background()
	unitID := p838ProjectUnit(t, h, projectID)
	if _, err := h.DBExec(ctx, "UPDATE work_units SET objective = 'Implement the fixture work unit' WHERE id = '"+unitID.String()+"'"); err != nil {
		t.Fatalf("set unit objective: %v", err)
	}
	if _, err := h.DBExec(ctx, "UPDATE runtime_state SET work_unit_id = '"+unitID.String()+"' WHERE project_id = '"+projectID.String()+"'"); err != nil {
		t.Fatalf("bind runtime to unit: %v", err)
	}
	protocolID := p813Protocol(t, h, nil, "impl-response", "IMPLEMENTATION_RESPONSE", "impl schema body")
	raw := h.App.DB().Raw()
	if _, err := dbrepo.NewResponseProtocolRepository(raw).CreateStateReference(ctx, stateID, protocolID, 1); err != nil {
		t.Fatalf("create state reference: %v", err)
	}
	templateID := p811Template(t, h, "p840-impl")
	if _, err := dbrepo.NewPromptTemplateVersionRepository(raw).AppendVersion(ctx, templateID, "Task: {{work_unit.task}}\nProto: {{response_protocol.schema}}"); err != nil {
		t.Fatalf("append template: %v", err)
	}
	fmt.Println("PM_SEED_PROJECT=" + projectID.String())
	fmt.Println("PM_SEED_STATE=" + stateID.String())
	fmt.Println("PM_SEED_TEMPLATE=" + templateID.String())
}
