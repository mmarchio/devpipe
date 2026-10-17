package audit

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/access"
)

func chainOf(t *testing.T, key string, n int) []ChainRow {
	t.Helper()
	prev := Genesis(key)
	var rows []ChainRow
	pid := uuid.New()
	for i := 1; i <= n; i++ {
		e := Event{ID: uuid.New(), At: time.Date(2026, 10, 9, 12, 0, i, 123456789, time.FixedZone("x", 3600)), ProjectID: &pid,
			Actor: Actor{ID: "local", Name: "Local", Type: access.PrincipalLocalAdmin}, Action: access.ActionLifecyclePause,
			ResourceKind: "project", Result: ResultSucceeded, HTTPStatus: 200, RequestID: "r", Metadata: map[string]any{"b": 1, "a": []any{"x", 2}}}
		h, err := ChainLink(key, int64(i), prev, e)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, ChainRow{ChainSeq: int64(i), PrevHash: prev, EventHash: h, Event: e})
		prev = h
	}
	return rows
}

// T18.106 / T18.107 — deterministic genesis and hashes.
func TestT18107Deterministic(t *testing.T) {
	if Genesis("system") != Genesis("system") || Genesis("system") == Genesis("project:x") {
		t.Fatal("genesis")
	}
	rows := chainOf(t, "k", 1)
	e := rows[0].Event
	// The same event read back (JSONB numbers, UTC, microseconds) hashes the same.
	back := e
	back.At = e.At.UTC().Truncate(time.Microsecond)
	back.Metadata = map[string]any{"a": []any{"x", float64(2)}, "b": float64(1)}
	h1, _ := ChainLink("k", 1, Genesis("k"), e)
	h2, _ := ChainLink("k", 1, Genesis("k"), back)
	if h1 != h2 || h1 != rows[0].EventHash {
		t.Fatal("hash not stable across storage round-trip")
	}
}

// T18.108 / T18.105 / T18.109.
func TestT18108Verify(t *testing.T) {
	rows := chainOf(t, "k", 5)
	if r := VerifyChain("k", rows); r.Status != ChainValid || r.Events != 5 {
		t.Fatalf("valid: %+v", r)
	}
	if r := VerifyChain("k", nil); r.Status != ChainValid {
		t.Fatal("empty chain")
	}
	tampered := append([]ChainRow(nil), rows...)
	tampered[2].Event.Reason = "edited later"
	if r := VerifyChain("k", tampered); r.Status != ChainInvalid || r.FirstFailing != 3 {
		t.Fatalf("tamper: %+v", r)
	}
	gap := append(append([]ChainRow(nil), rows[:1]...), rows[3:]...)
	if r := VerifyChain("k", gap); r.Status != ChainIncomplete || r.FirstFailing != 2 || len(r.Gaps) != 1 || r.Gaps[0] != (Gap{2, 3}) {
		t.Fatalf("gap: %+v", r)
	}
	gapTamper := append([]ChainRow(nil), gap...)
	gapTamper[2].Event.Path = "/x"
	if r := VerifyChain("k", gapTamper); r.Status != ChainInvalid || r.FirstFailing != 5 || len(r.Gaps) != 1 {
		t.Fatalf("gap + tamper: %+v", r)
	}
	relinked := append([]ChainRow(nil), rows...)
	relinked[1].PrevHash = Genesis("k")
	if r := VerifyChain("k", relinked); r.Status != ChainInvalid || r.FirstFailing != 2 {
		t.Fatalf("link: %+v", r)
	}
	if Overall([]ChainResult{{Status: ChainValid}, {Status: ChainIncomplete}}) != ChainIncomplete ||
		Overall([]ChainResult{{Status: ChainIncomplete}, {Status: ChainInvalid}}) != ChainInvalid {
		t.Fatal("overall")
	}
}
