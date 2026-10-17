package resilience

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"testing"
)

type fakePG struct{ code string }

func (f *fakePG) Error() string    { return "pg " + f.code }
func (f *fakePG) SQLState() string { return f.code }

var errSentinel = errors.New("subsystem sentinel")

// T18.188 — representative subsystem errors are classified correctly.
func TestT18188Classification(t *testing.T) {
	RegisterSentinel(errSentinel, Ambiguous)
	cases := []struct {
		name string
		err  error
		want ErrorClass
	}{
		{"plain", fmt.Errorf("boom"), Recoverable},
		{"marked ambiguous wrapped", fmt.Errorf("outer: %w", Mark(errors.New("x"), Ambiguous)), Ambiguous},
		{"serialization", fmt.Errorf("tx: %w", &fakePG{"40001"}), Transient},
		{"deadlock", &fakePG{"40P01"}, Transient},
		{"connection", &fakePG{"08006"}, Transient},
		{"unique", &fakePG{"23505"}, Permanent},
		{"disk full pg", &fakePG{"53100"}, OperatorRequired},
		{"enospc", fmt.Errorf("write: %w", &os.PathError{Op: "write", Path: "/x", Err: syscall.ENOSPC}), OperatorRequired},
		{"deadline", fmt.Errorf("call: %w", context.DeadlineExceeded), Transient},
		{"canceled", context.Canceled, Permanent},
		{"sentinel", fmt.Errorf("a: %w", fmt.Errorf("b: %w", errSentinel)), Ambiguous},
	}
	for _, c := range cases {
		if got := ClassOf(c.err); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
	if ClassOf(nil) != "" {
		t.Fatal("nil classified")
	}
}

// T18.190 — only TRANSIENT is retryable; an ambiguous mark wins over a
// transient cause.
func TestT18190AmbiguousNeverRetryable(t *testing.T) {
	for _, c := range Classes() {
		if Retryable(c) != (c == Transient) {
			t.Fatalf("%s retryable=%v", c, Retryable(c))
		}
	}
	err := Mark(fmt.Errorf("commit: %w", &fakePG{"08006"}), Ambiguous)
	if ClassOf(err) != Ambiguous || Retryable(ClassOf(err)) || !IsAmbiguous(err) {
		t.Fatal("ambiguous commit retryable")
	}
}

func TestMarkPreservesIs(t *testing.T) {
	err := Mark(fmt.Errorf("x: %w", errSentinel), Permanent)
	if !errors.Is(err, errSentinel) || Mark(nil, Transient) != nil {
		t.Fatal("mark breaks errors.Is")
	}
	var pg *fakePG
	if !errors.As(Mark(&fakePG{"40001"}, Ambiguous), &pg) {
		t.Fatal("mark breaks errors.As")
	}
}

func TestRegisterConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		e := fmt.Errorf("s%d", i)
		go func() { defer wg.Done(); RegisterSentinel(e, Permanent) }()
		go func() { defer wg.Done(); _ = ClassOf(e) }()
	}
	wg.Wait()
}

// T18.54 — a full disk is recognized however it is reported.
func TestT1854IsDiskFull(t *testing.T) {
	for _, err := range []error{
		fmt.Errorf("write: %w", syscall.ENOSPC),
		&fakePG{code: "53100"},
		errors.New("git commit: exit status 128: fatal: unable to write new index file: No space left on device"),
	} {
		if !IsDiskFull(err) {
			t.Errorf("not disk full: %v", err)
		}
	}
	for _, err := range []error{nil, errors.New("permission denied"), &fakePG{code: "40001"}, fmt.Errorf("%w", syscall.EACCES)} {
		if IsDiskFull(err) {
			t.Errorf("disk full: %v", err)
		}
	}
}
