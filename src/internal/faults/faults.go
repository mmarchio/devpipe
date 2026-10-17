// Package faults provides deterministic, test-only failure injection points
// around the overseer's critical boundaries (Phase 18, WBS 18.141). Points
// are compiled in but cost one atomic load when disabled; tests enable
// them with Enable, a real test-profile process with OVERSEER_FAULTS. The
// production profile refuses fault configuration.
package faults

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Point names one injection boundary.
type Point string

// Injection points (WBS 18.141).
const (
	BeforeDBCommit      Point = "before_db_commit"
	AfterDBCommit       Point = "after_db_commit"
	BeforeWorkerSend    Point = "before_worker_send"
	AfterWorkerSend     Point = "after_worker_send"
	AfterWorkerResponse Point = "after_worker_response"
	BeforeValidation    Point = "before_validation"
	AfterValidation     Point = "after_validation"
	BeforeGitCommit     Point = "before_git_commit"
	AfterGitCommit      Point = "after_git_commit"
	BeforeTransition    Point = "before_transition"
	AfterTransition     Point = "after_transition"
	PhaseAdvance        Point = "phase_advance"
	ProjectCompletion   Point = "project_completion"
	FSWrite             Point = "fs_write"
	// FSRename sits between a complete, fsynced temporary file and the
	// atomic rename over the target (WBS 18.55).
	FSRename  Point = "fs_rename"
	GitLock   Point = "git_lock"
	DBConnect Point = "db_connect"
)

// Points lists every known point.
func Points() []Point {
	return []Point{BeforeDBCommit, AfterDBCommit, BeforeWorkerSend, AfterWorkerSend, AfterWorkerResponse, BeforeValidation,
		AfterValidation, BeforeGitCommit, AfterGitCommit, BeforeTransition, AfterTransition, PhaseAdvance, ProjectCompletion,
		FSWrite, FSRename, GitLock, DBConnect}
}

// Kind is what an armed point does.
type Kind string

const (
	// KindError returns the configured error.
	KindError Kind = "error"
	// KindCrash returns ErrCrash: the caller abandons the operation as if
	// the process died (in-process crash simulation).
	KindCrash Kind = "crash"
	// KindDelay sleeps (context-aware) and continues.
	KindDelay Kind = "delay"
	// KindPanic panics with ErrInjectedPanic.
	KindPanic Kind = "panic"
	// KindExit terminates the process immediately (real-process forced
	// termination tests only).
	KindExit Kind = "exit"
)

// ErrCrash marks a simulated process crash.
var ErrCrash = errors.New("faults: simulated crash")

// ErrInjected is the default injected error.
var ErrInjected = errors.New("faults: injected failure")

// ErrInjectedPanic is the value of injected panics.
var ErrInjectedPanic = errors.New("faults: injected panic")

// Action is one armed behavior: it fires on the Nth hit of its point
// (1-based; 0 = every hit) at most Times times (0 = unlimited).
type Action struct {
	Point Point
	Nth   int
	Times int
	Kind  Kind
	Err   error
	Delay time.Duration
}

type armed struct {
	Action
	hits  int
	fired int
}

// Registry is a set of armed actions plus hit counters.
type Registry struct {
	mu      sync.Mutex
	actions []*armed
	hits    map[Point]int
	exit    func(code int)
}

var (
	enabled atomic.Bool
	global  atomic.Pointer[Registry]
)

// NewRegistry builds an empty registry.
func NewRegistry() *Registry { return &Registry{hits: map[Point]int{}} }

// Arm adds an action.
func (r *Registry) Arm(a Action) *Registry {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.actions = append(r.actions, &armed{Action: a})
	return r
}

// Hits returns how often point was reached.
func (r *Registry) Hits(p Point) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hits[p]
}

// Enable installs r globally (nil disables). It returns a restore func.
func Enable(r *Registry) func() {
	prev := global.Load()
	prevOn := enabled.Load()
	global.Store(r)
	enabled.Store(r != nil)
	return func() {
		global.Store(prev)
		enabled.Store(prevOn)
	}
}

// Enabled reports whether a registry is installed.
func Enabled() bool { return enabled.Load() }

type scopeKey struct{}

// scoped counts live context-scoped registries; while it is zero (and no
// global registry is enabled) Hit costs one atomic load.
var scoped atomic.Int64

