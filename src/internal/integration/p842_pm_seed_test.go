package integration

// PM live-seeder for P8-42: builds a composition-ready position, persists
// one canonical prompt through the P8-38 composer, and prints the project +
// prompt ids the live binary's provenance endpoint needs. Gated behind
// P842_SEED=1.
import (
	"os"
	"testing"

	"overseer/internal/prompt"
)

func TestP842Seeder(t *testing.T) {
	if os.Getenv("P842_SEED") != "1" {
		t.Skip("P842_SEED unset")
	}
	h := NewHarnessOn(t, baseDSN(), "overseer_p842_live")
	projectID, _, _, _, _, _, promptID := p842Fixture(t, h)
	println("PM842_PROJECT=" + projectID.String())
	println("PM842_PROMPT=" + promptID.String())
	_ = prompt.ComposePromptRequest{}
}
