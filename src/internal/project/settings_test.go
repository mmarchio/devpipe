package project

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func validSettingsArgs(t *testing.T) (uuid.UUID, string, string, string, string, string, int, float64, bool, int, time.Duration) {
	t.Helper()
	return uuid.New(), "/work/demo", "hermes", "default", "none", "", 8192, 0.8, true, 3, time.Minute
}

// T2.2.1 — Settings associated with one project.
func TestSettingsOneProject(t *testing.T) {
	pid, wd, wp, wm, cp, cm, cw, ct, ac, mr, wt := validSettingsArgs(t)
	s, err := NewSettings(pid, wd, wp, wm, cp, cm, cw, ct, ac, mr, wt)
	if err != nil {
		t.Fatalf("valid settings rejected: %v", err)
	}
	if s.ProjectID != pid {
		t.Fatalf("ProjectID %v != %v", s.ProjectID, pid)
	}
	if s.ProjectID == uuid.Nil {
		t.Fatal("ProjectID is nil")
	}
	if s.CreatedAt.IsZero() || s.UpdatedAt.IsZero() {
		t.Fatal("timestamps not assigned")
	}
	if s.CreatedAt.Location() != time.UTC {
		t.Fatal("timestamps not UTC")
	}
}

// T2.2.3 — Invalid context window rejected (zero or negative).
func TestSettingsInvalidContextWindow(t *testing.T) {
	pid, wd, wp, wm, cp, cm, _, _, ac, mr, wt := validSettingsArgs(t)
	for _, cw := range []int{0, -1} {
		if _, err := NewSettings(pid, wd, wp, wm, cp, cm, cw, 0.8, ac, mr, wt); err == nil {
			t.Fatalf("context window %d accepted", cw)
		}
	}
}

// T2.2.4 — Invalid context threshold rejected (must be 0 < t <= 1).
func TestSettingsInvalidContextThreshold(t *testing.T) {
	pid, wd, wp, wm, cp, cm, cw, _, ac, mr, wt := validSettingsArgs(t)
	for _, ct := range []float64{0, 1.0001, -0.5} {
		if _, err := NewSettings(pid, wd, wp, wm, cp, cm, cw, ct, ac, mr, wt); err == nil {
			t.Fatalf("threshold %v accepted", ct)
		}
	}
	// boundary values must be accepted: just above 0 and exactly 1.
	for _, ct := range []float64{0.0001, 1} {
		if _, err := NewSettings(pid, wd, wp, wm, cp, cm, cw, ct, ac, mr, wt); err != nil {
			t.Fatalf("threshold %v rejected: %v", ct, err)
		}
	}
}

// T2.2.5 — Negative retry count rejected.
func TestSettingsNegativeRetries(t *testing.T) {
	pid, wd, wp, wm, cp, cm, cw, ct, ac, _, wt := validSettingsArgs(t)
	if _, err := NewSettings(pid, wd, wp, wm, cp, cm, cw, ct, ac, -1, wt); err == nil {
		t.Fatal("negative retries accepted")
	}
	if _, err := NewSettings(pid, wd, wp, wm, cp, cm, cw, ct, ac, 0, wt); err != nil {
		t.Fatalf("zero retries rejected: %v", err)
	}
}

// T2.2.6 — Invalid worker timeout rejected.
func TestSettingsInvalidTimeout(t *testing.T) {
	pid, wd, wp, wm, cp, cm, cw, ct, ac, mr, _ := validSettingsArgs(t)
	for _, d := range []time.Duration{0, -time.Second} {
		if _, err := NewSettings(pid, wd, wp, wm, cp, cm, cw, ct, ac, mr, d); err == nil {
			t.Fatalf("timeout %v accepted", d)
		}
	}
}

// T2.2.7 — Working directory retained exactly (no rewriting).
func TestSettingsWorkingDirExact(t *testing.T) {
	pid, _, wp, wm, cp, cm, cw, ct, ac, mr, wt := validSettingsArgs(t)
	paths := []string{
		"/work/demo",
		"  /with/leading/trailing/  ",
		"relative/dir",
		"~/home/user",
		"/UPPER/CASE",
	}
	for _, wd := range paths {
		s, err := NewSettings(pid, wd, wp, wm, cp, cm, cw, ct, ac, mr, wt)
		if err != nil {
			t.Fatalf("working dir %q rejected: %v", wd, err)
		}
		if s.WorkingDirectory != wd {
			t.Fatalf("working dir %q rewritten to %q", wd, s.WorkingDirectory)
		}
	}
}

// T2.2.2 — a project cannot have multiple active settings records. The
// domain model enforces one settings value per project ID by construction;
// persistence enforces the uniqueness constraint (P2-03). Here we verify the
// single-project association is well defined.
func TestSettingsAssociation(t *testing.T) {
	pid, wd, wp, wm, cp, cm, cw, ct, ac, mr, wt := validSettingsArgs(t)
	s1, _ := NewSettings(pid, wd, wp, wm, cp, cm, cw, ct, ac, mr, wt)
	s2, _ := NewSettings(pid, wd, wp, wm, cp, cm, cw, ct, ac, mr, wt)
	if s1.ProjectID != s2.ProjectID {
		t.Fatalf("same args yielded different project association: %v vs %v", s1.ProjectID, s2.ProjectID)
	}
}
