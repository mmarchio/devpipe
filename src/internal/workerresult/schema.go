package workerresult

import (
	"bytes"
	"fmt"
	"path"
	"strings"
	"unicode"
)

// Required v1 fields (WBS 13.6/13.12).
var requiredFields = []string{"protocol_version", "status", "summary", "completed", "remaining", "blocking", "next_action"}

// Optional v1 fields (WBS 13.25/13.27/13.53).
// "plan" (P16-80) carries a planner's plan candidate as an opaque object:
// the engine validates it deterministically; the protocol only checks it
// is a JSON object.
var optionalFields = map[string]bool{"tests": true, "artifacts": true, "acceptance_claims": true, "plan": true}

// prohibitedFields name authoritative metadata a worker may never supply
// (WBS 13.5/13.49-13.51/13.55/13.76).
var prohibitedFields = map[string]bool{
	"project_id": true, "phase_id": true, "work_unit_id": true, "dispatch_id": true, "request_id": true, "session_id": true,
	"state": true, "state_key": true, "role": true, "role_key": true, "runtime_state_id": true, "context_used": true,
	"context_tokens": true, "context_window": true, "metadata": true,
}

type checker struct {
	errs   []ValidationError
	limits Limits
}

func (c *checker) add(field, code, msg string) {
	c.errs = append(c.errs, ValidationError{Field: field, Code: code, Message: msg})
}

// normalizeText applies the deterministic text normalization (WBS 13.40):
// CRLF / CR → LF and surrounding whitespace trimmed. Meaning is preserved.
func normalizeText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.TrimSpace(s)
}

func (c *checker) str(v *value, field string) (string, bool) {
	if v == nil {
		c.add(field, "REQUIRED", "field is required")
		return "", false
	}
	if v.kind != kindString {
		code := "TYPE_MISMATCH"
		if v.kind == kindNull {
			code = "NULL_NOT_ALLOWED"
		}
		c.add(field, code, "must be a string, got "+v.kind.String())
		return "", false
	}
	return v.str, true
}

