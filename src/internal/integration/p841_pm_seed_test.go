package integration

// PM live-seeder for P8-41: builds a composition-ready position, persists one
// canonical prompt through the P8-30 repository, and prints the project +
// prompt ids the live binary's REST surface needs. Gated behind P841_SEED=1.
import (
	"context"
	"os"
	"testing"

	"overseer/internal/dbrepo"
)

func TestP841Seeder(t *testing.T) {
	if os.Getenv("P841_SEED") != "1" {
		t.Skip("P841_SEED unset")
	}
	h := NewHarnessOn(t, baseDSN(), "overseer_p841_live")
	t.Cleanup(func() {
		_ = h.App.Shutdown(context.Background())
	})
	projectID, roleID, phaseID, snapID, templateID, unitID := p841Position(t, h)
	rec := p841FixtureRecord(t, h, projectID, snapID, templateID, unitID, roleID, 1, 1, "S", "live content — é", "hash-live")
	ids := p841Persist(t, h, rec)
	_ = phaseID
	println("PM841_PROJECT=" + projectID.String())
	println("PM841_PROMPT=" + ids[0].String())
	_ = dbrepo.NewCanonicalPromptRepository
}
