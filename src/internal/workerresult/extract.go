package workerresult

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode/utf8"
)

// Repair method names recorded in provenance (WBS 13.35).
const (
	RepairMethodStripBOM    = "STRIP_BOM"
	RepairMethodUnwrapFence = "UNWRAP_FENCE"
)

var bom = []byte{0xEF, 0xBB, 0xBF}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// Extract locates the protocol object in a raw response (WBS 13.9/13.10/
// 13.33). STRICT: the (whitespace-trimmed) body must be exactly one JSON
// object. TOLERANT: additionally exactly one ```json fence spanning the
// whole body. Unwrapping a fence or stripping a BOM is deterministic
// repair, allowed only under DETERMINISTIC_ONLY. Prose is never searched
// for something that merely looks like JSON (WBS 13.36).
func Extract(raw []byte, s Settings) ([]byte, ResultParseProvenance, *ParseError) {
	prov := ResultParseProvenance{Mode: s.ParseMode, RepairMode: s.RepairMode, SourceHash: sha(raw), ParserVersion: ParserVersion, Extraction: ExtractRaw}
	fail := func(code WorkerResultErrorCode, vcode, msg string) ([]byte, ResultParseProvenance, *ParseError) {
		return nil, prov, newParseError(code, []ValidationError{{Field: "$", Code: vcode, Message: msg}}, prov)
	}
	if s.Limits.MaxResponseBytes > 0 && len(raw) > s.Limits.MaxResponseBytes {
		return fail(ResultMalformed, "RESPONSE_TOO_LARGE", "response exceeds the protocol size limit")
	}
	if !utf8.Valid(raw) {
		return fail(ResultMalformed, "INVALID_ENCODING", "response is not valid UTF-8")
	}
	body := raw
	var methods []string
	if bytes.HasPrefix(body, bom) {
		if s.RepairMode != RepairDeterministicOnly {
			return fail(ResultMalformed, "BYTE_ORDER_MARK", "response starts with a byte-order mark")
		}
		body = body[len(bom):]
		methods = append(methods, RepairMethodStripBOM)
	}
	trimmed := bytes.TrimSpace(body)
	switch {
	case len(trimmed) == 0:
		return fail(ResultMissing, "EMPTY_RESPONSE", "response is empty")
	case trimmed[0] == '{':
		// raw JSON object; trailing content is judged by the syntax check
	case bytes.HasPrefix(trimmed, []byte("```")):
		inner, code, msg := unfence(string(trimmed))
		if code != "" {
			wc := ResultAmbiguous
			if code != "MULTIPLE_FENCES" {
				wc = ResultMissing
			}
			return fail(wc, code, msg)
		}
		if s.ParseMode != ModeTolerant || s.RepairMode != RepairDeterministicOnly {
			return fail(ResultMalformed, "WRAPPER_NOT_ALLOWED", "fenced response requires TOLERANT parsing with deterministic repair")
		}
		trimmed = []byte(inner)
		prov.Extraction = ExtractFenced
		methods = append(methods, RepairMethodUnwrapFence)
	default:
		return fail(ResultMissing, "NO_PROTOCOL_OBJECT", "response is not a protocol object")
	}
	if len(methods) > 0 {
		m := strings.Join(methods, "+")
		prov.Repaired, prov.RepairMethod = true, &m
	}
	prov.ParsedHash = sha(trimmed)
	return trimmed, prov, nil
}

// unfence accepts exactly one fence spanning the body: an opening line
// "```" or "```json", content, and a closing "```" line.
func unfence(s string) (string, string, string) {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	fences := 0
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			fences++
		}
	}
	if fences > 2 {
		return "", "MULTIPLE_FENCES", "more than one fenced block"
	}
	open := strings.ToLower(strings.TrimSpace(lines[0]))
	if len(lines) < 3 || (open != "```" && open != "```json") || strings.TrimSpace(lines[len(lines)-1]) != "```" {
		return "", "NO_PROTOCOL_OBJECT", "fence does not span the whole response"
	}
	inner := strings.TrimSpace(strings.Join(lines[1:len(lines)-1], "\n"))
	if !strings.HasPrefix(inner, "{") {
		return "", "NO_PROTOCOL_OBJECT", "fenced block is not a protocol object"
	}
	return inner, "", ""
}
