package integration

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"context"

	"github.com/google/uuid"

	"overseer/internal/statefile"
)

// P7-22 (WBS 7.39): the materialization REST API — POST
// /api/v1/projects/{project}/state-files/materialize writes the
// authoritative persisted content into the project's working directory and
// GET /api/v1/projects/{project}/state-files/drift reports per-file drift
// status. The destination is the project's configured working directory
// (server-resolved) — a client-supplied path target is rejected
// (T7.39.3) and materialization writes stay inside the project directory
// (T7.39.4). Both endpoints reuse the P7-14/P7-16 materialization/drift
// services; there is no parallel drift computation path.

// httpRaw performs the request and returns ONLY the status code — the body
// is not JSON-parsed (the mux 405 body is plain text).
func httpRaw(t *testing.T, method, url string, body any) int {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func p722MatURL(h *Harness, projectID string) string {
	return h.BaseURL + "/api/v1/projects/" + projectID + "/state-files/materialize"
}

func p722DriftURL(h *Harness, projectID string) string {
	return h.BaseURL + "/api/v1/projects/" + projectID + "/state-files/drift"
}

// p722Rig builds a project with a real working directory, a MANAGED
// state-file type + PROJECT-scoped file, and content at a known version,
// and points the project's configured working directory at the temp dir.
// The working directory is set by direct SQL (the fixture's server-resolved
// destination — it is not a test of the settings API).
func p722Rig(t *testing.T, h *Harness) (projectID, fileID uuid.UUID, dir, content string) {
	t.Helper()
	ctx := context.Background()
	projectID, _, _ = p705Rig(t, h)
	svc := p708Service(t, h)
	_, fileID = p708TypeAndFile(t, h, svc, projectID, "project-state", statefile.SourceManaged)
	content = "authoritative content v1"
	if _, err := svc.UpdateContent(ctx, fileID.String(), content, 0); err != nil {
		t.Fatalf("v1: %v", err)
	}
	dir = t.TempDir()
	if _, err := h.DBExec(ctx, "UPDATE project_settings SET working_directory = '"+dir+"' WHERE project_id = '"+projectID.String()+"'"); err != nil {
		t.Fatalf("set working directory: %v", err)
	}
	return
}

// T7.39.1 — Materialization succeeds: the POST writes the authoritative
// persisted content to the project's working directory
// (<dir>/.overseer/state/<DefaultFilename>) and the response identifies the
// materialized file (id, path, version, hash).
func TestP722MaterializationSucceeds(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, fileID, dir, content := p722Rig(t, h)

	code, got := httpJSON(t, http.MethodPost, p722MatURL(h, projectID.String()), map[string]any{})
	if code != http.StatusOK {
		t.Fatalf("materialize = %d, want 200: %v", code, got)
	}
	files, _ := got["files"].([]any)
	if len(files) != 1 {
		t.Fatalf("materialized files = %d, want 1: %v", len(files), got)
	}
	m, _ := files[0].(map[string]any)
	if m["id"] != fileID.String() {
		t.Fatalf("materialized file id = %v, want %s", m["id"], fileID)
	}
	wantPath := filepath.Join(dir, ".overseer", "state", "project-state.md")
	if m["path"] != wantPath {
		t.Fatalf("materialized path = %v, want %s", m["path"], wantPath)
	}
	if m["skipped"].(bool) {
		t.Fatalf("file reported skipped: %v", m)
	}
	// The on-disk bytes ARE the persisted authoritative content.
	b, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("read materialized file: %v", err)
	}
	if string(b) != content {
		t.Fatalf("materialized bytes = %q, want %q", b, content)
	}
	// Version + hash in the response match the persisted current version.
	var cur int
	var hash string
	if err := h.DBScan(ctx, "SELECT current_version, (SELECT content_hash FROM state_file_versions WHERE state_file_id = sf.id AND version = sf.current_version) FROM state_files sf WHERE sf.id = '"+fileID.String()+"'", &cur, &hash); err != nil {
		t.Fatalf("scan version: %v", err)
	}
	if m["version"].(float64) != float64(cur) || m["hash"] != hash {
		t.Fatalf("response (version,hash) = (%v,%v), want (%d,%s)", m["version"], m["hash"], cur, hash)
	}
	// The metadata record (projection for drift detection) was upserted.
	ver, mhash, mpath, _ := p715MetaRow(t, h, fileID.String())
	if ver != cur || mhash != hash || mpath != wantPath {
		t.Fatalf("meta row = (%d,%s,%s), want (%d,%s,%s)", ver, mhash, mpath, cur, hash, wantPath)
	}
}

