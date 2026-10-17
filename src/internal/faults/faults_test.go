package faults

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// T18.141 — faults can be enabled deterministically; disabled is a no-op.
func TestT18141Deterministic(t *testing.T) {
	ctx := context.Background()
	if Hit(ctx, BeforeWorkerSend) != nil || Enabled() {
		t.Fatal("disabled hit failed")
	}
	custom := errors.New("boom")
	r := NewRegistry().
		Arm(Action{Point: BeforeWorkerSend, Nth: 2, Times: 1, Kind: KindError, Err: custom}).
		Arm(Action{Point: AfterGitCommit, Kind: KindCrash, Times: 1})
	restore := Enable(r)
	defer restore()
	if Hit(ctx, BeforeWorkerSend) != nil {
		t.Fatal("fired on first hit")
	}
	if err := Hit(ctx, BeforeWorkerSend); !errors.Is(err, custom) {
		t.Fatalf("second hit: %v", err)
	}
	if Hit(ctx, BeforeWorkerSend) != nil {
		t.Fatal("fired after Times exhausted")
	}
	if err := Hit(ctx, AfterGitCommit); !IsCrash(err) {
		t.Fatalf("crash: %v", err)
	}
	if Hit(ctx, AfterGitCommit) != nil || r.Hits(BeforeWorkerSend) != 3 {
		t.Fatal("hit accounting")
	}
	func() {
		defer func() {
			if v := recover(); v == nil || !errors.Is(v.(error), ErrInjectedPanic) {
				t.Fatalf("panic %v", v)
			}
		}()
		r.Arm(Action{Point: PhaseAdvance, Kind: KindPanic})
		_ = Hit(ctx, PhaseAdvance)
	}()
	r.Arm(Action{Point: FSWrite, Kind: KindDelay, Delay: 5 * time.Millisecond})
	start := time.Now()
	if Hit(ctx, FSWrite) != nil || time.Since(start) < 5*time.Millisecond {
		t.Fatal("delay")
	}
	exited := 0
	r.SetExit(func(int) { exited++ })
	r.Arm(Action{Point: ProjectCompletion, Kind: KindExit})
	if !IsCrash(Hit(ctx, ProjectCompletion)) || exited != 1 {
		t.Fatal("exit")
	}
	restore()
	if Enabled() || Hit(ctx, AfterGitCommit) != nil {
		t.Fatal("restore")
	}
}

func TestRandomReproducible(t *testing.T) {
	a := Random(42, Points(), 5, 10, KindCrash)
	b := Random(42, Points(), 5, 10, KindCrash)
	if !reflect.DeepEqual(a, b) || len(a) != 5 || reflect.DeepEqual(a, Random(43, Points(), 5, 10, KindCrash)) {
		t.Fatal("schedule not reproducible")
	}
}

func TestParse(t *testing.T) {
	as, err := Parse("after_git_commit:exit@1, before_worker_send:errorx3, fs_write:delay=10ms")
	if err != nil {
		t.Fatal(err)
	}
	want := []Action{{Point: AfterGitCommit, Kind: KindExit, Nth: 1, Times: 1}, {Point: BeforeWorkerSend, Kind: KindError, Times: 3},
		{Point: FSWrite, Kind: KindDelay, Times: 1, Delay: 10 * time.Millisecond}}
	if !reflect.DeepEqual(as, want) {
		t.Fatalf("%+v", as)
	}
	for _, bad := range []string{"nope:crash", "fs_write", "fs_write:melt", "fs_write:crash@x"} {
		if _, err := Parse(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

// A context-scoped registry fires only for that context.
func TestScopedRegistry(t *testing.T) {
	r := NewRegistry().Arm(Action{Point: BeforeDBCommit, Kind: KindError})
	ctx, done := WithRegistry(context.Background(), r)
	defer done()
	if err := Hit(context.Background(), BeforeDBCommit); err != nil {
		t.Fatalf("unscoped context hit the scoped registry: %v", err)
	}
	if err := Hit(ctx, BeforeDBCommit); !errors.Is(err, ErrInjected) {
		t.Fatalf("scoped hit: %v", err)
	}
	if r.Hits(BeforeDBCommit) != 1 {
		t.Fatalf("hits %d", r.Hits(BeforeDBCommit))
	}
}