// WithRegistry scopes r to ctx: Hit on ctx (and contexts derived from it)
// uses r instead of the global registry, so a test can inject faults into
// one operation without touching concurrent background work. Call the
// returned func when done.
func WithRegistry(ctx context.Context, r *Registry) (context.Context, func()) {
	scoped.Add(1)
	var once sync.Once
	return context.WithValue(ctx, scopeKey{}, r), func() { once.Do(func() { scoped.Add(-1) }) }
}

// Hit records a pass through point and performs the first matching armed
// action. It is a no-op returning nil when faults are disabled.
func Hit(ctx context.Context, p Point) error {
	if scoped.Load() > 0 && ctx != nil {
		if r, _ := ctx.Value(scopeKey{}).(*Registry); r != nil {
			return r.hit(ctx, p)
		}
	}
	if !enabled.Load() {
		return nil
	}
	r := global.Load()
	if r == nil {
		return nil
	}
	return r.hit(ctx, p)
}

func (r *Registry) hit(ctx context.Context, p Point) error {
	r.mu.Lock()
	r.hits[p]++
	n := r.hits[p]
	var act *Action
	for _, a := range r.actions {
		if a.Point != p {
			continue
		}
		a.hits++
		if a.Times > 0 && a.fired >= a.Times {
			continue
		}
		if a.Nth > 0 && a.hits != a.Nth {
			continue
		}
		a.fired++
		cp := a.Action
		act = &cp
		break
	}
	exit := r.exit
	r.mu.Unlock()
	_ = n
	if act == nil {
		return nil
	}
	switch act.Kind {
	case KindError:
		if act.Err != nil {
			return act.Err
		}
		return fmt.Errorf("%w at %s", ErrInjected, p)
	case KindCrash:
		return fmt.Errorf("%w at %s", ErrCrash, p)
	case KindDelay:
		t := time.NewTimer(act.Delay)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
		return nil
	case KindPanic:
		panic(fmt.Errorf("%w at %s", ErrInjectedPanic, p))
	case KindExit:
		if exit != nil {
			exit(137)
		}
		return fmt.Errorf("%w at %s", ErrCrash, p)
	}
	return nil
}

// SetExit installs the process-exit function for KindExit (os.Exit in a
// real process).
func (r *Registry) SetExit(f func(code int)) { r.mu.Lock(); r.exit = f; r.mu.Unlock() }

// Random arms a seeded pseudo-random schedule: n actions of kind spread
// over points with hit numbers in [1, maxNth]. The same seed yields the
// same schedule (WBS 18.151).
func Random(seed int64, points []Point, n, maxNth int, kind Kind) []Action {
	rng := rand.New(rand.NewSource(seed))
	ps := append([]Point(nil), points...)
	sort.Slice(ps, func(i, j int) bool { return ps[i] < ps[j] })
	out := make([]Action, 0, n)
	for i := 0; i < n && len(ps) > 0; i++ {
		out = append(out, Action{Point: ps[rng.Intn(len(ps))], Nth: 1 + rng.Intn(maxNth), Times: 1, Kind: kind})
	}
	return out
}

var specRE = regexp.MustCompile(`^([a-z]+)(?:@(\d+))?(?:x(\d+))?(?:=(.+))?$`)

// Parse reads a spec "point:kind[@nth][xTimes][=delay]" separated by
// commas, e.g. "after_git_commit:exit@1,before_worker_send:errorx3".
// Times defaults to 1.
func Parse(spec string) ([]Action, error) {
	var out []Action
	known := map[Point]bool{}
	for _, p := range Points() {
		known[p] = true
	}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		pt, rest, ok := strings.Cut(part, ":")
		m := specRE.FindStringSubmatch(rest)
		if !ok || !known[Point(pt)] || m == nil {
			return nil, fmt.Errorf("faults: invalid spec %q", part)
		}
		a := Action{Point: Point(pt), Times: 1}
		switch Kind(m[1]) {
		case KindError, KindCrash, KindDelay, KindPanic, KindExit:
			a.Kind = Kind(m[1])
		default:
			return nil, fmt.Errorf("faults: unknown kind in %q", part)
		}
		if m[2] != "" {
			a.Nth, _ = strconv.Atoi(m[2])
		}
		if m[3] != "" {
			a.Times, _ = strconv.Atoi(m[3])
		}
		if m[4] != "" {
			d, err := time.ParseDuration(m[4])
			if err != nil {
				return nil, fmt.Errorf("faults: invalid delay in %q", part)
			}
			a.Delay = d
		}
		out = append(out, a)
	}
	return out, nil
}

// IsCrash reports whether err is a simulated crash.
func IsCrash(err error) bool { return errors.Is(err, ErrCrash) }
