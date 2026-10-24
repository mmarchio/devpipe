package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"overseer/internal/recovery"
)

// T18.211 — docs/recovery-decision-table.md is generated from the code
// tables (regenerate with OVERSEER_UPDATE_DOCS=1), so documentation, code
// and planner behavior cannot drift.
func TestT18211RecoveryTableDocumented(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "recovery-decision-table.md")
	want := "# Recovery decision table\n\nGenerated from `internal/recovery` (`DecisionTable`, `SideEffectMatrix`, the condition rules); do not edit by hand.\n\n" +
		"## Situations\n\n" + recovery.DecisionTableMarkdown() + "\n## Side effects\n\n" + recovery.SideEffectMarkdown() +
		"\n## Conditions\n\n" + recovery.ConditionsMarkdown() + "\n## Crash matrix\n\n" + recovery.CrashMatrixMarkdown()
	if os.Getenv("OVERSEER_UPDATE_DOCS") == "1" {
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatal("docs/recovery-decision-table.md differs from the code tables; regenerate it")
	}
}

// T18.210 — the runbook has an entry for every recovery condition and
// every required operational procedure.
func TestT18210RunbookCoversRecovery(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "runbook.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	for _, c := range recovery.Conditions() {
		if !strings.Contains(doc, "### `"+string(c)+"`") {
			t.Errorf("runbook has no entry for recovery condition %s", c)
		}
	}
	for _, h := range []string{"## Startup", "## Shutdown", "## Upgrade", "## Backup", "## Restore", "## Point-in-time recovery",
		"## Worker outage", "## Database outage", "## Disk full", "## DELIVERY_UNKNOWN", "## Repository mismatch",
		"## Checkpoint recovery", "## Audit verification failure", "## MCP server"} {
		if !strings.Contains(doc, h) {
			t.Errorf("runbook lacks %q", h)
		}
	}
}
