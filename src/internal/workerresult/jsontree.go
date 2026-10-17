package workerresult

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// valueKind is a JSON value type.
type valueKind int

const (
	kindNull valueKind = iota
	kindBool
	kindNumber
	kindString
	kindArray
	kindObject
)

func (k valueKind) String() string {
	return [...]string{"null", "boolean", "number", "string", "array", "object"}[k]
}

type member struct {
	key string
	val *value
}

// value is an ordered JSON tree (object member order preserved; no map
// iteration anywhere, WBS 13.41.2).
type value struct {
	kind valueKind
	str  string
	b    bool
	num  json.Number
	arr  []*value
	obj  []member
}

func (v *value) get(key string) (*value, bool) {
	for _, m := range v.obj {
		if m.key == key {
			return m.val, true
		}
	}
	return nil, false
}

const maxDepth = 16

type syntaxFail struct {
	code    WorkerResultErrorCode
	vcode   string
	message string
}

// parseTree validates JSON syntax token by token (WBS 13.11): truncated
// input, trailing garbage, invalid escapes and duplicate keys at any depth
// are rejected; nothing is repaired. A second top-level object is
// AMBIGUOUS (WBS 13.9.3).
func parseTree(b []byte) (*value, *syntaxFail) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	root, f := readValue(dec, 0)
	if f != nil {
		return nil, f
	}
	tok, err := dec.Token()
	switch {
	case errors.Is(err, io.EOF):
		return root, nil
	case err == nil && tok == json.Delim('{'):
		return nil, &syntaxFail{ResultAmbiguous, "MULTIPLE_OBJECTS", "more than one JSON object"}
	}
	return nil, &syntaxFail{ResultMalformed, "TRAILING_CONTENT", "content after the protocol object"}
}

func syntaxError(err error) *syntaxFail {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return &syntaxFail{ResultMalformed, "TRUNCATED", "JSON ends unexpectedly"}
	}
	return &syntaxFail{ResultMalformed, "INVALID_JSON", err.Error()}
}

func readValue(dec *json.Decoder, depth int) (*value, *syntaxFail) {
	if depth > maxDepth {
		return nil, &syntaxFail{ResultMalformed, "TOO_DEEP", "JSON nesting too deep"}
	}
	tok, err := dec.Token()
	if err != nil {
		return nil, syntaxError(err)
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			v := &value{kind: kindObject}
			seen := map[string]bool{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, syntaxError(err)
				}
				key, ok := kt.(string)
				if !ok {
					return nil, &syntaxFail{ResultMalformed, "INVALID_JSON", "object key is not a string"}
				}
				if seen[key] {
					return nil, &syntaxFail{ResultMalformed, "DUPLICATE_FIELD", "duplicate field " + key}
				}
				seen[key] = true
				mv, f := readValue(dec, depth+1)
				if f != nil {
					return nil, f
				}
				v.obj = append(v.obj, member{key, mv})
			}
			if _, err := dec.Token(); err != nil {
				return nil, syntaxError(err)
			}
			return v, nil
		case '[':
			v := &value{kind: kindArray}
			for dec.More() {
				ev, f := readValue(dec, depth+1)
				if f != nil {
					return nil, f
				}
				v.arr = append(v.arr, ev)
			}
			if _, err := dec.Token(); err != nil {
				return nil, syntaxError(err)
			}
			return v, nil
		}
		return nil, &syntaxFail{ResultMalformed, "INVALID_JSON", "unexpected delimiter"}
	case string:
		return &value{kind: kindString, str: t}, nil
	case json.Number:
		return &value{kind: kindNumber, num: t}, nil
	case bool:
		return &value{kind: kindBool, b: t}, nil
	case nil:
		return &value{kind: kindNull}, nil
	}
	return nil, &syntaxFail{ResultMalformed, "INVALID_JSON", "unexpected token"}
}

// render writes the value as compact JSON in member order (canonical for
// opaque payloads such as the P16-80 plan object).
func (v *value) render(b *bytes.Buffer) {
	switch v.kind {
	case kindNull:
		b.WriteString("null")
	case kindBool:
		if v.b {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case kindNumber:
		b.WriteString(v.num.String())
	case kindString:
		jstr(b, v.str)
	case kindArray:
		b.WriteByte('[')
		for i, e := range v.arr {
			if i > 0 {
				b.WriteByte(',')
			}
			e.render(b)
		}
		b.WriteByte(']')
	case kindObject:
		b.WriteByte('{')
		for i, m := range v.obj {
			if i > 0 {
				b.WriteByte(',')
			}
			jstr(b, m.key)
			b.WriteByte(':')
			m.val.render(b)
		}
		b.WriteByte('}')
	}
}
