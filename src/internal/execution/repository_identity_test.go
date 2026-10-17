package execution

import (
	"overseer/internal/wbs"
	"strings"
	"testing"
)

// T18.49 — identity comparison: unrelated repository (root commit), moved
// path, no longer git, branch switch, match.
func TestT1849IdentityMismatch(t *testing.T) {
	base := RepositoryIdentity{Toplevel: "/w/a", RootCommit: "aaaa", Branch: "main", IsGit: true}
	cases := map[string]struct {
		got              RepositoryIdentity
		identity, branch string
	}{
		"same":          {base, "", ""},
		"unrelated":     {RepositoryIdentity{Toplevel: "/w/a", RootCommit: "bbbb", Branch: "main", IsGit: true}, "unrelated repository", ""},
		"moved":         {RepositoryIdentity{Toplevel: "/w/b", RootCommit: "aaaa", Branch: "main", IsGit: true}, "moved", ""},
		"not git":       {RepositoryIdentity{Toplevel: "/w/a"}, "no longer a git repository", ""},
		"branch switch": {RepositoryIdentity{Toplevel: "/w/a", RootCommit: "aaaa", Branch: "dev", IsGit: true}, "", "branch switched"},
	}
	for name, c := range cases {
		id, br := IdentityMismatch(base, c.got)
		if (c.identity == "") != (id == "") || !strings.Contains(id, c.identity) || (c.branch == "") != (br == "") || !strings.Contains(br, c.branch) {
			t.Errorf("%s: identity %q branch %q", name, id, br)
		}
	}
	// A non-git working directory only checks its path.
	plain := RepositoryIdentity{Toplevel: "/w/a"}
	if id, br := IdentityMismatch(plain, RepositoryIdentity{Toplevel: "/w/a"}); id != "" || br != "" {
		t.Fatal("plain directory mismatch")
	}
}

// T18.202 — a required command without a (passing) result is reported.
func TestT18202MissingResults(t *testing.T) {
	cmds := []wbs.ValidationSpec{{Program: "go", Args: []string{"test", "./..."}}, {Program: "go", Args: []string{"vet", "./..."}}}
	recs := []ValidationRecord{{Program: "go", Args: []string{"test", "./..."}, Outcome: ValidationPass}}
	if m := MissingResults(cmds, recs, ""); len(m) != 1 || m[0] != "go vet ./..." {
		t.Fatalf("missing any: %v", m)
	}
	recs = append(recs, ValidationRecord{Program: "go", Args: []string{"vet", "./..."}, Outcome: ValidationFail})
	if m := MissingResults(cmds, recs, ""); len(m) != 0 {
		t.Fatalf("all present: %v", m)
	}
	if m := MissingResults(cmds, recs, ValidationPass); len(m) != 1 {
		t.Fatalf("missing pass: %v", m)
	}
}
