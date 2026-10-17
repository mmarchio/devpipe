package access

import "testing"

// WBS 17.127 — principal types and validation.
func TestP1702PrincipalTypes(t *testing.T) {
	for _, s := range []string{"LOCAL_ADMIN", "SYSTEM", "USER"} {
		if _, err := ParsePrincipalType(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if _, err := ParsePrincipalType("ROOT"); err == nil {
		t.Fatal("unknown type accepted")
	}
	if err := LocalAdmin().Validate(); err != nil {
		t.Fatal(err)
	}
	if err := System().Validate(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []Principal{{}, {ID: "x", Name: "x"}, {ID: "x", Type: PrincipalUser}, {Name: "x", Type: PrincipalUser}, {ID: "x", Name: "x", Type: "ROOT"}} {
		if p.Validate() == nil {
			t.Fatalf("invalid principal accepted: %+v", p)
		}
	}
	if !(Principal{}).IsZero() || LocalAdmin().IsZero() {
		t.Fatal("IsZero")
	}
}

// WBS 17.128 — v1 grants local administrators full access; users read
// only; zero principals nothing.
func TestP1703LocalPolicy(t *testing.T) {
	var pol Authorizer = LocalPolicy{}
	res := Resource{Kind: "project", ProjectID: "p", ID: "p"}
	user := Principal{ID: "u", Name: "U", Type: PrincipalUser}
	for _, a := range Actions() {
		if !pol.Can(LocalAdmin(), a, res) || !pol.Can(System(), a, res) {
			t.Fatalf("admin / system denied %s", a)
		}
		if pol.Can(Principal{}, a, res) {
			t.Fatalf("zero principal allowed %s", a)
		}
		if got := pol.Can(user, a, res); got == a.IsMutation() {
			t.Fatalf("user %s: got %v", a, got)
		}
	}
	if pol.Can(LocalAdmin(), Action("DROP_DB"), res) {
		t.Fatal("unknown action allowed")
	}
	if ActionRead.IsMutation() || ActionPreview.IsMutation() || !ActionLifecyclePause.IsMutation() {
		t.Fatal("IsMutation")
	}
}
