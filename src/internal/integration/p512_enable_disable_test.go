package integration

import (
	"context"
	"testing"

	"overseer/internal/dbrepo"
	"overseer/internal/service"
)

func newRoleSvc(t *testing.T, h *Harness) *service.RoleService {
	t.Helper()
	return service.NewRoleService(
		dbrepo.NewRoleRepository(h.App.DB().Raw()),
		dbrepo.NewSystemPromptRepository(h.App.DB().Raw()),
	)
}

func seedRoleWithPrompt(t *testing.T, h *Harness) (string, *dbrepo.SystemPromptRepository) {
	t.Helper()
	svc := newRoleSvc(t, h)
	promptRepo := dbrepo.NewSystemPromptRepository(h.App.DB().Raw())
	id := seedRole(t, h, nil, "engineer-x", "E")
	if _, err := promptRepo.AppendVersion(context.Background(), id, "initial system prompt"); err != nil {
		t.Fatalf("append v1 prompt: %v", err)
	}
	_ = svc
	return id, promptRepo
}

// T5.13.1 — Enabled role eligible.
func TestP512EnabledRoleEligible(t *testing.T) {
	h := NewHarness(t)
	svc := newRoleSvc(t, h)
	id := seedRole(t, h, nil, "engineer-x", "E")
	ok, err := svc.IsEligible(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("enabled role eligibility = false, want true")
	}
}

// T5.13.2 — Disabled role not eligible.
func TestP512DisabledRoleNotEligible(t *testing.T) {
	h := NewHarness(t)
	svc := newRoleSvc(t, h)
	id := seedRole(t, h, nil, "engineer-x", "E")
	if err := svc.Disable(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	ok, err := svc.IsEligible(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("disabled role eligibility = true, want false")
	}
}

// T5.13.3 — Disabling role doesn't delete prompt history.
func TestP512DisableRetainsPromptHistory(t *testing.T) {
	h := NewHarness(t)
	svc := newRoleSvc(t, h)
	id, promptRepo := seedRoleWithPrompt(t, h)
	if err := svc.Disable(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	versions, err := promptRepo.ListVersions(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 1 {
		t.Fatalf("prompt versions after disable = %d, want 1 (history retained)", len(versions))
	}
	if cur, _ := promptRepo.GetCurrentVersion(context.Background(), id); cur != 1 {
		t.Fatalf("current version after disable = %d, want 1", cur)
	}
}

// T5.13.4 — Re-enabling role restores eligibility.
func TestP512ReenableRestoresEligibility(t *testing.T) {
	h := NewHarness(t)
	svc := newRoleSvc(t, h)
	id := seedRole(t, h, nil, "engineer-x", "E")
	if err := svc.Disable(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if err := svc.Enable(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	ok, err := svc.IsEligible(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("re-enabled role eligibility = false, want true")
	}
}
