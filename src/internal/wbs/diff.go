package wbs

import "errors"

// ErrMissingContent is returned by DiffContents when a version's content is
// missing (nil pointer) (WBS 4.17, T4.17.5 primitive half). The REST
// version-pair rejection is P4-24's endpoint half.
var ErrMissingContent = errors.New("wbs: missing version content")

// HunkOperation classifies a diff hunk.
type HunkOperation string

const (
	// OpAdded is a hunk whose lines exist only in the new content.
	OpAdded HunkOperation = "added"
	// OpRemoved is a hunk whose lines exist only in the old content.
	OpRemoved HunkOperation = "removed"
	// OpModified is a hunk pairing replaced old-side lines with new-side
	// lines (modified content, WBS 4.17 T4.17.3).
	OpModified HunkOperation = "modified"
)

// DiffHunk is one contiguous region of change. For OpAdded, FromLines is
// empty and ToLines holds the added lines; for OpRemoved, ToLines is empty;
// for OpModified both sides carry the aligned old/new lines.
type DiffHunk struct {
	Operation HunkOperation
	FromLines []string
	ToLines   []string
}

// Diff is the result of DiffContents: a deterministic, line-based textual
// diff between two WBS version contents (WBS 4.17). An identical pair of
// contents yields a zero-hunk Diff. No DB access, no LLM — the same inputs
// always produce byte-identical output.
type Diff struct {
	Hunks []DiffHunk
}

// DiffContents computes a deterministic line-based diff between the old
// (from) and new (to) WBS version contents (WBS 4.17). Each content is a
// pointer so a missing version content is distinguishable from an empty one:
// a nil pointer is rejected with ErrMissingContent (T4.17.5); the empty
// string is valid content. The alignment is the classic longest-common-
// subsequence (LCS) over lines, which is pure and deterministic: the same
// inputs always produce the same hunks. A region where removed and added
// lines are adjacent in the alignment is represented as a single OpModified
// hunk (T4.17.3) rather than separate added/removed hunks.
func DiffContents(from, to *string) (Diff, error) {
	if from == nil || to == nil {
		return Diff{}, ErrMissingContent
	}
	oldLines := splitLines(*from)
	newLines := splitLines(*to)

	lcsLen := make([][]int, len(oldLines)+1)
	for i := range lcsLen {
		lcsLen[i] = make([]int, len(newLines)+1)
	}
	for i := len(oldLines) - 1; i >= 0; i-- {
		for j := len(newLines) - 1; j >= 0; j-- {
			if oldLines[i] == newLines[j] {
				lcsLen[i][j] = lcsLen[i+1][j+1] + 1
			} else if lcsLen[i+1][j] >= lcsLen[i][j+1] {
				lcsLen[i][j] = lcsLen[i+1][j]
			} else {
				lcsLen[i][j] = lcsLen[i][j+1]
			}
		}
	}

	// Walk the LCS table to emit ordered change entries.
	type entry struct {
		kind    HunkOperation
		oldLine string
		newLine string
	}
	var entries []entry
	i, j := 0, 0
	for i < len(oldLines) && j < len(newLines) {
		switch {
		case oldLines[i] == newLines[j]:
			i++
			j++
		case lcsLen[i+1][j] >= lcsLen[i][j+1]:
			entries = append(entries, entry{kind: OpRemoved, oldLine: oldLines[i]})
			i++
		default:
			entries = append(entries, entry{kind: OpAdded, newLine: newLines[j]})
			j++
		}
	}
	for ; i < len(oldLines); i++ {
		entries = append(entries, entry{kind: OpRemoved, oldLine: oldLines[i]})
	}
	for ; j < len(newLines); j++ {
		entries = append(entries, entry{kind: OpAdded, newLine: newLines[j]})
	}

	// Collapse maximal runs of removed+added entries into hunks: a run
	// containing both sides is one OpModified hunk; a run with only removed
	// (or only added) entries is a single OpRemoved/OpAdded hunk.
	var hunks []DiffHunk
	for n := 0; n < len(entries); {
		m := n
		removed, added := false, false
		for m < len(entries) && entries[m].kind != "" {
			switch entries[m].kind {
			case OpRemoved:
				removed = true
			case OpAdded:
				added = true
			}
			m++
		}
		h := DiffHunk{}
		for k := n; k < m; k++ {
			if entries[k].oldLine != "" || entries[k].kind == OpRemoved {
				h.FromLines = append(h.FromLines, entries[k].oldLine)
			}
			if entries[k].newLine != "" || entries[k].kind == OpAdded {
				h.ToLines = append(h.ToLines, entries[k].newLine)
			}
		}
		switch {
		case removed && added:
			h.Operation = OpModified
		case removed:
			h.Operation = OpRemoved
		default:
			h.Operation = OpAdded
		}
		hunks = append(hunks, h)
		n = m
	}
	if hunks == nil {
		hunks = []DiffHunk{}
	}
	return Diff{Hunks: hunks}, nil
}

// splitLines splits content into lines on "\n". A trailing newline does not
// yield a phantom empty final line, so "a\nb\n" and "a\nb" are line-identical.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	var lines []string
	start := 0
	for pos := 0; pos < len(s); pos++ {
		if s[pos] == '\n' {
			lines = append(lines, s[start:pos])
			start = pos + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}
