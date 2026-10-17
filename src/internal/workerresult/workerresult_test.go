package workerresult_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/worker"
	wr "overseer/internal/workerresult"
)

func dispatch() worker.WorkerDispatch {
	ph, wu := uuid.New(), uuid.New()
	return worker.WorkerDispatch{ID: uuid.New(), ProjectID: uuid.New(), SessionID: uuid.New(), PhaseID: &ph, WorkUnitID: &wu,
		RequestID: uuid.New(), StateKey: "IMPLEMENT", RoleKey: "ENGINEER", Status: worker.DispatchCompleted}
}

func resp(d worker.WorkerDispatch, content string) worker.WorkerResponseRecord {
	return worker.WorkerResponseRecord{ID: uuid.New(), DispatchID: d.ID, ProjectID: d.ProjectID, Content: content, ContentHash: worker.ContentHash(content)}
}

func settings(mode wr.ParseMode, repair wr.RepairMode) wr.Settings {
	s := wr.DefaultSettings()
	s.ParseMode, s.RepairMode = mode, repair
	return s
}

func parse(t *testing.T, s wr.Settings, content string) (wr.WorkerResult, *wr.ParseError) {
	t.Helper()
	d := dispatch()
	r, err := wr.DeterministicParser{Settings: s, AcceptanceIDs: []string{"AC-1", "AC-2"}}.Parse(context.Background(), resp(d, content), d)
	if err == nil {
		if r.Metadata.DispatchID != d.ID || r.Metadata.ProjectID != d.ProjectID || *r.Metadata.WorkUnitID != *d.WorkUnitID {
			t.Fatal("metadata not from dispatch")
		}
		return r, nil
	}
	pe, ok := wr.AsParseError(err)
	if !ok {
		t.Fatalf("not a ParseError: %v", err)
	}
	return r, pe
}

