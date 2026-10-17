package audit

import (
	"strings"
	"testing"
	"time"

	"overseer/internal/access"
)

func validEvent() Event {
	return Event{At: time.Now(), Actor: Actor{ID: "local-admin", Name: "Admin", Type: access.PrincipalLocalAdmin},
		Action: access.ActionLifecyclePause, ResourceKind: "project", Result: ResultSucceeded, RequestID: "r1"}
}

// P17-19 (WBS 17.106) — actor and request metadata are required.
func TestP1719EventValidate(t *testing.T) {
	if err := validEvent().Validate(); err != nil {
		t.Fatal(err)
	}
	muts := map[string]func(*Event){
		"actor":     func(e *Event) { e.Actor = Actor{} },
		"type":      func(e *Event) { e.Actor.Type = "ROOT" },
		"read":      func(e *Event) { e.Action = access.ActionRead },
		"result":    func(e *Event) { e.Result = "MAYBE" },
		"requestID": func(e *Event) { e.RequestID = "" },
		"kind":      func(e *Event) { e.ResourceKind = "" },
		"reason":    func(e *Event) { e.Reason = strings.Repeat("x", MaxReason+1) },
		"at":        func(e *Event) { e.At = time.Time{} },
	}
	for name, m := range muts {
		e := validEvent()
		m(&e)
		if e.Validate() == nil {
			t.Fatalf("%s: invalid event accepted", name)
		}
	}
	if ResultForStatus(204) != ResultSucceeded || ResultForStatus(303) != ResultSucceeded || ResultForStatus(409) != ResultRejected || ResultForStatus(500) != ResultFailed {
		t.Fatal("ResultForStatus")
	}
}

// P17-20 (WBS 17.104) — filter validation and cursor round trip.
func TestP1720Filter(t *testing.T) {
	f := Filter{}
	if err := f.Validate(); err != nil || f.Limit != 50 {
		t.Fatalf("%v %d", err, f.Limit)
	}
	now := time.Now()
	earlier := now.Add(-time.Hour)
	for _, bad := range []Filter{{Limit: 201}, {Limit: -1}, {Action: access.ActionRead}, {Action: "NOPE"}, {Result: "X"}, {From: &now, To: &earlier}, {BeforeSeq: -1}} {
		if bad.Validate() == nil {
			t.Fatalf("invalid filter accepted: %+v", bad)
		}
	}
	for _, n := range []int64{1, 42, 1 << 40} {
		got, err := DecodeCursor(EncodeCursor(n))
		if err != nil || got != n {
			t.Fatalf("cursor %d: %d %v", n, got, err)
		}
	}
	if EncodeCursor(0) != "" {
		t.Fatal("empty cursor")
	}
	for _, bad := range []string{"%%%", "eDox", EncodeCursor(5) + "x"} {
		if _, err := DecodeCursor(bad); err == nil {
			t.Fatalf("bad cursor %q accepted", bad)
		}
	}
}
