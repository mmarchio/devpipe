package cache

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/project"
	"overseer/internal/prompt"
	"overseer/internal/role"
)

func validInput() PromptCacheInput {
	c := "cont-hash"
	return PromptCacheInput{
		ProjectID: uuid.MustParse("11111111-1111-1111-1111-111111111111"), ProjectConfigHash: "pc",
		ContextSnapshotHash: "snap", StateMachineID: uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		StateMachineVersion: 1, StateKey: "IMPLEMENT", RoleHash: "role", TemplateHash: "tmpl",
		ResponseProtocolHash: "proto", CompressionConfigHash: "comp", ContinuationStatusHash: &c,
		WorkUnitSpecHash: "unit", StateInstructionHash: "si",
	}
}

func mustKey(t *testing.T, in PromptCacheInput) string {
	t.Helper()
	k, err := CacheKey(in, CurrentSchema)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// --- 10.2 input ---

func TestT1021To1024Input(t *testing.T) {
	if _, err := NewPromptCacheInput(validInput()); err != nil {
		t.Fatalf("T10.2.1: %v", err)
	}
	bad := validInput()
	bad.ContextSnapshotHash, bad.RoleHash, bad.ProjectID = "", "", uuid.Nil
	var ie *InputError
	if _, err := NewPromptCacheInput(bad); !errors.As(err, &ie) || len(ie.Fields) != 3 {
		t.Fatalf("T10.2.2: %v", err)
	}
	none := validInput()
	none.ContinuationStatusHash = nil
	if _, err := NewPromptCacheInput(none); err != nil {
		t.Fatal("T10.2.3 continuation optional")
	}
	empty := ""
	none.ContinuationStatusHash = &empty
	if _, err := NewPromptCacheInput(none); err == nil {
		t.Fatal("empty continuation hash accepted")
	}
	typ := reflect.TypeOf(PromptCacheInput{})
	for i := 0; i < typ.NumField(); i++ {
		n := strings.ToLower(typ.Field(i).Name)
		for _, w := range []string{"name", "description", "createdat", "updatedat", "title"} {
			if strings.Contains(n, w) {
				t.Fatalf("T10.2.4 display field %s in cache input", typ.Field(i).Name)
			}
		}
	}
}

// --- 10.3 / 10.4 key ---

func TestT103KeyGeneration(t *testing.T) {
	in := validInput()
	k := mustKey(t, in)
	if k != mustKey(t, in) || !strings.HasPrefix(k, "prompt-cache:v2:") || len(k) != len("prompt-cache:v2:")+64 {
		t.Fatalf("T10.3.1 %s", k)
	}
	// T10.3.2: the key is built field-by-field in a fixed order; a copy
	// constructed in a different literal order is the same value.
	in2 := PromptCacheInput{CompressionConfigHash: "comp", ResponseProtocolHash: "proto", TemplateHash: "tmpl", RoleHash: "role",
		StateKey: "IMPLEMENT", StateMachineVersion: 1, StateMachineID: in.StateMachineID, ContextSnapshotHash: "snap",
		ProjectConfigHash: "pc", ProjectID: in.ProjectID, ContinuationStatusHash: in.ContinuationStatusHash,
		StateInstructionHash: "si", WorkUnitSpecHash: "unit"}
	if mustKey(t, in2) != k {
		t.Fatal("T10.3.2")
	}
	for name, mut := range map[string]func(*PromptCacheInput){
		"T10.3.3 snapshot":    func(p *PromptCacheInput) { p.ContextSnapshotHash = "snap2" },
		"T10.3.4 template":    func(p *PromptCacheInput) { p.TemplateHash = "tmpl2" },
		"T10.3.5 role":        func(p *PromptCacheInput) { p.RoleHash = "role2" },
		"T10.3.6 compression": func(p *PromptCacheInput) { p.CompressionConfigHash = "comp2" },
		"machine version":     func(p *PromptCacheInput) { p.StateMachineVersion = 2 },
		"protocol":            func(p *PromptCacheInput) { p.ResponseProtocolHash = "p2" },
		"unit spec":           func(p *PromptCacheInput) { p.WorkUnitSpecHash = "u2" },
		"project":             func(p *PromptCacheInput) { p.ProjectID = uuid.New() },
		"continuation":        func(p *PromptCacheInput) { p.ContinuationStatusHash = nil },
		"policy":              func(p *PromptCacheInput) { p.PreparationPolicy = "SAME_SESSION" },
	} {
		c := validInput()
		mut(&c)
		if mustKey(t, c) == k {
			t.Fatalf("%s did not change the key", name)
		}
	}
}

func TestT104SchemaVersion(t *testing.T) {
	in := validInput()
	v1, _ := CacheKey(in, 1)
	v2, _ := CacheKey(in, 2)
	if v1 == v2 || !strings.HasPrefix(v2, "prompt-cache:v2:") {
		t.Fatal("T10.4.1/T10.4.2")
	}
	if _, err := CacheKey(in, 99); !errors.Is(err, ErrUnknownSchema) {
		t.Fatal("T10.4.3")
	}
	if v, err := SchemaOf(v1); err != nil || v != 1 {
		t.Fatal("T10.4.4 historical key auditable")
	}
}

func TestLayeredKeys(t *testing.T) {
	in := validInput()
	c1, _ := CanonicalKey(in, CurrentSchema)
	comp := in
	comp.CompressionConfigHash, comp.ProjectConfigHash = "other", "other"
	c2, _ := CanonicalKey(comp, CurrentSchema)
	if c1 != c2 || mustKey(t, comp) == mustKey(t, in) || !strings.HasPrefix(c1, "canonical-cache:v2:") {
		t.Fatal("compression/project-config change must keep the canonical key and change the worker key")
	}
	tm := in
	tm.TemplateHash = "t3"
	if c3, _ := CanonicalKey(tm, CurrentSchema); c3 == c1 {
		t.Fatal("template change must change the canonical key")
	}
}

// --- 10.5 / 10.6 / 10.12 / 10.21 hashes ---

func TestT105ProjectConfigHash(t *testing.T) {
	s := project.Settings{ProjectID: uuid.New(), WorkerProvider: "hermes", WorkerModel: "m", ContextWindow: 64000, ContextThreshold: 0.8,
		WorkingDirectory: "/a", MaxRetries: 1, PlanningPolicy: "p"}
	h := ProjectConfigHash(s)
	if h != ProjectConfigHash(s) {
		t.Fatal("T10.5.3")
	}
	r := s
	r.ContextWindow = 32000
	if ProjectConfigHash(r) == h {
		t.Fatal("T10.5.1")
	}
	ui := s
	ui.WorkingDirectory, ui.MaxRetries, ui.AutoContinue, ui.UpdatedAt = "/b", 9, true, time.Now()
	ui.PlanningPolicy = "other" // planning only affects planning prompts (planning hash)
	if ProjectConfigHash(ui) != h {
		t.Fatal("T10.5.2 irrelevant change moved the hash")
	}
	// T10.5.4: the hash is computed from explicit fields only (no secret
	// field exists on project settings); a planted secret-like value in an
	// excluded field cannot reach it.
	sec := s
	sec.WorkingDirectory = "sk-secret"
	if ProjectConfigHash(sec) != h {
		t.Fatal("T10.5.4")
	}
	if PlanningConfigHash("ATOMIC", "atomic") == PlanningConfigHash("COMPONENT", "atomic") {
		t.Fatal("T10.24.1 planning policy")
	}
}

func TestT106RoleHash(t *testing.T) {
	r := role.RoleSnapshot{RoleKey: "ENGINEER", RoleVersion: 1,
		SystemPrompt: role.PromptSnapshot{Version: 1, ContentHash: "s1"},
		Instructions: []role.InstructionSnapshot{{Key: "a", Version: 1, ContentHash: "a1"}, {Key: "b", Version: 1, ContentHash: "b1"}},
		StateFiles:   []string{"ARCHITECTURE"}}
	h := RoleHash(r)
	if RoleHash(r) != h {
		t.Fatal("T10.6.1")
	}
	sp := r
	sp.SystemPrompt.ContentHash = "s2"
	if RoleHash(sp) == h {
		t.Fatal("T10.6.2")
	}
	ins := r
	ins.Instructions = []role.InstructionSnapshot{{Key: "a", Version: 2, ContentHash: "a2"}, r.Instructions[1]}
	if RoleHash(ins) == h {
		t.Fatal("T10.6.3")
	}
	re := r
	re.Instructions = []role.InstructionSnapshot{r.Instructions[1], r.Instructions[0]}
	if RoleHash(re) == h {
		t.Fatal("T10.6.4 reorder")
	}
	sf := r
	sf.StateFiles = []string{"ARCHITECTURE", "ACTIVE_TASK"}
	if RoleHash(sf) == h {
		t.Fatal("T10.22.3 state-file requirement")
	}
	// T10.6.5: RoleSnapshot carries no display name — nothing to change.
	if _, ok := reflect.TypeOf(role.RoleSnapshot{}).FieldByName("Name"); ok {
		t.Fatal("display name became a role-hash input")
	}
}

func TestT1012ContinuationHash(t *testing.T) {
	u := uuid.New()
	a := prompt.ContinuationStatusInput{WorkUnitID: u, Completed: []string{"repository"}, Remaining: []string{"tests"}}
	h := ContinuationStatusHash(a)
	if ContinuationStatusHash(a) != h {
		t.Fatal("T10.12.1")
	}
	b := a
	b.Remaining = []string{"integration"}
	if ContinuationStatusHash(b) == h {
		t.Fatal("T10.12.2")
	}
	c := a
	c.Blocking = []string{"schema review"}
	if ContinuationStatusHash(c) == h {
		t.Fatal("T10.12.3")
	}
	d := a
	d.WorkUnitID = uuid.New()
	if ContinuationStatusHash(d) == h {
		t.Fatal("T10.12.4")
	}
	// moving an item between lists changes the hash
	e := a
	e.Completed, e.Remaining = []string{}, []string{"repository", "tests"}
	if ContinuationStatusHash(e) == h {
		t.Fatal("list boundary ignored")
	}
}

func TestWorkUnitSpecAndVersionedHash(t *testing.T) {
	u := prompt.WorkUnitContext{ID: uuid.New(), Name: "10.17", Objective: "o", Acceptance: []string{"a"}, Dependencies: []string{"x", "y"}}
	h := WorkUnitSpecHash(u)
	v := u
	v.Acceptance = []string{"a", "b"}
	if WorkUnitSpecHash(v) == h {
		t.Fatal("T10.21.3")
	}
	w := u
	w.Dependencies = []string{"y", "x"}
	if WorkUnitSpecHash(w) != h {
		t.Fatal("dependency order must not matter (rendered sorted)")
	}
	w.Dependencies = []string{"x"}
	if WorkUnitSpecHash(w) == h {
		t.Fatal("T10.21.2")
	}
	id := uuid.New()
	if VersionedHash("template", id, 1, "same") == VersionedHash("template", id, 2, "same") {
		t.Fatal("T10.8.2 a new version with identical text must miss")
	}
}

// --- 10.44 / 10.28 / 10.40 metadata ---

func TestT1044Classification(t *testing.T) {
	for _, s := range []string{InputRole, InputStateInstruction, InputTemplate, InputResponseProtocol, InputPlanningConfig, "file:ARCHITECTURE"} {
		if Classify(s) != Static {
			t.Fatalf("T10.44.1 %s", s)
		}
	}
	for _, d := range []string{InputContinuation, "file:ACTIVE_TASK", "worker_status", "test_output", "generated_plan", "something_new"} {
		if Classify(d) != Dynamic {
			t.Fatalf("T10.44.2/T10.44.3 %s", d)
		}
	}
	if Classify(InputRole) != Classify(InputRole) {
		t.Fatal("T10.44.4")
	}
}

func TestT1028AndT1040MetadataDiff(t *testing.T) {
	files := []FileVersion{{TypeKey: "ACTIVE_TASK", Version: 4, Hash: "a4"}, {TypeKey: "ARCHITECTURE", Version: 7, Hash: "r7"}}
	a := BuildMetadata(validInput(), Details{Template: "IMPLEMENT_WORK_UNIT v6", Files: files})
	if d := Diff(a, a); !d.Equal() || len(d.Unchanged) == 0 {
		t.Fatal("T10.28.1/T10.40.1 identical metadata")
	}
	in := validInput()
	in.ContextSnapshotHash = "snap-v5"
	files2 := []FileVersion{{TypeKey: "ACTIVE_TASK", Version: 5, Hash: "a5"}, files[1]}
	b := BuildMetadata(in, Details{Template: "IMPLEMENT_WORK_UNIT v6", Files: files2})
	d := Diff(a, b)
	changed := map[string]Change{}
	for _, c := range d.Changed {
		changed[c.Name] = c
	}
	if c, ok := changed["file:ACTIVE_TASK"]; !ok || c.Before != "ACTIVE_TASK v4" || c.After != "ACTIVE_TASK v5" {
		t.Fatalf("T10.40.2 %+v", d.Changed)
	}
	if _, ok := changed[InputContextSnapshot]; !ok || len(d.Changed) != 2 {
		t.Fatalf("changed set %+v", d.Changed)
	}
	un := strings.Join(d.Unchanged, ",")
	for _, n := range []string{InputRole, InputTemplate, InputResponseProtocol, InputCompression, "file:ARCHITECTURE"} {
		if !strings.Contains(un, n) {
			t.Fatalf("%s not reported unchanged", n)
		}
	}
	if a.Classes()["file:ACTIVE_TASK"] != Dynamic || a.Classes()[InputRole] != Static {
		t.Fatal("classes stored with metadata")
	}
}

// --- 10.1 / 10.31 / 10.32 / 10.33 / 10.34 entries ---

func TestT101Entry(t *testing.T) {
	now := time.Now().UTC()
	k := mustKey(t, validInput())
	in := EntryInput{ProjectID: uuid.New(), CacheKey: k, CanonicalPromptID: uuid.New(), WorkerPromptID: uuid.New(), Now: now}
	e, err := NewValidEntry(in)
	if err != nil || e.Status != StatusValid || e.Source != SourceOnDemand || !e.CreatedAt.Equal(now) || e.Layer != LayerWorker {
		t.Fatalf("T10.1.1/T10.1.4/T10.1.5 %+v %v", e, err)
	}
	for name, mut := range map[string]func(*EntryInput){
		"T10.1.2 key":     func(p *EntryInput) { p.CacheKey = "" },
		"T10.1.3 worker":  func(p *EntryInput) { p.WorkerPromptID = uuid.Nil },
		"T10.1.6 project": func(p *EntryInput) { p.ProjectID = uuid.Nil },
		"bad source":      func(p *EntryInput) { p.Source = "LATER" },
	} {
		c := in
		mut(&c)
		if _, err := NewValidEntry(c); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	pre := in
	pre.Source = SourcePrecomputed
	if p, err := NewValidEntry(pre); err != nil || p.Source != SourcePrecomputed || p.CacheKey != e.CacheKey {
		t.Fatal("T10.43.2/T10.43.3 source is metadata only")
	}
}

func TestT1031AndT1032BuildLease(t *testing.T) {
	now := time.Now().UTC()
	b, err := NewBuildingEntry(uuid.New(), "prompt-cache:v1:x", LayerWorker, "A", time.Minute, now)
	if err != nil || b.Status != StatusBuilding {
		t.Fatal("T10.31.1")
	}
	if err := b.Claim("B", time.Minute, now.Add(time.Second)); !errors.Is(err, ErrLeaseHeld) {
		t.Fatal("T10.32.1")
	}
	if err := b.CheckOwner("B", now); !errors.Is(err, ErrNotLeaseOwner) {
		t.Fatal("T10.32.3")
	}
	later := now.Add(2 * time.Minute)
	if !b.Stale(later) {
		t.Fatal("T10.31.4")
	}
	if err := b.Claim("B", time.Minute, later); err != nil || b.BuildOwner != "B" {
		t.Fatal("T10.32.2 expired lease reclaim")
	}
	if err := b.CheckOwner("A", later); !errors.Is(err, ErrNotLeaseOwner) {
		t.Fatal("old owner finalized after reclaim")
	}
	if err := b.Transition(StatusValid); err != nil || !CanTransition(StatusBuilding, StatusFailed) {
		t.Fatal("T10.31.2/T10.31.3")
	}
	if err := b.Transition(StatusBuilding); !errors.Is(err, ErrIllegalTransition) {
		t.Fatal("VALID → BUILDING allowed")
	}
	if CanTransition(StatusFailed, StatusValid) || CanTransition(StatusInvalidated, StatusValid) {
		t.Fatal("terminal history revived")
	}
}

func TestT1033AndT1034FailureEntries(t *testing.T) {
	now := time.Now().UTC()
	until := now.Add(30 * time.Second)
	f := PromptCacheEntry{Status: StatusFailed, RetryAfter: &until}
	if f.Dispatchable() {
		t.Fatal("T10.33.1")
	}
	if !f.Suppresses(now.Add(10*time.Second)) || f.Suppresses(now.Add(31*time.Second)) {
		t.Fatal("T10.34.1/T10.34.2")
	}
	perm := PromptCacheEntry{Status: StatusFailed}
	if perm.Suppresses(now) {
		t.Fatal("permanent failure without window suppresses")
	}
	if !TransientErrorKinds["RATE_LIMIT"] || TransientErrorKinds["AUTH"] {
		t.Fatal("transient kinds")
	}
}

// --- 10.35 retention ---

func TestT1035Retention(t *testing.T) {
	now := time.Now().UTC()
	unit := uuid.New()
	mk := func(age time.Duration, st Status) PromptCacheEntry {
		return PromptCacheEntry{ID: uuid.New(), Layer: LayerWorker, StateKey: "IMPLEMENT", WorkUnitID: &unit, Status: st,
			CreatedAt: now.Add(-age), LastUsedAt: now.Add(-age)}
	}
	newest := mk(time.Hour, StatusValid)
	old := mk(40*24*time.Hour, StatusInvalidated)
	oldRef := mk(50*24*time.Hour, StatusInvalidated)
	recent := mk(2*24*time.Hour, StatusInvalidated)
	building := mk(60*24*time.Hour, StatusBuilding)
	all := []PromptCacheEntry{newest, old, oldRef, recent, building}
	ref := func(id uuid.UUID) bool { return id == oldRef.ID }
	if (Retention{Policy: RetainAll}).Eligible(all, now, ref) != nil {
		t.Fatal("retain all")
	}
	got := (Retention{Policy: RetainDays, Value: 30}).Eligible(all, now, ref)
	if len(got) != 1 || got[0] != old.ID {
		t.Fatalf("T10.35.1-T10.35.3 %v", got)
	}
	got = (Retention{Policy: RetainLatest, Value: 2}).Eligible(all, now, ref)
	if len(got) != 1 || got[0] != old.ID {
		t.Fatalf("T10.35.4 latest-N %v", got)
	}
	if (Retention{Policy: RetainDays}).Validate() == nil || (Retention{Policy: "FOREVER"}).Validate() == nil {
		t.Fatal("invalid retention accepted")
	}
}

// --- 10.45-10.47 fragments ---

func TestT1045To1047Fragments(t *testing.T) {
	pid := uuid.New()
	roleID := uuid.New()
	mk := func(typ prompt.PromptComponentType, order int, content string, src uuid.UUID, ver int) prompt.PromptComponent {
		c, _ := prompt.NewPromptComponent(typ, prompt.SourceSystemDefault, order, content, src, ver, "h-"+content)
		return c
	}
	comps := prompt.Components{
		mk(prompt.ComponentSystem, 0, "system", roleID, 1),
		mk(prompt.ComponentRoleInstruction, 1, "role rules", uuid.New(), 2),
		mk(prompt.ComponentProjectContext, 2, "ARCHITECTURE v7", uuid.New(), 7),
		mk(prompt.ComponentTaskContext, 3, "ACTIVE_TASK v2", uuid.New(), 2),
		mk(prompt.ComponentStateInstruction, 4, "implement", uuid.New(), 3),
	}
	frags := BuildFragments(pid, comps, time.Now())
	kinds := map[FragmentKind]PromptFragment{}
	for _, f := range frags {
		kinds[f.Kind] = f
		if ValidateFragment(f) != nil || len(f.Sources) == 0 {
			t.Fatalf("T10.45.1/T10.46.2 %+v", f)
		}
	}
	if len(kinds) != 3 || strings.Contains(kinds[FragmentSystemRolePrefix].Content, "ACTIVE_TASK") {
		t.Fatal("T10.45.3 fragments")
	}
	// ACTIVE_TASK change does not move the role fragment key; role change does.
	comps2 := append(prompt.Components{}, comps...)
	comps2[3] = mk(prompt.ComponentTaskContext, 3, "ACTIVE_TASK v3", comps[3].SourceID, 3)
	f2 := map[FragmentKind]PromptFragment{}
	for _, f := range BuildFragments(pid, comps2, time.Now()) {
		f2[f.Kind] = f
	}
	if f2[FragmentSystemRolePrefix].FragmentKey != kinds[FragmentSystemRolePrefix].FragmentKey {
		t.Fatal("T10.47.1")
	}
	comps3 := append(prompt.Components{}, comps...)
	comps3[1] = mk(prompt.ComponentRoleInstruction, 1, "role rules v3", comps[1].SourceID, 3)
	comps3[4] = mk(prompt.ComponentStateInstruction, 4, "implement v4", comps[4].SourceID, 4)
	f3 := map[FragmentKind]PromptFragment{}
	for _, f := range BuildFragments(pid, comps3, time.Now()) {
		f3[f.Kind] = f
	}
	if f3[FragmentSystemRolePrefix].FragmentKey == kinds[FragmentSystemRolePrefix].FragmentKey {
		t.Fatal("T10.47.2")
	}
	if f3[FragmentStatePrefix].FragmentKey == kinds[FragmentStatePrefix].FragmentKey {
		t.Fatal("T10.47.3")
	}
	if FragmentKey(uuid.New(), FragmentSystemRolePrefix, kinds[FragmentSystemRolePrefix].Sources) == kinds[FragmentSystemRolePrefix].FragmentKey {
		t.Fatal("fragment key not project-scoped")
	}
	bad := kinds[FragmentStatePrefix]
	bad.Content += "x"
	if ValidateFragment(bad) == nil {
		t.Fatal("T10.45.2 tampered content accepted")
	}
}

// --- 10.37 stats ---

func TestT1037Stats(t *testing.T) {
	if s := Aggregate(nil, nil); s.HitRate != 0 || s.Lookups != 0 {
		t.Fatal("T10.64.4 empty")
	}
	pid := uuid.New()
	now := time.Now()
	ev := func(typ EventType, l Layer) Event { return NewEvent(pid, typ, l, "k", now) }
	hit := ev(EventHit, LayerWorker)
	hit.AvoidedCompressionCalls, hit.AvoidedTokens, hit.AvoidedLatency = 1, 18000, time.Second
	events := []Event{ev(EventLookup, LayerWorker), ev(EventMiss, LayerWorker), ev(EventLookup, LayerWorker), hit,
		ev(EventLookup, LayerWorker), ev(EventMiss, LayerWorker), ev(EventHit, LayerCanonical), ev(EventHit, LayerCompression), ev(EventLookup, LayerWorker), hit}
	entries := []PromptCacheEntry{{Status: StatusValid}, {Status: StatusInvalidated}, {Status: StatusFailed}}
	s := Aggregate(entries, events)
	if s.Lookups != 4 || s.Hits != 2 || s.Misses != 2 || s.HitRate != 0.5 {
		t.Fatalf("T10.37.1-T10.37.4 %+v", s)
	}
	if s.CompressionCallsAvoided != 2 || s.EstimatedTokensAvoided != 36000 || s.CanonicalHits != 1 || s.CompressionHits != 1 || s.WorkerHits != 2 {
		t.Fatalf("T10.37.5 %+v", s)
	}
	if s.Valid != 1 || s.Invalidated != 1 || s.Failed != 1 || s.Entries != 3 {
		t.Fatal("entry counts")
	}
}