func fixture(t *testing.T, name string) string {
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const valid = `{"protocol_version":"1.0","status":"complete","summary":"S","completed":["a"],"remaining":[],"blocking":[],"next_action":""}`

func with(field, raw string) string {
	m := map[string]string{"protocol_version": `"1.0"`, "status": `"incomplete"`, "summary": `"S"`, "completed": `["a"]`,
		"remaining": `["b"]`, "blocking": `[]`, "next_action": `"n"`}
	if raw == "-" {
		delete(m, field)
	} else {
		m[field] = raw
	}
	var parts []string
	for _, k := range []string{"protocol_version", "status", "summary", "completed", "remaining", "blocking", "next_action"} {
		if v, ok := m[k]; ok {
			parts = append(parts, `"`+k+`":`+v)
		}
	}
	for k, v := range m {
		if !strings.Contains(strings.Join(parts, ","), `"`+k+`"`) {
			parts = append(parts, `"`+k+`":`+v)
		}
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// --- P13-01 / P13-02 ---------------------------------------------------------

func TestP1301Status(t *testing.T) {
	for _, s := range []string{"complete", "incomplete", "blocked", "failed"} {
		st, err := wr.ParseStatus(s)
		if err != nil || string(st) != s {
			t.Fatalf("T13.1.1-4 %s", s)
		}
		r := wr.WorkerResult{Status: st}
		n := 0
		for _, b := range []bool{r.IsComplete(), r.IsIncomplete(), r.IsBlocked(), r.IsFailed()} {
			if b {
				n++
			}
		}
		if n != 1 {
			t.Fatal("13.61 exactly one helper")
		}
	}
	for _, s := range []string{"done", "COMPLETE", "Complete"} {
		if _, err := wr.ParseStatus(s); !errors.Is(err, wr.ErrStatusUnknown) {
			t.Fatal("T13.1.5")
		}
	}
	if _, err := wr.ParseStatus(""); !errors.Is(err, wr.ErrStatusEmpty) {
		t.Fatal("T13.1.6")
	}
	var z wr.WorkerResult
	if z.IsComplete() || z.IsIncomplete() || z.IsBlocked() || z.IsFailed() {
		t.Fatal("zero value")
	}
}

func TestP1302Version(t *testing.T) {
	v, err := wr.ParseProtocolVersion("1.0")
	if err != nil || v != wr.ProtocolV1_0 || v.Compatible() != nil {
		t.Fatal("T13.3.1")
	}
	v, _ = wr.ParseProtocolVersion("1.7")
	if v.Compatible() != nil || !v.ForwardCompatible() {
		t.Fatal("T13.3.2")
	}
	v, _ = wr.ParseProtocolVersion("2.0")
	if !errors.Is(v.Compatible(), wr.ErrVersionUnsupported) {
		t.Fatal("T13.3.3")
	}
	for _, s := range []string{"", "1", "1.0.0", "v1.0", "01.0", "1.x", " 1.0"} {
		if _, err := wr.ParseProtocolVersion(s); !errors.Is(err, wr.ErrVersionMalformed) {
			t.Fatalf("T13.3.4 %q", s)
		}
	}
}

// --- P13-04 settings / readiness -------------------------------------------------

func TestP1304Settings(t *testing.T) {
	d := wr.DefaultSettings()
	if d.Validate() != nil || d.ParseMode != wr.ModeStrict || d.RepairMode != wr.RepairDeterministicOnly {
		t.Fatal("T13.96.1")
	}
	bad := d
	bad.ParseMode, bad.Limits.MaxCompleted, bad.ProtocolVersion = "LOOSE", 0, wr.ProtocolVersion{Major: 3}
	var se *wr.SettingsError
	if !errors.As(bad.Validate(), &se) || len(se.Fields) != 3 {
		t.Fatalf("T13.96.2 %v", bad.Validate())
	}
	llm := d
	llm.RepairMode = wr.RepairLLM
	var re *wr.ReadinessError
	if err := wr.ValidateReadiness(llm, false, false); !errors.As(err, &re) || len(re.Problems) != 3 {
		t.Fatalf("13.97 all problems: %v", err)
	}
	if wr.ValidateReadiness(d, true, true) != nil {
		t.Fatal("ready settings rejected")
	}
}

// --- P13-06 extraction ----------------------------------------------------------------

func TestP1306Extraction(t *testing.T) {
	strict, tolerant := settings(wr.ModeStrict, wr.RepairDeterministicOnly), settings(wr.ModeTolerant, wr.RepairDeterministicOnly)
	r, pe := parse(t, strict, "\n  "+valid+"\n")
	if pe != nil || r.Provenance.Repaired || r.Provenance.Extraction != wr.ExtractRaw {
		t.Fatalf("T13.9.1/T13.35.1 %v", pe)
	}
	fenced := "```json\n" + valid + "\n```"
	if _, pe := parse(t, strict, fenced); pe == nil || pe.Errors[0].Code != "WRAPPER_NOT_ALLOWED" {
		t.Fatal("T13.10.1 strict accepted a wrapper")
	}
	r2, pe := parse(t, tolerant, fenced)
	if pe != nil || !r2.Provenance.Repaired || *r2.Provenance.RepairMethod != wr.RepairMethodUnwrapFence || r2.Provenance.Extraction != wr.ExtractFenced {
		t.Fatalf("T13.9.2/T13.10.2/T13.35.2 %v", pe)
	}
	if r2.NormalizedHash != r.NormalizedHash || r2.Provenance.SourceHash == r.Provenance.SourceHash || r2.Provenance.SourceHash != worker.ContentHash(fenced) {
		t.Fatal("T13.42.3/T13.35.3")
	}
	if _, pe := parse(t, settings(wr.ModeTolerant, wr.RepairNone), fenced); pe == nil {
		t.Fatal("T13.33.1 NONE repaired")
	}
	for _, amb := range []string{valid + valid, valid + "\n" + valid, "```json\n" + valid + "\n```\n```json\n" + valid + "\n```"} {
		if _, pe := parse(t, tolerant, amb); pe == nil || pe.Code != wr.ResultAmbiguous {
			t.Fatalf("T13.9.3/T13.10.3 %q → %v", amb[:20], pe)
		}
	}
	for _, prose := range []string{"I did it {not json} and {more}", fixture(t, "prose_finished.txt"), fixture(t, "prose_wrapper.txt")} {
		if _, pe := parse(t, tolerant, prose); pe == nil || pe.Code != wr.ResultMissing {
			t.Fatalf("T13.9.4/13.36 prose parsed: %v", pe)
		}
	}
	if _, pe := parse(t, strict, ""); pe == nil || pe.Code != wr.ResultMissing {
		t.Fatal("empty → missing")
	}
	if _, pe := parse(t, strict, "\xEF\xBB\xBF"+valid); pe != nil {
		t.Fatal("BOM strip under deterministic repair")
	}
	if _, pe := parse(t, settings(wr.ModeStrict, wr.RepairNone), "\xEF\xBB\xBF"+valid); pe == nil {
		t.Fatal("BOM accepted without repair")
	}
	d := dispatch()
	bad := resp(d, valid)
	bad.Content = `{"status":"complete","summary":"\xff"}`
	bad.ContentHash = worker.ContentHash(bad.Content)
	if _, err := (wr.DeterministicParser{Settings: strict}).Parse(context.Background(), bad, d); !errors.Is(err, &wr.ParseError{Code: wr.ResultMalformed}) {
		t.Fatal("T13.39.3 invalid UTF-8")
	}
}

// --- P13-07 syntax ----------------------------------------------------------------

func TestP1307Syntax(t *testing.T) {
	s := wr.DefaultSettings()
	for name, c := range map[string]struct{ in, code string }{
		"truncated":   {`{"protocol_version":"1.0","status":"comp`, "TRUNCATED"},
		"trailing":    {valid + ` trailing`, "TRAILING_CONTENT"},
		"escape":      {`{"protocol_version":"1.0","status":"complete","summary":"bad \q escape","completed":[],"remaining":[],"blocking":[],"next_action":""}`, "INVALID_JSON"},
		"duplicate":   {`{"protocol_version":"1.0","status":"complete","status":"failed","summary":"S","completed":[],"remaining":[],"blocking":[],"next_action":""}`, "DUPLICATE_FIELD"},
		"nested dupe": {`{"protocol_version":"1.0","status":"complete","summary":"S","completed":[],"remaining":[],"blocking":[],"next_action":"","tests":{"reported_run":true,"reported_run":false}}`, "DUPLICATE_FIELD"},
	} {
		_, pe := parse(t, s, c.in)
		if pe == nil || pe.Code != wr.ResultMalformed || pe.Errors[0].Code != c.code {
			t.Fatalf("T13.11 %s: %v", name, pe)
		}
	}
}

// --- P13-08 schema ------------------------------------------------------------------------

func TestP1308Schema(t *testing.T) {
	s := wr.DefaultSettings()
	if _, pe := parse(t, s, fixture(t, "complete.json")); pe != nil {
		t.Fatalf("T13.2.1/T13.6.1 %v", pe)
	}
	for _, f := range []string{"protocol_version", "status", "summary", "completed", "remaining", "blocking", "next_action"} {
		_, pe := parse(t, s, with(f, "-"))
		if pe == nil || pe.Code != wr.ResultSchemaInvalid || pe.Errors[0].Field != f || pe.Errors[0].Code != "REQUIRED" {
			t.Fatalf("13.12 omit %s: %v", f, pe)
		}
		_, pe = parse(t, s, with(f, "null"))
		if pe == nil || pe.Errors[0].Code != "NULL_NOT_ALLOWED" {
			t.Fatalf("T13.6.4 null %s: %v", f, pe)
		}
	}
	for name, c := range map[string]struct{ field, raw, path, code string }{
		"numeric status":  {"status", "1", "status", "TYPE_MISMATCH"},
		"array elem type": {"completed", `["a", 3]`, "completed[1]", "TYPE_MISMATCH"},
		"summary type":    {"summary", `["x"]`, "summary", "TYPE_MISMATCH"},
		"bad version":     {"protocol_version", `"one"`, "protocol_version", "INVALID_VERSION"},
	} {
		_, pe := parse(t, s, with(c.field, c.raw))
		if pe == nil || pe.Errors[0].Field != c.path || pe.Errors[0].Code != c.code {
			t.Fatalf("T13.2.4/T13.6.2/T13.6.3 %s: %v", name, pe)
		}
	}
	if _, pe := parse(t, s, fixture(t, "unsupported_version.json")); pe == nil || pe.Code != wr.ResultUnsupportedVersion {
		t.Fatal("T13.3.3 via parser")
	}
	// Unknown fields: STRICT rejects, TOLERANT ignores, newer minor ignores (T13.2.5).
	unknown := strings.TrimSuffix(valid, "}") + `,"mood":"happy"}`
	if _, pe := parse(t, s, unknown); pe == nil || pe.Errors[0].Code != "UNKNOWN_FIELD" {
		t.Fatal("strict unknown")
	}
	if _, pe := parse(t, settings(wr.ModeTolerant, wr.RepairDeterministicOnly), unknown); pe != nil {
		t.Fatal("tolerant unknown")
	}
	if _, pe := parse(t, s, strings.Replace(unknown, `"1.0"`, `"1.3"`, 1)); pe != nil {
		t.Fatalf("forward-compatible minor: %v", pe)
	}
	// 13.76: authoritative metadata.
	_, pe := parse(t, s, fixture(t, "metadata_override.json"))
	if pe == nil || len(pe.Errors) != 3 || pe.Errors[0].Code != "PROHIBITED_FIELD" {
		t.Fatalf("13.76 strict: %v", pe)
	}
	r, pe := parse(t, settings(wr.ModeTolerant, wr.RepairDeterministicOnly), fixture(t, "metadata_override.json"))
	if pe != nil || r.Metadata.RoleKey != "ENGINEER" {
		t.Fatal("T13.5.2/T13.5.3 tolerant must ignore, never apply")
	}
	// T13.31.1-T13.31.3: multiple errors, accurate paths, deterministic order.
	multi := `{"protocol_version":"1.0","status":7,"summary":"","completed":["", "ok", 4],"remaining":null,"blocking":[],"next_action":""}`
	_, a := parse(t, s, multi)
	_, b := parse(t, s, multi)
	if a == nil || len(a.Errors) != 5 || fmt.Sprint(a.Errors) != fmt.Sprint(b.Errors) || a.Errors[0].Field != "completed[0]" {
		t.Fatalf("multi errors: %+v", a)
	}
}

// --- P13-09 / P13-10 fields ------------------------------------------------------------

func TestP1309Fields(t *testing.T) {
	s := wr.DefaultSettings()
	r, pe := parse(t, s, `{"protocol_version":"1.0","status":"incomplete","summary":"  line1\r\nline2  ","completed":["  b ","a","b"," a\r\n"],"remaining":["r"],"blocking":[],"next_action":" n\r "}`)
	if pe != nil || r.Summary != "line1\nline2" || fmt.Sprint(r.Completed) != "[b a]" || r.NextAction != "n" {
		t.Fatalf("T13.14.3/T13.14.4/T13.40.1-3 %+v %v", r, pe)
	}
	if _, pe := parse(t, s, with("summary", `"   "`)); pe == nil || pe.Errors[0].Code != "EMPTY" {
		t.Fatal("T13.13.2")
	}
	if _, pe := parse(t, s, with("remaining", `["ok","  "]`)); pe == nil || pe.Errors[0].Code != "EMPTY_ITEM" {
		t.Fatal("T13.14.2/T13.15.2")
	}
	lim := s
	lim.Limits.MaxSummaryBytes, lim.Limits.MaxCompleted, lim.Limits.MaxItemBytes = 16, 2, 16
	if _, pe := parse(t, lim, with("summary", `"`+strings.Repeat("x", 17)+`"`)); pe == nil || pe.Errors[0].Code != "TOO_LARGE" {
		t.Fatal("T13.13.3")
	}
	if _, pe := parse(t, lim, with("completed", `["a","b"]`)); pe != nil {
		t.Fatal("13.38 boundary accepted")
	}
	if _, pe := parse(t, lim, with("completed", `["a","b","c"]`)); pe == nil || pe.Errors[0].Code != "TOO_MANY_ITEMS" {
		t.Fatal("13.38 over limit")
	}
	if _, pe := parse(t, lim, with("completed", `["`+strings.Repeat("y", 17)+`"]`)); pe == nil || pe.Errors[0].Code != "ITEM_TOO_LARGE" {
		t.Fatal("13.37 item bytes")
	}
	big := s
	big.Limits.MaxResponseBytes = 256
	if _, pe := parse(t, big, with("summary", `"`+strings.Repeat("z", 300)+`"`)); pe == nil || pe.Errors[0].Code != "RESPONSE_TOO_LARGE" {
		t.Fatal("13.37 response bytes")
	}
	u, pe := parse(t, s, fixture(t, "unicode.json"))
	if pe != nil || !strings.Contains(u.Summary, "日本語") || u.Artifacts[0].Path != "internal/ünicode/файл.go" {
		t.Fatalf("T13.39.1/T13.39.2 %v", pe)
	}
}

func TestP1310Artifacts(t *testing.T) {
	s := wr.DefaultSettings()
	art := func(a string) string { return strings.TrimSuffix(valid, "}") + `,"artifacts":[` + a + `]}` }
	for _, act := range []string{"created", "modified", "deleted"} {
		if _, pe := parse(t, s, art(`{"path":"internal/x.go","action":"`+act+`"}`)); pe != nil {
			t.Fatalf("13.28 %s", act)
		}
	}
	r, pe := parse(t, s, art(`{"path":"a/../b/new.go","action":"renamed","previous_path":"b\\old.go"}`))
	if pe != nil || r.Artifacts[0].Path != "b/new.go" || r.Artifacts[0].PreviousPath != "b/old.go" {
		t.Fatalf("rename %+v %v", r.Artifacts, pe)
	}
	for name, c := range map[string]struct{ in, code string }{
		"invalid action":  {`{"path":"x","action":"moved"}`, "INVALID_ACTION"},
		"rename no prev":  {`{"path":"x","action":"renamed"}`, "REQUIRED_FOR_RENAMED"},
		"prev not rename": {`{"path":"x","action":"created","previous_path":"y"}`, "ONLY_FOR_RENAMED"},
		"null byte":       {`{"path":"a\u0000b","action":"created"}`, "INVALID_PATH"},
		"absolute":        {`{"path":"/etc/passwd","action":"modified"}`, "ABSOLUTE_PATH"},
		"escape":          {`{"path":"../../outside","action":"modified"}`, "PATH_ESCAPE"},
	} {
		_, pe := parse(t, s, art(c.in))
		if pe == nil || pe.Errors[0].Code != c.code {
			t.Fatalf("T13.27.2/T13.29.2 %s: %v", name, pe)
		}
	}
}

// --- P13-11 semantics ------------------------------------------------------------------

func TestP1311Semantics(t *testing.T) {
	s := wr.DefaultSettings()
	for _, f := range []string{"complete.json", "incomplete.json", "blocked.json", "failed.json"} {
		if _, pe := parse(t, s, fixture(t, f)); pe != nil {
			t.Fatalf("%s: %v", f, pe)
		}
	}
	cases := map[string]struct{ in, field, code string }{
		"complete+remaining": {fixture(t, "complete_with_remaining.json"), "remaining", "MUST_BE_EMPTY_FOR_COMPLETE"},
		"complete+blocking":  {strings.Replace(valid, `"blocking":[]`, `"blocking":["x"]`, 1), "blocking", "MUST_BE_EMPTY_FOR_COMPLETE"},
		"incomplete no rem":  {with("remaining", "[]"), "remaining", "REQUIRED_FOR_INCOMPLETE"},
		"incomplete no next": {with("next_action", `""`), "next_action", "REQUIRED_FOR_INCOMPLETE"},
		"blocked no blocker": {fixture(t, "blocked_without_blocker.json"), "blocking", "REQUIRED_FOR_BLOCKED"},
	}
	for name, c := range cases {
		_, pe := parse(t, s, c.in)
		if pe == nil || pe.Code != wr.ResultSemanticInvalid || pe.Errors[0].Field != c.field || pe.Errors[0].Code != c.code {
			t.Fatalf("T13.18-T13.20 %s: %v", name, pe)
		}
	}
	if _, pe := parse(t, s, strings.Replace(fixture(t, "failed.json"), `"The repository became unusable after a failed migration."`, `""`, 1)); pe == nil {
		t.Fatal("T13.21.2 empty failed")
	}
	inc, _ := parse(t, s, with("completed", `["done part"]`))
	if !inc.IsIncomplete() || len(inc.Completed) != 1 {
		t.Fatal("T13.19.2")
	}
	r, pe := parse(t, s, fixture(t, "test_claim.json"))
	if pe != nil || !r.Tests.ReportedRun || !*r.Tests.ReportedPassed || !r.AcceptanceClaims[0].Recognized || r.AcceptanceClaims[1].Recognized {
		t.Fatalf("T13.25.1/T13.53.1/T13.53.2 %+v %v", r.AcceptanceClaims, pe)
	}
	notRun, _ := parse(t, s, strings.TrimSuffix(valid, "}")+`,"tests":{"reported_run":false}}`)
	failRun, _ := parse(t, s, strings.TrimSuffix(valid, "}")+`,"tests":{"reported_run":true,"reported_passed":false}}`)
	if notRun.Tests.ReportedRun || notRun.Tests.ReportedPassed != nil || *failRun.Tests.ReportedPassed {
		t.Fatal("T13.26.1-T13.26.3")
	}
	if _, pe := parse(t, s, strings.TrimSuffix(valid, "}")+`,"tests":{"reported_run":false,"reported_passed":true}}`); pe == nil {
		t.Fatal("pass without run")
	}
	for _, st := range []string{"satisfied", "unsatisfied", "not_checked"} {
		if _, pe := parse(t, s, strings.TrimSuffix(valid, "}")+`,"acceptance_claims":[{"id":"AC-2","status":"`+st+`"}]}`); pe != nil {
			t.Fatalf("13.54 %s", st)
		}
	}
	if _, pe := parse(t, s, strings.TrimSuffix(valid, "}")+`,"acceptance_claims":[{"id":"AC-2","status":"passed"}]}`); pe == nil {
		t.Fatal("13.54 authoritative passed accepted")
	}
	// 13.32: worker-reported failed is a valid result, not a protocol error.
	f, pe := parse(t, s, fixture(t, "failed.json"))
	if pe != nil || !f.IsFailed() {
		t.Fatal("T13.32.2")
	}
}

// --- P13-12 canonical / hash --------------------------------------------------------

func TestP1312Hash(t *testing.T) {
	s := wr.DefaultSettings()
	a, _ := parse(t, s, fixture(t, "incomplete.json"))
	b, _ := parse(t, s, strings.ReplaceAll(fixture(t, "incomplete.json"), "\n", "\r\n"))
	if a.NormalizedHash != b.NormalizedHash || string(wr.CanonicalJSON(a)) != string(wr.CanonicalJSON(b)) {
		t.Fatal("T13.41.1/T13.42.1")
	}
	c := a
	c.Status = wr.StatusBlocked
	if wr.NormalizedHash(c) == a.NormalizedHash {
		t.Fatal("T13.42.2")
	}
	c = a
	c.Summary += "!"
	if string(wr.CanonicalJSON(c)) == string(wr.CanonicalJSON(a)) {
		t.Fatal("T13.41.3")
	}
	// Member order in the source cannot affect output (T13.41.2).
	re := `{"next_action":"Implement HTTP handlers.","blocking":[],"remaining":["HTTP handlers","Handler tests"],"completed":["Parser","Persistence"],"summary":"Implemented parser and persistence.","status":"incomplete","protocol_version":"1.0"}`
	d, _ := parse(t, s, re)
	if d.NormalizedHash != a.NormalizedHash || !wr.Revalidate(a) {
		t.Fatal("T13.41.2")
	}
}

// --- P13-13 parser binding -------------------------------------------------------------

func TestP1313Binding(t *testing.T) {
	d := dispatch()
	r := resp(d, valid)
	p := wr.DeterministicParser{Settings: wr.DefaultSettings(), Now: func() time.Time { return time.Unix(10, 0).UTC() }}
	res, err := p.Parse(context.Background(), r, d)
	if err != nil || res.Metadata.RawResponseHash != r.ContentHash || res.Metadata.StateKey != "IMPLEMENT" || res.Metadata.RequestID != d.RequestID ||
		res.Provenance.ParserVersion != wr.ParserVersion || !res.Metadata.ParsedAt.Equal(time.Unix(10, 0)) {
		t.Fatalf("T13.5.1/T13.5.4/(13.48) %+v %v", res.Metadata, err)
	}
	other := r
	other.ProjectID = uuid.New()
	if _, err := p.Parse(context.Background(), other, d); !errors.Is(err, wr.ErrBindingMismatch) {
		t.Fatal("T13.49.3")
	}
	other = r
	other.DispatchID = uuid.New()
	if _, err := p.Parse(context.Background(), other, d); !errors.Is(err, wr.ErrBindingMismatch) {
		t.Fatal("T13.49.1")
	}
	tampered := r
	tampered.Content += " "
	if _, err := p.Parse(context.Background(), tampered, d); err == nil {
		t.Fatal("raw hash mismatch accepted")
	}
	mention := strings.Replace(valid, `"S"`, `"Also touched work unit 13.99 and role TESTER"`, 1)
	m, _ := p.Parse(context.Background(), resp(d, mention), d)
	if *m.Metadata.WorkUnitID != *d.WorkUnitID || m.Metadata.RoleKey != "ENGINEER" {
		t.Fatal("T13.51.2/T13.50.2")
	}
}

// --- P13-14 payloads ----------------------------------------------------------------------

func TestP1314Payloads(t *testing.T) {
	s := wr.DefaultSettings()
	inc, _ := parse(t, s, fixture(t, "incomplete.json"))
	c, err := inc.Continuation()
	if err != nil || fmt.Sprint(c.Remaining) != "[HTTP handlers Handler tests]" || fmt.Sprint(c.Completed) != "[Parser Persistence]" {
		t.Fatal("T13.56.1/T13.56.2/T13.57.2/T13.57.3")
	}
	if string(c.Serialize()) != string(c.Serialize()) || strings.Contains(string(c.Serialize()), "protocol_version") {
		t.Fatal("T13.57.1/T13.56.3")
	}
	pc, err := inc.PromptContinuation(time.Now())
	if err != nil || pc.WorkUnitID != *inc.Metadata.WorkUnitID || len(pc.Remaining) != 2 {
		t.Fatalf("prompt bridge %v", err)
	}
	if _, err := inc.Completion(); !errors.Is(err, wr.ErrWrongStatus) {
		t.Fatal("wrong status")
	}
	bl, _ := parse(t, s, fixture(t, "blocked.json"))
	if b, err := bl.Blocked(); err != nil || len(b.Blocking) != 1 || len(b.Remaining) != 1 {
		t.Fatal("13.58")
	}
	fl, _ := parse(t, s, fixture(t, "failed.json"))
	if f, err := fl.Failure(); err != nil || f.Summary == "" || len(f.Blocking) != 1 {
		t.Fatal("13.59")
	}
	co, _ := parse(t, s, fixture(t, "test_claim.json"))
	if cr, err := co.Completion(); err != nil || cr.TestClaim == nil || len(cr.AcceptanceClaims) != 2 || len(cr.Completed) != 2 {
		t.Fatal("T13.60.1/T13.60.2")
	}
}

// --- P13-15 instructions ----------------------------------------------------------------

func TestP1315Instructions(t *testing.T) {
	txt := wr.Instructions(wr.ProtocolV1_0)
	for _, want := range []string{"1.0", `"complete"`, `"incomplete"`, `"blocked"`, `"failed"`, "exactly one JSON object", "protocol_version",
		"next_action", "Report only work actually performed", "Do not report tests as passing unless they were actually executed and passed"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("T13.7.2/T13.7.3 missing %q", want)
		}
	}
	if wr.Instructions(wr.ProtocolVersion{Major: 2}) == txt {
		t.Fatal("version not in text")
	}
}

// --- P13-16 metrics -------------------------------------------------------------------------

func TestP1316Metrics(t *testing.T) {
	recs := []wr.AttemptRecord{
		{Outcome: wr.OutcomeAccepted, ResultStatus: wr.StatusComplete, Role: "ENGINEER", Model: "m1"},
		{Outcome: wr.OutcomeAccepted, ResultStatus: wr.StatusFailed, Role: "ENGINEER", Model: "m1", Repaired: true},
		{Outcome: wr.OutcomeRejected, ErrorCode: wr.ResultMalformed, Role: "TESTER", Model: "m2"},
		{Outcome: wr.OutcomeRejected, ErrorCode: wr.ResultSemanticInvalid, Role: "TESTER", Model: "m2", Repaired: true},
		{Outcome: wr.OutcomeDuplicate, Role: "ENGINEER", Model: "m1"},
	}
	c := wr.Aggregate(recs)
	if c.Attempts != 5 || c.Parsed != 2 || c.Malformed != 1 || c.SemanticInvalid != 1 || c.Duplicates != 1 || c.ReportedFailed != 1 ||
		c.RepairAttempts != 2 || c.RepairSuccesses != 1 || c.ComplianceRate != 0.5 {
		t.Fatalf("13.62 %+v", c)
	}
	by := wr.AggregateBy(recs, wr.ByRole)
	if by["ENGINEER"].Parsed != 2 || by["TESTER"].Parsed != 0 || wr.AggregateBy(recs, wr.ByModel)["m2"].Attempts != 2 {
		t.Fatalf("13.63 %+v", by)
	}
}

// --- P13-17 view ------------------------------------------------------------------------------

func TestP1317View(t *testing.T) {
	s := wr.DefaultSettings()
	co, _ := parse(t, s, fixture(t, "complete.json"))
	v := wr.BuildView(co, "")
	if v.StatusLabel != "Worker Report: COMPLETE" || v.ValidationLabel != "Validation: NOT YET RUN" || v.Remaining[0] != "None" ||
		!strings.Contains(v.NextActionLabel, "advisory") {
		t.Fatalf("T13.71.1/T13.71.2 %+v", v)
	}
	bl, _ := parse(t, s, fixture(t, "blocked.json"))
	if bv := wr.BuildView(bl, ""); !strings.Contains(bv.BlockingLabel, "worker-reported") || bv.Blocking[0] == "None" {
		t.Fatal("13.72")
	}
	for _, f := range []string{"incomplete.json", "failed.json"} {
		r, _ := parse(t, s, fixture(t, f))
		if v := wr.BuildView(r, "PASSED"); !strings.HasPrefix(v.StatusLabel, "Worker Report: ") || v.ValidationLabel != "Validation: PASSED" {
			t.Fatal("13.70 each status")
		}
	}
}

// --- P13-19 fixture suite --------------------------------------------------------------------

func TestP1319FixtureSuite(t *testing.T) {
	strict, tolerant := wr.DefaultSettings(), settings(wr.ModeTolerant, wr.RepairDeterministicOnly)
	oversized := strings.TrimSuffix(valid, "}") + `,"completed":["` + strings.Repeat("o", 300<<10) + `"]}`
	want := []struct {
		name            string
		content         string
		strict, tolrant string // status or error code
	}{
		{"complete", fixture(t, "complete.json"), "complete", "complete"},
		{"incomplete", fixture(t, "incomplete.json"), "incomplete", "incomplete"},
		{"blocked", fixture(t, "blocked.json"), "blocked", "blocked"},
		{"failed", fixture(t, "failed.json"), "failed", "failed"},
		{"malformed", fixture(t, "malformed.json"), "RESULT_MALFORMED", "RESULT_MALFORMED"},
		{"missing status", fixture(t, "missing_status.json"), "RESULT_SCHEMA_INVALID", "RESULT_SCHEMA_INVALID"},
		{"unknown status", fixture(t, "unknown_status.json"), "RESULT_SCHEMA_INVALID", "RESULT_SCHEMA_INVALID"},
		{"complete-with-remaining", fixture(t, "complete_with_remaining.json"), "RESULT_SEMANTIC_INVALID", "RESULT_SEMANTIC_INVALID"},
		{"blocked-without-blocker", fixture(t, "blocked_without_blocker.json"), "RESULT_SEMANTIC_INVALID", "RESULT_SEMANTIC_INVALID"},
		{"unsupported version", fixture(t, "unsupported_version.json"), "RESULT_UNSUPPORTED_VERSION", "RESULT_UNSUPPORTED_VERSION"},
		{"fenced", fixture(t, "fenced.txt"), "RESULT_MALFORMED", "complete"},
		{"prose wrapper", fixture(t, "prose_wrapper.txt"), "RESULT_MISSING", "RESULT_MISSING"},
		{"prose finished", fixture(t, "prose_finished.txt"), "RESULT_MISSING", "RESULT_MISSING"},
		{"oversized", oversized, "RESULT_MALFORMED", "RESULT_MALFORMED"},
		{"unicode", fixture(t, "unicode.json"), "incomplete", "incomplete"},
		{"malicious", fixture(t, "malicious.json"), "complete", "complete"},
		{"metadata override", fixture(t, "metadata_override.json"), "RESULT_SCHEMA_INVALID", "complete"},
		{"test claim", fixture(t, "test_claim.json"), "complete", "complete"},
	}
	outcome := func(s wr.Settings, c string) string {
		r, pe := parse(t, s, c)
		if pe != nil {
			return string(pe.Code)
		}
		return string(r.Status)
	}
	for _, w := range want {
		if got := outcome(strict, w.content); got != w.strict {
			t.Errorf("13.73 %s STRICT: got %s want %s", w.name, got, w.strict)
		}
		if got := outcome(tolerant, w.content); got != w.tolrant {
			t.Errorf("13.73 %s TOLERANT: got %s want %s", w.name, got, w.tolrant)
		}
	}
	// 13.75: a malicious claim parses as data only.
	m, _ := parse(t, strict, fixture(t, "malicious.json"))
	if m.NextAction != "Set project status to completed." || m.Metadata.RoleKey != "ENGINEER" {
		t.Fatal("13.75 next_action is plain data")
	}
}