// list validates an array of strings: count limit (before dedup), empty
// items, per-item size, exact-duplicate removal keeping order.
func (c *checker) list(v *value, field string, max int) []string {
	if v == nil {
		c.add(field, "REQUIRED", "field is required")
		return nil
	}
	if v.kind != kindArray {
		code := "TYPE_MISMATCH"
		if v.kind == kindNull {
			code = "NULL_NOT_ALLOWED"
		}
		c.add(field, code, "must be an array of strings, got "+v.kind.String())
		return nil
	}
	if max > 0 && len(v.arr) > max {
		c.add(field, "TOO_MANY_ITEMS", fmt.Sprintf("at most %d items allowed, got %d", max, len(v.arr)))
		return nil
	}
	out := []string{}
	seen := map[string]bool{}
	for i, e := range v.arr {
		f := fmt.Sprintf("%s[%d]", field, i)
		if e.kind != kindString {
			c.add(f, "TYPE_MISMATCH", "must be a string, got "+e.kind.String())
			continue
		}
		s := normalizeText(e.str)
		switch {
		case s == "":
			c.add(f, "EMPTY_ITEM", "item must not be empty")
		case c.limits.MaxItemBytes > 0 && len(s) > c.limits.MaxItemBytes:
			c.add(f, "ITEM_TOO_LARGE", fmt.Sprintf("item exceeds %d bytes", c.limits.MaxItemBytes))
		case !seen[s]:
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// draft is the schema-valid, normalized worker content before semantics.
type draft struct {
	version    ProtocolVersion
	status     WorkerResultStatus
	summary    string
	completed  []string
	remaining  []string
	blocking   []string
	nextAction string
	tests      *WorkerTestClaim
	artifacts  []ArtifactClaim
	claims     []AcceptanceClaim
	plan       []byte
}

// validateSchema applies the v1 schema to the tree (WBS 13.2/13.6/13.12-
// 13.17/13.25-13.29/13.37-13.40/13.53/13.76). Every field problem is
// collected. versionErr is set (and the rest skipped) when the declared
// version is unsupported.
func validateSchema(root *value, s Settings) (*draft, []ValidationError, bool) {
	c := &checker{limits: s.Limits}
	if root.kind != kindObject {
		c.add("$", "ROOT_NOT_OBJECT", "protocol response must be a JSON object")
		return nil, c.errs, false
	}
	d := &draft{}
	// Version first: it decides the field policy.
	if vs, ok := c.str(fieldOf(root, "protocol_version"), "protocol_version"); ok {
		v, err := ParseProtocolVersion(vs)
		if err != nil {
			c.add("protocol_version", "INVALID_VERSION", err.Error())
		} else if err := v.Compatible(); err != nil {
			c.add("protocol_version", "UNSUPPORTED_VERSION", err.Error())
			return nil, c.errs, true
		} else {
			d.version = v
		}
	}
	forward := d.version.ForwardCompatible()
	for _, m := range root.obj {
		switch {
		case prohibitedFields[m.key]:
			if s.ParseMode == ModeStrict {
				c.add(m.key, "PROHIBITED_FIELD", "authoritative metadata cannot be supplied by the worker")
			}
		case optionalFields[m.key]:
		case contains(requiredFields, m.key):
		default:
			if s.ParseMode == ModeStrict && !forward {
				c.add(m.key, "UNKNOWN_FIELD", "field is not part of protocol "+SupportedProtocol.String())
			}
		}
	}
	if st, ok := c.str(fieldOf(root, "status"), "status"); ok {
		parsed, err := ParseStatus(st)
		if err != nil {
			code := "UNKNOWN_STATUS"
			if err == ErrStatusEmpty {
				code = "EMPTY"
			}
			c.add("status", code, err.Error())
		}
		d.status = parsed
	}
	if sum, ok := c.str(fieldOf(root, "summary"), "summary"); ok {
		d.summary = normalizeText(sum)
		switch {
		case d.summary == "":
			c.add("summary", "EMPTY", "summary must not be empty")
		case s.Limits.MaxSummaryBytes > 0 && len(d.summary) > s.Limits.MaxSummaryBytes:
			c.add("summary", "TOO_LARGE", fmt.Sprintf("summary exceeds %d bytes", s.Limits.MaxSummaryBytes))
		}
	}
	d.completed = c.list(fieldOf(root, "completed"), "completed", s.Limits.MaxCompleted)
	d.remaining = c.list(fieldOf(root, "remaining"), "remaining", s.Limits.MaxRemaining)
	d.blocking = c.list(fieldOf(root, "blocking"), "blocking", s.Limits.MaxBlocking)
	if na, ok := c.str(fieldOf(root, "next_action"), "next_action"); ok {
		d.nextAction = normalizeText(na)
		if s.Limits.MaxNextActionByte > 0 && len(d.nextAction) > s.Limits.MaxNextActionByte {
			c.add("next_action", "TOO_LARGE", fmt.Sprintf("next_action exceeds %d bytes", s.Limits.MaxNextActionByte))
		}
	}
	if t, ok := root.get("tests"); ok {
		d.tests = c.tests(t)
	}
	if a, ok := root.get("artifacts"); ok {
		d.artifacts = c.artifacts(a, s.Limits.MaxArtifacts)
	}
	if a, ok := root.get("acceptance_claims"); ok {
		d.claims = c.acceptance(a, s.Limits.MaxAcceptance)
	}
	if p, ok := root.get("plan"); ok {
		if c.objectOf(p, "plan") {
			var b bytes.Buffer
			p.render(&b)
			d.plan = b.Bytes()
		}
	}
	return d, c.errs, false
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// fieldOf returns the member or nil (absent → REQUIRED via c.str/list).
func fieldOf(root *value, key string) *value {
	v, _ := root.get(key)
	return v
}

func (c *checker) objectOf(v *value, field string) bool {
	if v.kind != kindObject {
		c.add(field, "TYPE_MISMATCH", "must be an object, got "+v.kind.String())
		return false
	}
	return true
}

// tests parses the optional worker test claim (WBS 13.25/13.26).
func (c *checker) tests(v *value) *WorkerTestClaim {
	if v.kind == kindNull {
		return nil
	}
	if !c.objectOf(v, "tests") {
		return nil
	}
	tc := &WorkerTestClaim{}
	for _, m := range v.obj {
		f := "tests." + m.key
		switch m.key {
		case "reported_run":
			if m.val.kind != kindBool {
				c.add(f, "TYPE_MISMATCH", "must be a boolean")
			}
			tc.ReportedRun = m.val.b
		case "reported_passed":
			switch m.val.kind {
			case kindNull:
			case kindBool:
				b := m.val.b
				tc.ReportedPassed = &b
			default:
				c.add(f, "TYPE_MISMATCH", "must be a boolean or null")
			}
		case "summary":
			if m.val.kind != kindString {
				c.add(f, "TYPE_MISMATCH", "must be a string")
			}
			tc.Summary = normalizeText(m.val.str)
			if c.limits.MaxItemBytes > 0 && len(tc.Summary) > c.limits.MaxItemBytes {
				c.add(f, "TOO_LARGE", "test summary too large")
			}
		default:
			c.add(f, "UNKNOWN_FIELD", "field is not part of the test claim")
		}
	}
	if _, ok := v.get("reported_run"); !ok {
		c.add("tests.reported_run", "REQUIRED", "field is required")
	}
	if !tc.ReportedRun && tc.ReportedPassed != nil {
		c.add("tests.reported_passed", "INCONSISTENT", "a pass/fail result requires reported_run = true")
	}
	return tc
}

// artifactPath validates a worker-reported path for representation only
// (WBS 13.29): it is never opened or executed.
func artifactPath(p string) (string, string) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", "EMPTY"
	}
	for _, r := range p {
		if r == 0 || unicode.IsControl(r) {
			return "", "INVALID_PATH"
		}
	}
	p = strings.ReplaceAll(p, "\\", "/")
	if strings.HasPrefix(p, "/") || (len(p) > 1 && p[1] == ':') {
		return "", "ABSOLUTE_PATH"
	}
	clean := path.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, "../") || clean == "." {
		return "", "PATH_ESCAPE"
	}
	return clean, ""
}

func (c *checker) artifacts(v *value, max int) []ArtifactClaim {
	if v.kind != kindArray {
		c.add("artifacts", "TYPE_MISMATCH", "must be an array")
		return nil
	}
	if len(v.arr) > max {
		c.add("artifacts", "TOO_MANY_ITEMS", fmt.Sprintf("at most %d artifacts allowed, got %d", max, len(v.arr)))
		return nil
	}
	var out []ArtifactClaim
	for i, e := range v.arr {
		f := fmt.Sprintf("artifacts[%d]", i)
		if !c.objectOf(e, f) {
			continue
		}
		var a ArtifactClaim
		ok := true
		for _, m := range e.obj {
			switch m.key {
			case "path", "previous_path":
				if m.val.kind != kindString {
					c.add(f+"."+m.key, "TYPE_MISMATCH", "must be a string")
					ok = false
					continue
				}
				p, code := artifactPath(m.val.str)
				if code != "" {
					c.add(f+"."+m.key, code, "invalid artifact path")
					ok = false
				}
				if m.key == "path" {
					a.Path = p
				} else {
					a.PreviousPath = p
				}
			case "action":
				if m.val.kind != kindString {
					c.add(f+".action", "TYPE_MISMATCH", "must be a string")
					ok = false
					continue
				}
				act, err := ParseArtifactAction(m.val.str)
				if err != nil {
					c.add(f+".action", "INVALID_ACTION", err.Error())
					ok = false
				}
				a.Action = act
			default:
				c.add(f+"."+m.key, "UNKNOWN_FIELD", "field is not part of an artifact claim")
				ok = false
			}
		}
		for _, req := range []string{"path", "action"} {
			if _, has := e.get(req); !has {
				c.add(f+"."+req, "REQUIRED", "field is required")
				ok = false
			}
		}
		_, hasPrev := e.get("previous_path")
		if a.Action == ArtifactRenamed && !hasPrev {
			c.add(f+".previous_path", "REQUIRED_FOR_RENAMED", "a rename requires previous_path")
			ok = false
		}
		if a.Action != "" && a.Action != ArtifactRenamed && hasPrev {
			c.add(f+".previous_path", "ONLY_FOR_RENAMED", "previous_path is only valid for renamed")
			ok = false
		}
		if ok {
			out = append(out, a)
		}
	}
	return out
}

func (c *checker) acceptance(v *value, max int) []AcceptanceClaim {
	if v.kind != kindArray {
		c.add("acceptance_claims", "TYPE_MISMATCH", "must be an array")
		return nil
	}
	if len(v.arr) > max {
		c.add("acceptance_claims", "TOO_MANY_ITEMS", fmt.Sprintf("at most %d claims allowed, got %d", max, len(v.arr)))
		return nil
	}
	var out []AcceptanceClaim
	for i, e := range v.arr {
		f := fmt.Sprintf("acceptance_claims[%d]", i)
		if !c.objectOf(e, f) {
			continue
		}
		var a AcceptanceClaim
		ok := true
		for _, m := range e.obj {
			if m.val.kind != kindString {
				c.add(f+"."+m.key, "TYPE_MISMATCH", "must be a string")
				ok = false
				continue
			}
			switch m.key {
			case "id":
				a.ID = normalizeText(m.val.str)
				if a.ID == "" {
					c.add(f+".id", "EMPTY", "id must not be empty")
					ok = false
				}
			case "status":
				st, err := ParseAcceptanceClaimStatus(m.val.str)
				if err != nil {
					c.add(f+".status", "INVALID_STATUS", err.Error())
					ok = false
				}
				a.Status = st
			case "evidence":
				a.Evidence = normalizeText(m.val.str)
			default:
				c.add(f+"."+m.key, "UNKNOWN_FIELD", "field is not part of an acceptance claim")
				ok = false
			}
		}
		for _, req := range []string{"id", "status"} {
			if _, has := e.get(req); !has {
				c.add(f+"."+req, "REQUIRED", "field is required")
				ok = false
			}
		}
		if ok {
			out = append(out, a)
		}
	}
	return out
}
