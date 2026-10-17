package compression

import "strings"

// DiffOp is one diff line operation.
type DiffOp string

const (
	DiffEqual   DiffOp = " "
	DiffRemoved DiffOp = "-"
	DiffAdded   DiffOp = "+"
)

// DiffLine is one line of a canonical-vs-compressed diff.
type DiffLine struct {
	Op   DiffOp `json:"op"`
	Text string `json:"text"`
}

// Diff is a deterministic line diff with summary counts (WBS 9.48).
type Diff struct {
	Lines   []DiffLine `json:"lines"`
	Removed int        `json:"removed"`
	Added   int        `json:"added"`
	Equal   int        `json:"equal"`
}

// LineDiff computes a deterministic longest-common-subsequence line diff
// from a to b (no LLM). Neither input is modified (strings are immutable).
func LineDiff(a, b string) Diff {
	al := splitLines(a)
	bl := splitLines(b)
	n, m := len(al), len(bl)
	// lcs[i][j] = LCS length of al[i:], bl[j:].
	lcs := make([][]int32, n+1)
	for i := range lcs {
		lcs[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if al[i] == bl[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	var d Diff
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case al[i] == bl[j]:
			d.Lines = append(d.Lines, DiffLine{Op: DiffEqual, Text: al[i]})
			d.Equal++
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			d.Lines = append(d.Lines, DiffLine{Op: DiffRemoved, Text: al[i]})
			d.Removed++
			i++
		default:
			d.Lines = append(d.Lines, DiffLine{Op: DiffAdded, Text: bl[j]})
			d.Added++
			j++
		}
	}
	for ; i < n; i++ {
		d.Lines = append(d.Lines, DiffLine{Op: DiffRemoved, Text: al[i]})
		d.Removed++
	}
	for ; j < m; j++ {
		d.Lines = append(d.Lines, DiffLine{Op: DiffAdded, Text: bl[j]})
		d.Added++
	}
	return d
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// ComponentsText joins component contents (canonical order) — the canonical
// side of a compression diff.
func ComponentsText(comps []CompressionComponent) string {
	var b strings.Builder
	for i, c := range comps {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(c.Content)
	}
	return b.String()
}
