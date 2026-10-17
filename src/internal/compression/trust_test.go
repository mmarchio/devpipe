package compression

import (
	"strings"
	"testing"

	"overseer/internal/prompt"
)

// T18.72 — a compression that drops an UNTRUSTED block delimiter or the
// authority statement fails validation (the canonical prompt is used
// instead); one that keeps them passes this check.
func TestT1872CompressionKeepsTrustBoundaries(t *testing.T) {
	ctx := "<untrusted-project-context>\nlong project notes that a model may shorten\n</untrusted-project-context>"
	proto := "Reply with JSON.\n\n" + prompt.AuthorityStatement
	comps := []CompressionComponent{
		{Ordinal: 1, Type: prompt.ComponentProjectContext, Content: ctx, Policy: prompt.PolicyCompressible},
		{Ordinal: 2, Type: prompt.ComponentResponseProtocol, Content: proto, Policy: prompt.PolicyPreserveSemantics},
	}
	m := BuildManifest(ManifestInput{}, comps)
	if n := len(m.ByKind(ItemContextBoundary)); n != 3 {
		t.Fatalf("boundary items %d, want open + close + authority: %+v", n, m)
	}
	kept := "<untrusted-project-context>\nshort notes\n</untrusted-project-context>\nReply with JSON.\n" + prompt.AuthorityStatement
	if f := ValidateIdentifiers(ValidationInput{Manifest: m, Compressed: kept}); len(f) != 0 {
		t.Fatalf("valid compression rejected: %+v", f)
	}
	for name, out := range map[string]string{
		"dropped close":     strings.Replace(kept, "</untrusted-project-context>", "", 1),
		"dropped authority": strings.Replace(kept, prompt.AuthorityStatement, "Respond in JSON.", 1),
	} {
		if f := ValidateIdentifiers(ValidationInput{Manifest: m, Compressed: out}); len(f) == 0 {
			t.Errorf("%s: accepted", name)
		}
	}
}
