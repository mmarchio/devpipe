package prompt

import (
	"errors"
	"fmt"
	"strings"
)

// DefaultContextDelimVersion is the built-in delimiter format version
// (WBS 8.39: the exact delimiter syntax is data, configurable/versioned,
// not scattered through code).
const DefaultContextDelimVersion = 1

// DeterministicFormat is one pinned version of the untrusted-context
// delimiter format (WBS 8.39): label-tagged angle-bracket boundaries with
// the WBS 8.40 escape scheme. The open/close syntax lives on this value,
// so a later format version changes the syntax in one place.
type DeterministicFormat struct {
	// name identifies the format ("default" or a custom version name);
	// version is the integer version (1 = the built-in default).
	name    string
	version int

	// openTmpl / closeTmpl each render one label placeholder (%s) into
	// the section's open/close delimiter.
	openTmpl  string
	closeTmpl string
}

var errMalformedFormat = errors.New("prompt: malformed delimiter format")

// NewDeterministicFormat returns the built-in default format
// (version 1: <label> / </label>).
func NewDeterministicFormat(version int) (*DeterministicFormat, error) {
	if version != DefaultContextDelimVersion {
		return nil, fmt.Errorf("prompt: unknown delimiter format version %d", version)
	}
	return &DeterministicFormat{name: "default", version: version, openTmpl: "<%s>", closeTmpl: "</%s>"}, nil
}

// NewDelimitedFormat builds a custom, versioned delimiter format
// (configurable syntax). Each template must carry exactly one %s
// placeholder; an empty version name is rejected.
func NewDelimitedFormat(version, openTmpl, closeTmpl string) (*DeterministicFormat, error) {
	if version == "" {
		return nil, errMalformedFormat
	}
	if n := strings.Count(openTmpl, "%s"); n != 1 {
		return nil, errMalformedFormat
	}
	if n := strings.Count(closeTmpl, "%s"); n != 1 {
		return nil, errMalformedFormat
	}
	return &DeterministicFormat{name: version, version: 1, openTmpl: openTmpl, closeTmpl: closeTmpl}, nil
}

// Version reports the format's integer version.
func (f *DeterministicFormat) Version() int {
	return f.version
}

// OpenTag returns the section's opening delimiter for label (T8.39.1).
func (f *DeterministicFormat) OpenTag(label string) string {
	return strings.ReplaceAll(f.openTmpl, "%s", label)
}

// CloseTag returns the section's closing delimiter for label (T8.39.1).
func (f *DeterministicFormat) CloseTag(label string) string {
	return strings.ReplaceAll(f.closeTmpl, "%s", label)
}

// ContextSection is one delimited block of untrusted content
// (project/WBS/task/test data, WBS 8.39): the content is escaped so the
// structural boundary is the section's own (WBS 8.40).
type ContextSection struct {
	format *DeterministicFormat
	label  string
	// raw is the original content, retained verbatim.
	raw string
}

// Section wraps content in a labelled, delimited section. The label is a
// canonical, non-empty, unescaped identifier (the component-context
// names: task-context, test-context, ...).
func (f *DeterministicFormat) Section(label, content string) (*ContextSection, error) {
	if label == "" || strings.ContainsAny(label, "<>/\\% ") {
		return nil, fmt.Errorf("prompt: invalid context section label %q", label)
	}
	return &ContextSection{format: f, label: label, raw: content}, nil
}

// Label reports the section's label.
func (s *ContextSection) Label() string {
	return s.label
}

// Version reports the delimiter format version the section was rendered
// with (configurable/versioned syntax, WBS 8.39).
func (s *ContextSection) Version() int {
	return s.format.Version()
}

// escape neutralizes the escape character and both structural delimiters
// inside untrusted content (WBS 8.40), so neither an embedded open tag,
// close tag, nor nested-looking section can break the boundary.
func (s *ContextSection) escape() string {
	open := s.format.OpenTag(s.label)
	close_ := s.format.CloseTag(s.label)
	var b strings.Builder
	i := 0
	for i < len(s.raw) {
		c := s.raw[i]
		if c == '\\' {
			b.WriteString("\\\\")
			i++
			continue
		}
		if strings.HasPrefix(s.raw[i:], open) {
			b.WriteString("\\")
			b.WriteString(open)
			i += len(open)
			continue
		}
		if strings.HasPrefix(s.raw[i:], close_) {
			b.WriteString("\\")
			b.WriteString(close_)
			i += len(close_)
			continue
		}
		b.WriteByte(c)
		i++
	}
	return b.String()
}

// Render returns the delimited section: open delimiter, newline, escaped
// content, newline, close delimiter. Repeated rendering is byte-identical
// (T8.39.4).
func (s *ContextSection) Render() string {
	open := s.format.OpenTag(s.label)
	close_ := s.format.CloseTag(s.label)
	return open + "\n" + s.escape() + "\n" + close_
}

// unescape reverses escape: the mapping is injective, so the original
// content is always reconstructable (T8.40.4).
func (s *ContextSection) unescape(esc string) string {
	open := s.format.OpenTag(s.label)
	close_ := s.format.CloseTag(s.label)
	var b strings.Builder
	i := 0
	for i < len(esc) {
		c := esc[i]
		if c == '\\' && i+1 < len(esc) && esc[i+1] == '\\' {
			b.WriteByte('\\')
			i += 2
			continue
		}
		if c == '\\' && (strings.HasPrefix(esc[i:], "\\"+open) || strings.HasPrefix(esc[i:], "\\"+close_)) {
			if strings.HasPrefix(esc[i:], "\\"+open) {
				b.WriteString(open)
				i += 1 + len(open)
			} else {
				b.WriteString(close_)
				i += 1 + len(close_)
			}
			continue
		}
		b.WriteByte(c)
		i++
	}
	return b.String()
}

// Content extracts and unescapes the original content from a rendered
// section produced by this format and label (T8.40.4): the first
// structural delimiter pair is the section's own; everything between them
// is the (escaped) content.
func (f *DeterministicFormat) Content(label, rendered string) (string, error) {
	open := f.OpenTag(label)
	close_ := f.CloseTag(label)
	i := strings.Index(rendered, open)
	if i < 0 {
		return "", fmt.Errorf("prompt: rendered section has no opening %s delimiter", open)
	}
	rest := rendered[i+len(open):]
	if !strings.HasPrefix(rest, "\n") {
		return "", fmt.Errorf("prompt: rendered section %s must open on its own line", open)
	}
	rest = rest[1:]
	j := strings.LastIndex(rest, close_)
	if j < 0 {
		return "", fmt.Errorf("prompt: rendered section has no closing %s delimiter", close_)
	}
	body := rest[:j]
	if !strings.HasSuffix(body, "\n") {
		return "", fmt.Errorf("prompt: rendered section %s must close on its own line", close_)
	}
	body = body[:len(body)-1]
	sec := &ContextSection{format: f, label: label}
	return sec.unescape(body), nil
}
