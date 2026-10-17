package compression

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"overseer/internal/prompt"
)

// Section is one policy-bounded part of the structured compression input
// (WBS 9.11). A component without <preserve> regions is one section (Part
// 1); a non-exact component with regions is split into its text parts (the
// component policy) and its region parts (PRESERVE_EXACT, the tags
// removed), in content order.
type Section struct {
	Ordinal int
	Part    int
	Type    prompt.PromptComponentType
	Policy  prompt.CompressionPolicy
	Content string
}

// StructuredInput is the rendered structured input plus its boundary token.
type StructuredInput struct {
	Text     string
	Boundary string
	Sections []Section
}

// SplitSections splits components into sections (see Section).
func SplitSections(comps []CompressionComponent) []Section {
	var out []Section
	for _, c := range comps {
		// An UNTRUSTED block's delimiter lines (WBS 18.72) are exact
		// sections of their own: only the content between them follows
		// the component's policy, so compression can never drop or alter
		// a trust boundary.
		if open, inner, close_, ok := splitTrustBlock(c.Content); ok {
			out = append(out, Section{Ordinal: c.Ordinal, Part: 1, Type: c.Type, Policy: prompt.PolicyPreserveExact, Content: open})
			ic := c
			ic.Content = inner
			for _, sec := range SplitSections([]CompressionComponent{ic}) {
				sec.Part++
				out = append(out, sec)
			}
			out = append(out, Section{Ordinal: c.Ordinal, Part: 1000, Type: c.Type, Policy: prompt.PolicyPreserveExact, Content: close_})
			continue
		}
		regs := PreserveRegionsOf(c)
		if c.Policy == prompt.PolicyPreserveExact || len(regs) == 0 {
			out = append(out, Section{Ordinal: c.Ordinal, Part: 1, Type: c.Type, Policy: c.Policy, Content: c.Content})
			continue
		}
		part := 0
		emit := func(p prompt.CompressionPolicy, text string) {
			if strings.TrimSpace(text) == "" {
				return
			}
			part++
			out = append(out, Section{Ordinal: c.Ordinal, Part: part, Type: c.Type, Policy: p, Content: text})
		}
		pos := 0
		for _, r := range regs {
			emit(c.Policy, c.Content[pos:r.Start-len(PreserveOpenTag)])
			emit(prompt.PolicyPreserveExact, r.Text)
			pos = r.End + len(PreserveCloseTag)
		}
		emit(c.Policy, c.Content[pos:])
	}
	return out
}

// chooseBoundary derives a deterministic boundary token that occurs in no
// section content, so task content can never spoof a policy boundary
// (T9.11.2): the token is derived from a hash of the contents and a counter,
// incremented until it is absent from every content.
func chooseBoundary(sections []Section) string {
	h := sha256.New()
	for _, s := range sections {
		h.Write([]byte(s.Content))
		h.Write([]byte{0})
	}
	seed := h.Sum(nil)
	for n := 0; ; n++ {
		sum := sha256.Sum256(append(append([]byte{}, seed...), []byte(strconv.Itoa(n))...))
		b := "CB" + strings.ToUpper(hex.EncodeToString(sum[:8]))
		clash := false
		for _, s := range sections {
			if strings.Contains(s.Content, b) {
				clash = true
				break
			}
		}
		if !clash {
			return b
		}
	}
}

// RenderStructuredInput renders the policy-framed compression input (WBS
// 9.11): every section carries its ordinal, part, policy and type; content
// is placed verbatim between boundary lines (no escaping — the boundary is
// chosen so it cannot occur in content), so exact source content is
// reconstructable byte-for-byte (T9.11.4). Deterministic.
func RenderStructuredInput(comps []CompressionComponent) StructuredInput {
	sections := SplitSections(comps)
	b := chooseBoundary(sections)
	var sb strings.Builder
	for i, s := range sections {
		if i > 0 {
			sb.WriteString("\n")
		}
		fmt.Fprintf(&sb, "<<<%s SECTION %d.%d %s %s>>>\n", b, s.Ordinal, s.Part, s.Policy, s.Type)
		sb.WriteString(s.Content)
		fmt.Fprintf(&sb, "\n<<<%s END %d.%d>>>\n", b, s.Ordinal, s.Part)
	}
	return StructuredInput{Text: sb.String(), Boundary: b, Sections: sections}
}

var headerRE = regexp.MustCompile(`^<<<(CB[0-9A-F]{16}) SECTION (\d+)\.(\d+) ([A-Z_]+) ([A-Z_]+)>>>$`)

// ErrMalformedStructuredInput reports an unparseable structured input.
var ErrMalformedStructuredInput = errors.New("compression: malformed structured input")

// ParseStructuredInput reverses RenderStructuredInput (T9.11.4).
func ParseStructuredInput(text string) (StructuredInput, error) {
	var out StructuredInput
	rest := text
	for strings.TrimSpace(rest) != "" {
		rest = strings.TrimLeft(rest, "\n")
		nl := strings.IndexByte(rest, '\n')
		if nl < 0 {
			return StructuredInput{}, ErrMalformedStructuredInput
		}
		m := headerRE.FindStringSubmatch(rest[:nl])
		if m == nil {
			return StructuredInput{}, ErrMalformedStructuredInput
		}
		if out.Boundary == "" {
			out.Boundary = m[1]
		} else if out.Boundary != m[1] {
			return StructuredInput{}, ErrMalformedStructuredInput
		}
		ord, _ := strconv.Atoi(m[2])
		part, _ := strconv.Atoi(m[3])
		end := "\n<<<" + m[1] + " END " + m[2] + "." + m[3] + ">>>\n"
		body := rest[nl+1:]
		j := strings.Index(body, end)
		if j < 0 {
			return StructuredInput{}, ErrMalformedStructuredInput
		}
		out.Sections = append(out.Sections, Section{
			Ordinal: ord, Part: part,
			Policy: prompt.CompressionPolicy(m[4]), Type: prompt.PromptComponentType(m[5]),
			Content: body[:j],
		})
		rest = body[j+len(end):]
	}
	out.Text = text
	return out, nil
}

// splitTrustBlock splits a component wrapped in an UNTRUSTED block into
// its open delimiter line, inner content and close delimiter line.
func splitTrustBlock(content string) (open, inner, close_ string, ok bool) {
	format, err := prompt.NewDeterministicFormat(prompt.DefaultContextDelimVersion)
	if err != nil {
		return "", "", "", false
	}
	for _, l := range prompt.UntrustedLabels() {
		o, c := format.OpenTag(l), format.CloseTag(l)
		if strings.HasPrefix(content, o+"\n") && strings.HasSuffix(content, "\n"+c) && len(content) >= len(o)+len(c)+2 {
			return o, content[len(o)+1 : len(content)-len(c)-1], c, true
		}
	}
	return "", "", "", false
}