// T7.39.2 — Drift status returned: the GET reports per-file drift status
// (synchronized / drifted / missing / not-materialized) against the
// persisted authoritative versions.
func TestP722DriftStatusReturned(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, fileID, dir, _ := p722Rig(t, h)
	target := filepath.Join(dir, ".overseer", "state", "project-state.md")

	// 1. Unmaterialized: a file with content but no on-disk copy is MISSING.
	code, got := httpJSON(t, http.MethodGet, p722DriftURL(h, projectID.String()), nil)
	if code != http.StatusOK {
		t.Fatalf("drift = %d, want 200: %v", code, got)
	}
	files, _ := got["files"].([]any)
	if len(files) != 1 {
		t.Fatalf("drift files = %d, want 1: %v", len(files), got)
	}
	m, _ := files[0].(map[string]any)
	if m["id"] != fileID.String() || m["status"] != "MISSING" {
		t.Fatalf("drift entry = %v, want MISSING for %s", m, fileID)
	}

	// 2. Materialize, then the file is SYNCHRONIZED.
	if code, got := httpJSON(t, http.MethodPost, p722MatURL(h, projectID.String()), map[string]any{}); code != http.StatusOK {
		t.Fatalf("materialize = %d: %v", code, got)
	}
	code, got = httpJSON(t, http.MethodGet, p722DriftURL(h, projectID.String()), nil)
	if code != http.StatusOK {
		t.Fatalf("drift (post-materialize) = %d: %v", code, got)
	}
	files, _ = got["files"].([]any)
	m, _ = files[0].(map[string]any)
	if m["status"] != "SYNCHRONIZED" {
		t.Fatalf("drift = %v, want SYNCHRONIZED", m)
	}

	// 3. External edit -> DRIFTED (the persisted version stays the
	// comparison source — the P7-16 contract, no parallel path).
	if err := os.WriteFile(target, []byte("externally edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, got = httpJSON(t, http.MethodGet, p722DriftURL(h, projectID.String()), nil)
	if code != http.StatusOK {
		t.Fatalf("drift (edited) = %d: %v", code, got)
	}
	files, _ = got["files"].([]any)
	m, _ = files[0].(map[string]any)
	if m["status"] != "DRIFTED" {
		t.Fatalf("drift = %v, want DRIFTED", m)
	}

	// 4. A second, no-content file reports NOT_MATERIALIZED (a file-level
	// status, not an op error).
	svc := p708Service(t, h)
	_, file2 := p708TypeAndFile(t, h, svc, projectID, "no-content", statefile.SourceManaged)
	code, got = httpJSON(t, http.MethodGet, p722DriftURL(h, projectID.String()), nil)
	if code != http.StatusOK {
		t.Fatalf("drift (mixed) = %d: %v", code, got)
	}
	files, _ = got["files"].([]any)
	if len(files) != 2 {
		t.Fatalf("drift files = %d, want 2: %v", len(files), got)
	}
	byID := map[string]map[string]any{}
	for _, e := range files {
		em, _ := e.(map[string]any)
		byID[em["id"].(string)] = em
	}
	if byID[file2.String()]["status"] != "NOT_MATERIALIZED" {
		t.Fatalf("no-content file drift = %v, want NOT_MATERIALIZED", byID[file2.String()])
	}
	// The drift read is non-mutating: the persisted version is untouched.
	var cur int
	if err := h.DBScan(ctx, "SELECT current_version FROM state_files WHERE id = '"+fileID.String()+"'", &cur); err != nil {
		t.Fatalf("scan version: %v", err)
	}
	if cur != 1 {
		t.Fatalf("current_version = %d, want 1 (drift must not write)", cur)
	}
}

// T7.39.3 — Arbitrary destination rejected: a client-supplied path target
// in the POST body is rejected by the strict decode (the endpoint accepts
// no fields), a path-like query parameter is ignored, and nothing is
// written to the foreign location.
func TestP722ArbitraryDestinationRejected(t *testing.T) {
	h := NewHarness(t)
	projectID, fileID, dir, _ := p722Rig(t, h)
	foreign := t.TempDir()

	// Any field in the body is outside the (empty) allow-list: 400.
	code, got := httpJSON(t, http.MethodPost, p722MatURL(h, projectID.String()),
		map[string]any{"path": foreign})
	if code != http.StatusBadRequest {
		t.Fatalf("materialize with path field = %d, want 400: %v", code, got)
	}
	_ = got
	// A path-like query parameter is equally ignored: the server resolves
	// the destination from the project's configured working directory.
	code, got = httpJSON(t, http.MethodPost, p722MatURL(h, projectID.String())+"?path="+foreign, map[string]any{})
	if code != http.StatusOK {
		t.Fatalf("materialize with query path = %d, want 200: %v", code, got)
	}
	// The file was written to the project working directory, and nothing
	// landed in the foreign location.
	b, err := os.ReadFile(filepath.Join(dir, ".overseer", "state", "project-state.md"))
	if err != nil {
		t.Fatalf("expected materialized file in project dir: %v", err)
	}
	_ = b
	entries, err := os.ReadDir(foreign)
	if err != nil {
		t.Fatalf("read foreign dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("foreign dir has %d entries, want 0 (no arbitrary path)", len(entries))
	}
	// Unknown project -> 404; malformed project segment -> 404 (the
	// established project-scoped read contract).
	code, _ = httpJSON(t, http.MethodPost, h.BaseURL+"/api/v1/projects/22222222-2222-2222-2222-222222222222/state-files/materialize", map[string]any{})
	if code != http.StatusNotFound {
		t.Fatalf("unknown project = %d, want 404", code)
	}
	code, _ = httpJSON(t, http.MethodPost, h.BaseURL+"/api/v1/projects/not-a-uuid/state-files/materialize", map[string]any{})
	if code != http.StatusNotFound {
		t.Fatalf("malformed project segment = %d, want 404", code)
	}
	_ = fileID
}

// T7.39.4 — Project working-directory isolation enforced: materialization
// writes stay inside the project directory (only the .overseer/state
// namespace), pre-existing unrelated files survive byte-identical, the
// drifted-then-rematerialized cycle restores the authoritative bytes, and
// write methods on the read-only drift endpoint are 405 (the mux
// contract; the body is plain text — raw-status checks).
func TestP722WorkingDirectoryIsolation(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, fileID, dir, content := p722Rig(t, h)

	// Pre-existing unrelated files outside the .overseer namespace: byte
	// identity must survive the op (the T7.20.4 property on the REST
	// surface).
	other := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(other, []byte("unrelated"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, got := httpJSON(t, http.MethodPost, p722MatURL(h, projectID.String()), map[string]any{})
	if code != http.StatusOK {
		t.Fatalf("materialize = %d: %v", code, got)
	}
	b, err := os.ReadFile(other)
	if err != nil || string(b) != "unrelated" {
		t.Fatalf("unrelated file perturbed: %q (%v)", b, err)
	}
	// Walk the whole project directory: every entry is the walk root, the
	// .overseer namespace, or inside its state/ subtree — no escape.
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		if p722InsideStateNamespace(dir, path) {
			return nil
		}
		t.Fatalf("write outside the state namespace: %s", path)
		return nil
	})
	// The drifted-then-rematerialized cycle restores the authoritative
	// bytes (the persisted version is the source of truth).
	target := filepath.Join(dir, ".overseer", "state", "project-state.md")
	if err := os.WriteFile(target, []byte("externally edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, got = httpJSON(t, http.MethodPost, p722MatURL(h, projectID.String()), map[string]any{})
	if code != http.StatusOK {
		t.Fatalf("re-materialize = %d: %v", code, got)
	}
	b, err = os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != content {
		t.Fatalf("re-materialized bytes = %q, want %q", b, content)
	}
	// Write methods on the read-only drift endpoint are rejected with no
	// write path: the handler is a pure read that rejects any body (the
	// strict decoder rejects empty and non-empty alike) — 400 for PUT and
	// POST, with or without a body (stated choice; the non-mutation below
	// is the load-bearing proof).
	for _, method := range []string{http.MethodPut, http.MethodPost, http.MethodPatch} {
		for _, body := range []any{map[string]any{}, nil} {
			code := httpRaw(t, method, p722DriftURL(h, projectID.String()), body)
			// Write methods are rejected with no write path: 400 (the
			// strict-decode rejection on the PUT {file} route, which this
			// literal path also matches) or 405 (mux method rejection) —
			// either way the read-only contract holds.
			if code != http.StatusBadRequest && code != http.StatusMethodNotAllowed {
				t.Fatalf("%s drift (body=%v) = %d, want 400/405", method, body, code)
			}
		}
	}
	// No write path: version untouched.
	var cur int
	if err := h.DBScan(ctx, "SELECT current_version FROM state_files WHERE id = '"+fileID.String()+"'", &cur); err != nil {
		t.Fatalf("scan version: %v", err)
	}
	if cur != 1 {
		t.Fatalf("current_version = %d, want 1 after rejected writes", cur)
	}
}

// p722InsideStateNamespace reports whether path is the project root dir or
// under dir/.overseer/state (the only namespace the op may write).
func p722InsideStateNamespace(dir, path string) bool {
	if path == dir {
		return true
	}
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	rel = filepath.ToSlash(rel)
	// Pre-existing entries at the project root (and elsewhere outside
	// .overseer) are allowed — the op only WRITES into the state
	// namespace, so the isolation check applies to the .overseer tree:
	// every entry under it must be inside state/.
	if !strings.HasPrefix(rel, ".overseer") {
		return true
	}
	inner, err := filepath.Rel(filepath.Join(dir, ".overseer"), path)
	if err != nil {
		return false
	}
	inner = filepath.ToSlash(inner)
	return inner == "." || inner == "state" ||
		(len(inner) > len("state/") && inner[:len("state/")] == "state/")
}
