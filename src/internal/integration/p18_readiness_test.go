package integration

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"overseer/internal/app"
	"overseer/internal/backup"
	"overseer/internal/config"
	"overseer/internal/service"
)

func p18ReadinessItem(r *service.ProductionReadiness, area, name string) service.ReadinessItem {
	for _, c := range r.Checks {
		if c.Area == area && c.Name == name {
			return c
		}
	}
	return service.ReadinessItem{}
}

// T18.207 / T18.208 — production readiness: a missing critical
// requirement (non-production profile, no backup) makes the report
// NOT_READY; satisfying one turns its check green; no secret value
// appears in the report.
func TestT18207ProductionReadiness(t *testing.T) {
	const csrf = "SENTINEL-ready-csrf-0123456789abcdef0123456789abcdef"
	t.Setenv("P18_READY_CSRF", csrf)
	r := p17NewRigWith(t, p16Options{config: func(c *config.Config) { c.Web.CSRFKeyEnv = "P18_READY_CSRF" }})
	var rep service.ProductionReadiness
	code, raw := r.b.api(http.MethodGet, "/api/v1/system/readiness", nil)
	if code != http.StatusOK || r.b.getJSON("/api/v1/system/readiness", &rep) != http.StatusOK {
		t.Fatalf("readiness %d %s", code, raw)
	}
	if rep.Status != service.ReadinessNotReady {
		t.Fatalf("status %s", rep.Status)
	}
	if p18ReadinessItem(&rep, "security", "profile").Status != service.CheckFail ||
		p18ReadinessItem(&rep, "backup", "recent_backup").Status != service.CheckFail ||
		p18ReadinessItem(&rep, "database", "server_version").Status != service.CheckPass ||
		p18ReadinessItem(&rep, "migrations", "schema").Status != service.CheckPass {
		t.Fatalf("checks: %+v", rep.Checks)
	}
	if strings.Contains(string(raw), csrf) {
		t.Fatal("a secret value appears in the readiness report")
	}
	// A backup satisfies the backup requirement.
	if _, err := backup.Backup(context.Background(), r.h.App.DB().Raw(), filepath.Join(t.TempDir(), "b"),
		backup.Options{AppVersion: app.Version, Profile: "test"}); err != nil {
		t.Fatal(err)
	}
	_ = r.b.getJSON("/api/v1/system/readiness", &rep)
	if p18ReadinessItem(&rep, "backup", "recent_backup").Status != service.CheckPass {
		t.Fatalf("backup check after a backup: %+v", p18ReadinessItem(&rep, "backup", "recent_backup"))
	}
	if p18ReadinessItem(&rep, "backup", "restore_verified").Status != service.CheckWarn {
		t.Fatal("unverified restore not warned")
	}
	// 18.209: the console shows the backend report.
	if code, page := r.b.page("/system"); code != http.StatusOK || !strings.Contains(page, "Production Readiness") ||
		!strings.Contains(page, rep.Status) || !strings.Contains(page, "restore_verified") {
		t.Fatalf("system page %d does not render the readiness report", code)
	}
}
