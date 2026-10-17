package prompt

import (
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

var contentTS = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

// T8.3.1 — Multiline template stored exactly.
func TestT831MultilineStoredExactly(t *testing.T) {
	src := "# Objective\n{{work_unit.objective}}\n\n# Instructions\n{{state.instructions}}\n\n# Acceptance Criteria\n{{work_unit.acceptance_criteria}}\n"
	series, err := NewVersionSeries(uuid.New(), src, contentTS)
	if err != nil {
		t.Fatalf("NewVersionSeries: %v", err)
	}
	v1, err := series.Current()
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if v1.Content != src {
		t.Fatalf("stored content differs from source:\n got %q\nwant %q", v1.Content, src)
	}
}

// T8.3.2 — Unicode retained.
func TestT832UnicodeRetained(t *testing.T) {
	for _, src := range []string{
		"# Plan\nÜnïcødé — 中文 prompt\n",
		"naïve café ☕ → 完了\n",
	} {
		series, err := NewVersionSeries(uuid.New(), src, contentTS)
		if err != nil {
			t.Fatalf("NewVersionSeries: %v", err)
		}
		v1, err := series.Current()
		if err != nil {
			t.Fatalf("Current: %v", err)
		}
		if v1.Content != src {
			t.Fatalf("unicode content mangled:\n got %q\nwant %q", v1.Content, src)
		}
	}
}

// T8.3.3 — Template syntax retained (the series stores source, never parses
// or renders it — no external side effects).
func TestT833TemplateSyntaxRetained(t *testing.T) {
	src := "{{state.files}}\n{{ work_unit.name }}\n{{unknown.var}}\n"
	series, err := NewVersionSeries(uuid.New(), src, contentTS)
	if err != nil {
		t.Fatalf("NewVersionSeries: %v", err)
	}
	v1, err := series.Current()
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if v1.Content != src {
		t.Fatalf("syntax mangled:\n got %q\nwant %q", v1.Content, src)
	}
	for _, frag := range []string{"{{state.files}}", "{{ work_unit.name }}", "{{unknown.var}}"} {
		if !strings.Contains(v1.Content, frag) {
			t.Fatalf("expected fragment %q in stored content", frag)
		}
	}
}

// T8.3.4 — Empty required template rejected.
func TestT834EmptyRequiredTemplateRejected(t *testing.T) {
	if _, err := NewVersionSeries(uuid.New(), "", contentTS); err == nil {
		t.Fatalf("empty initial content accepted")
	}
	series, err := NewVersionSeries(uuid.New(), "v1 content\n", contentTS)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := series.Append("", contentTS); err == nil {
		t.Fatalf("empty append accepted")
	}
	cur, err := series.Current()
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if cur.Version != 1 {
		t.Fatalf("rejected append must not advance the version, got %d", cur.Version)
	}
}

// T8.4.1 — Initial content creates v1.
func TestT841InitialContentCreatesV1(t *testing.T) {
	id := uuid.New()
	series, err := NewVersionSeries(id, "# Initial\n", contentTS)
	if err != nil {
		t.Fatalf("NewVersionSeries: %v", err)
	}
	v1, err := series.Current()
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if v1.Version != 1 {
		t.Fatalf("initial version = %d, want 1", v1.Version)
	}
	if v1.TemplateID != id {
		t.Fatalf("version row must carry the template id")
	}
	if !v1.CreatedAt.Equal(contentTS) {
		t.Fatalf("created_at = %v, want %v", v1.CreatedAt, contentTS)
	}
}

// T8.4.2 — Edit creates v2.
func TestT842EditCreatesV2(t *testing.T) {
	series, err := NewVersionSeries(uuid.New(), "# v1\n", contentTS)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	v2, err := series.Append("# v2 edited\n", contentTS)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if v2.Version != 2 {
		t.Fatalf("edit version = %d, want 2", v2.Version)
	}
	if v2.Content != "# v2 edited\n" {
		t.Fatalf("edit content = %q", v2.Content)
	}
	if series.Len() != 2 {
		t.Fatalf("series length = %d, want 2", series.Len())
	}
}

// T8.4.3 — v1 remains retrievable after the edit.
func TestT843V1RemainsRetrievable(t *testing.T) {
	series, err := NewVersionSeries(uuid.New(), "# original v1\n", contentTS)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := series.Append("# edited v2\n", contentTS); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, err := series.GetVersion(1)
	if err != nil {
		t.Fatalf("GetVersion(1): %v", err)
	}
	if got.Content != "# original v1\n" {
		t.Fatalf("v1 content = %q, want the original", got.Content)
	}
	if got.Version != 1 {
		t.Fatalf("retrieved version = %d, want 1", got.Version)
	}
	if _, err := series.GetVersion(3); err == nil {
		t.Fatalf("unknown version must be rejected")
	}
	if _, err := series.GetVersion(0); err == nil {
		t.Fatalf("version 0 must be rejected")
	}
}

// T8.4.4 — Current version advances.
func TestT844CurrentVersionAdvances(t *testing.T) {
	series, err := NewVersionSeries(uuid.New(), "v1\n", contentTS)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	want := 1
	for i := 0; i < 3; i++ {
		if _, err := series.Append("edit\n", contentTS); err != nil {
			t.Fatalf("Append %d: %v", i+1, err)
		}
		want++
	}
	cur, err := series.Current()
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if cur.Version != want {
		t.Fatalf("current version = %d, want %d", cur.Version, want)
	}
	if series.Len() != want {
		t.Fatalf("series length = %d, want %d", series.Len(), want)
	}
}

// T8.4.6 — Content hash generated.
func TestT846ContentHashGenerated(t *testing.T) {
	src := "# Hashed content\n"
	sum := ContentHash(src)
	if len(sum) != 64 {
		t.Fatalf("hash length = %d, want 64 hex chars", len(sum))
	}
	if _, err := hex.DecodeString(sum); err != nil {
		t.Fatalf("hash is not hex: %v", err)
	}
	if ContentHash(src) != sum {
		t.Fatalf("hash must be deterministic for identical bytes")
	}
	if ContentHash(src+"x") == sum {
		t.Fatalf("hash must differ for differing bytes")
	}
	series, err := NewVersionSeries(uuid.New(), src, contentTS)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	v1, err := series.Current()
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if v1.ContentHash != sum {
		t.Fatalf("stored content_hash = %q, want generated %q", v1.ContentHash, sum)
	}
	if v1.ContentHash == "" {
		t.Fatalf("content_hash must never be empty")
	}
}
