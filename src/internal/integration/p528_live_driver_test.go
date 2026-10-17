package integration

// P5-28 live-driver evidence: the deterministic serializer exercised over the
// real harness pool (service + repos), not just pure fixtures. Read-only op,
// so the -race suite covers concurrency; this test proves byte-stability of
// the live read path and sensitivity to a configuration modification.

import (
	"testing"
)

// Live driver — SnapshotRole serializations are byte-for-byte identical for an
// unchanged configuration read twice through the real service.
func TestP528LiveSerializationByteStable(t *testing.T) {
	h := NewHarness(t)
	svc := snapshotSvc(t, h)
	id := seedSnapshotRole(t, h, svc)
	a, err := svc.SnapshotRole(ctx0(), id)
	if err != nil {
		t.Fatal(err)
	}
	b, err := svc.SnapshotRole(ctx0(), id)
	if err != nil {
		t.Fatal(err)
	}
	sa, sb := a.Serialize(), b.Serialize()
	if sa == "" {
		t.Fatal("empty serialization from live snapshot")
	}
	if sa != sb {
		t.Fatalf("live serializations differ:\nA: %s\nB: %s", sa, sb)
	}
}

// Live driver — a configuration modification (system-prompt append) changes
// the serialized representation.
func TestP528LiveConfigChangeChangesSerialization(t *testing.T) {
	h := NewHarness(t)
	svc := snapshotSvc(t, h)
	id := seedSnapshotRole(t, h, svc)
	before, err := svc.SnapshotRole(ctx0(), id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateSystemPrompt(ctx0(), id, "revised system prompt body", before.SystemPrompt.Version); err != nil {
		t.Fatal(err)
	}
	after, err := svc.SnapshotRole(ctx0(), id)
	if err != nil {
		t.Fatal(err)
	}
	if before.Serialize() == after.Serialize() {
		t.Fatal("prompt append did not change the serialized representation")
	}
}
