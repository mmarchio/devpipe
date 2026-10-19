package archtest

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// nonTestGoFiles lists the module's non-test Go files under internal/ and
// cmd/ (module-relative paths).
func nonTestGoFiles(t *testing.T) (string, []string) {
	t.Helper()
	root, err := moduleRoot()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, top := range []string{"internal", "cmd"} {
		_ = filepath.WalkDir(filepath.Join(root, top), func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
				rel, _ := filepath.Rel(root, p)
				out = append(out, filepath.ToSlash(rel))
			}
			return nil
		})
	}
	sort.Strings(out)
	return root, out
}

// T18.87 — no non-test code runs a shell: every process is started as
// executable + argument array, so an argument with shell metacharacters
// stays one argument.
func TestT1887NoShellInvocation(t *testing.T) {
	root, files := nonTestGoFiles(t)
	shell := regexp.MustCompile(`"(?:/bin/|/usr/bin/)?(?:sh|bash|zsh|dash|ksh)"\s*,\s*"-c"|exec\.Command(?:Context)?\([^)]*"(?:/bin/|/usr/bin/)?(?:sh|bash|zsh|dash|ksh)"`)
	// The guard itself must recognise the forms it forbids.
	for _, sample := range []string{`exec.Command("sh", "-c", cmd)`, `exec.CommandContext(ctx, "/bin/bash", "-c", s)`, `[]string{"bash", "-c", x}`} {
		if !shell.MatchString(sample) {
			t.Fatalf("guard misses %s", sample)
		}
	}
	for _, f := range files {
		src, _ := os.ReadFile(filepath.Join(root, f))
		if loc := shell.FindIndex(src); loc != nil {
			t.Errorf("%s: shell invocation %q", f, src[loc[0]:loc[1]])
		}
	}
}

// fsWriteSites is the reviewed inventory of non-test files that write to
// the filesystem (WBS 18.85 / 18.86). Each entry states how its paths are
// confined; a new write site fails this test until it is reviewed and
// added here.
var fsWriteSites = map[string]string{
	"internal/backup/backup.go":                  "writes only inside the operator-supplied backup directory (O_EXCL files, manifest)",
	"internal/gorefactor/edit.go":                "writeIfChanged confines every write to the module root (statefile.SafeRelPath, symlinks resolved)",
	"internal/gorefactor/ops.go":                 "MkdirAll / Remove confined to the module root via confined()",
	"internal/repoinspect/git.go":                "write probe: temp file in the guarded working directory / its git dir, removed at once",
	"internal/service/state_file_materialize.go": "atomic writes to statefile.SafeRelPath targets under <root>/.overseer/state",
	"internal/service/web_drift.go":              "drift restore writes resolveMaterializedPath (statefile.SafeRelPath) targets",
	"internal/service/engine_statefiles.go":      "WBS files written to statefile.SafeRelPath targets under <working directory>/statefiles/wbs (directory confined before MkdirAll)",
	"internal/config/yamledit.go":                "the operator's configuration file (OVERSEER_CONFIG_PATH): atomic temp file + rename in its own directory, previous content kept as <path>.bak",
	"internal/service/system_config.go":          "restores the operator's configuration file (OVERSEER_CONFIG_PATH) when an edit does not load",
	"internal/service/engine_plan.go":            "statefiles/PLAN_REJECTED.md written (writeStatefile) and removed at statefile.SafeRelPath targets under the working directory",
}

// T18.85 / T18.86 — inventory of every filesystem write operation in
// non-test code.
func TestT1885FilesystemWriteInventory(t *testing.T) {
	root, files := nonTestGoFiles(t)
	write := regexp.MustCompile(`\bos\.(WriteFile|Create|CreateTemp|OpenFile|Mkdir|MkdirAll|MkdirTemp|Remove|RemoveAll|Rename|Symlink|Link|Chmod|Chown|Truncate)\(`)
	seen := map[string]bool{}
	for _, f := range files {
		src, _ := os.ReadFile(filepath.Join(root, f))
		if write.Match(src) {
			seen[f] = true
			if _, ok := fsWriteSites[f]; !ok {
				t.Errorf("%s writes to the filesystem but is not in the reviewed inventory (confine its paths, then add it)", f)
			}
		}
	}
	for f := range fsWriteSites {
		if !seen[f] {
			t.Errorf("inventory entry %s no longer writes to the filesystem: remove it", f)
		}
	}
}
