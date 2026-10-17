package workerresult

import (
	"bytes"
	"encoding/json"
)

func jstr(b *bytes.Buffer, s string) {
	var tmp bytes.Buffer
	enc := json.NewEncoder(&tmp)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	b.Write(bytes.TrimRight(tmp.Bytes(), "\n"))
}

func jlist(b *bytes.Buffer, xs []string) {
	b.WriteByte('[')
	for i, x := range xs {
		if i > 0 {
			b.WriteByte(',')
		}
		jstr(b, x)
	}
	b.WriteByte(']')
}

// CanonicalJSON is the byte-stable serialization of the worker-reported
// content (WBS 13.41): fixed field order, normalized values, no metadata,
// provenance or timestamps (PLAN Decision 4).
func CanonicalJSON(r WorkerResult) []byte {
	var b bytes.Buffer
	b.WriteString(`{"protocol_version":`)
	jstr(&b, r.ProtocolVersion.String())
	b.WriteString(`,"status":`)
	jstr(&b, string(r.Status))
	b.WriteString(`,"summary":`)
	jstr(&b, r.Summary)
	b.WriteString(`,"completed":`)
	jlist(&b, r.Completed)
	b.WriteString(`,"remaining":`)
	jlist(&b, r.Remaining)
	b.WriteString(`,"blocking":`)
	jlist(&b, r.Blocking)
	b.WriteString(`,"next_action":`)
	jstr(&b, r.NextAction)
	b.WriteString(`,"tests":`)
	if t := r.Tests; t == nil {
		b.WriteString("null")
	} else {
		b.WriteString(`{"reported_run":`)
		b.WriteString(map[bool]string{true: "true", false: "false"}[t.ReportedRun])
		b.WriteString(`,"reported_passed":`)
		switch {
		case t.ReportedPassed == nil:
			b.WriteString("null")
		case *t.ReportedPassed:
			b.WriteString("true")
		default:
			b.WriteString("false")
		}
		b.WriteString(`,"summary":`)
		jstr(&b, t.Summary)
		b.WriteByte('}')
	}
	b.WriteString(`,"artifacts":[`)
	for i, a := range r.Artifacts {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"path":`)
		jstr(&b, a.Path)
		b.WriteString(`,"action":`)
		jstr(&b, string(a.Action))
		b.WriteString(`,"previous_path":`)
		jstr(&b, a.PreviousPath)
		b.WriteByte('}')
	}
	b.WriteString(`],"acceptance_claims":[`)
	for i, c := range r.AcceptanceClaims {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"id":`)
		jstr(&b, c.ID)
		b.WriteString(`,"status":`)
		jstr(&b, string(c.Status))
		b.WriteString(`,"evidence":`)
		jstr(&b, c.Evidence)
		b.WriteByte('}')
	}
	b.WriteByte(']')
	// The plan (P16-80) is appended only when present, so results without
	// one keep their Phase 13 hashes.
	if r.Plan != nil {
		b.WriteString(`,"plan":`)
		b.Write(r.Plan)
	}
	b.WriteByte('}')
	return b.Bytes()
}

// NormalizedHash is SHA-256 of CanonicalJSON (WBS 13.42), stored separately
// from the raw response hash.
func NormalizedHash(r WorkerResult) string { return sha(CanonicalJSON(r)) }
