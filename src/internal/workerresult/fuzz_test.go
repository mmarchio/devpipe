package workerresult_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	wr "overseer/internal/workerresult"
)

// P13-20 (WBS 13.74): arbitrary content never panics and never yields an
// invalid accepted WorkerResult.
func FuzzP1320Parse(f *testing.F) {
	files, _ := filepath.Glob("testdata/*")
	for _, p := range files {
		if b, err := os.ReadFile(p); err == nil {
			f.Add(string(b))
		}
	}
	f.Add("")
	f.Add("{")
	f.Add(`{"a":[[[[[[[[[[[[[[[[[[[[[[1]]]]]]]]]]]]]]]]]]]]]}`)
	modes := []wr.Settings{wr.DefaultSettings(), settings(wr.ModeTolerant, wr.RepairDeterministicOnly), settings(wr.ModeTolerant, wr.RepairNone)}
	f.Fuzz(func(t *testing.T, content string) {
		d := dispatch()
		for _, s := range modes {
			r, err := wr.DeterministicParser{Settings: s}.Parse(context.Background(), resp(d, content), d)
			if err != nil {
				if _, ok := wr.AsParseError(err); !ok {
					t.Fatalf("untyped error %v", err)
				}
				continue
			}
			if !wr.Revalidate(r) || r.Metadata.DispatchID != d.ID {
				t.Fatalf("invalid accepted result for %q", content)
			}
			if len(r.Completed) > s.Limits.MaxCompleted || len(r.Summary) > s.Limits.MaxSummaryBytes {
				t.Fatal("limits exceeded")
			}
		}
	})
}
