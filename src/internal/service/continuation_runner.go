package service

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"

	"overseer/internal/continuation"
	"overseer/internal/persistence"
	"overseer/internal/project"
)

// maxStepsPerClaim bounds the transitions one claim executes before the
// lease is released and the continuation re-queued (no unbounded loops).
const maxStepsPerClaim = 32

// continuationRunner is the managed execution mechanism (WBS 14.61,
// 14.62): a polling loop plus a bounded worker pool, every goroutine
// tracked and drained on shutdown. Each continuation is executed one
// transition at a time under a database lease (WBS 14.92).
type continuationRunner struct {
	svc      *ContinuationService
	interval time.Duration
	workers  int

	mu      sync.Mutex
	running map[uuid.UUID]bool
	kick    chan struct{}
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	sem     chan struct{}
	closed  bool
}

// Start launches the background runner (idempotent). interval <= 0 uses
// one second; workers <= 0 uses four.
func (s *ContinuationService) Start(interval time.Duration, workers int) {
	if s.runner != nil {
		return
	}
	if interval <= 0 {
		interval = time.Second
	}
	if workers <= 0 {
		workers = 4
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &continuationRunner{svc: s, interval: interval, workers: workers, running: map[uuid.UUID]bool{},
		kick: make(chan struct{}, 1), cancel: cancel, sem: make(chan struct{}, workers)}
	s.runner = r
	r.wg.Add(1)
	go r.loop(ctx)
}

// Kick asks the runner to look for work now (no-op without a runner).
func (s *ContinuationService) Kick(uuid.UUID) {
	if r := s.runner; r != nil {
		select {
		case r.kick <- struct{}{}:
		default:
		}
	}
}

// Shutdown stops the runner and waits for in-flight steps (WBS 14.62:
// no orphan continuation goroutines).
func (s *ContinuationService) Shutdown(ctx context.Context) {
	r := s.runner
	if r == nil {
		return
	}
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	r.cancel()
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func (r *continuationRunner) loop(ctx context.Context) {
	defer r.wg.Done()
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		r.scan(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-r.kick:
		}
	}
}

func (r *continuationRunner) scan(ctx context.Context) {
	list, err := r.svc.d.Repo.ListClaimable(ctx, r.svc.d.Now(), 50)
	if err != nil {
		if ctx.Err() == nil {
			r.svc.d.Logger.Warn("continuation scan failed", "error", err.Error())
		}
		return
	}
	for _, c := range list {
		r.mu.Lock()
		if r.closed || r.running[c.ID] {
			r.mu.Unlock()
			continue
		}
		select {
		case r.sem <- struct{}{}:
		default:
			r.mu.Unlock()
			return
		}
		r.running[c.ID] = true
		r.wg.Add(1)
		r.mu.Unlock()
		go func(c continuation.Continuation) {
			defer func() {
				r.mu.Lock()
				delete(r.running, c.ID)
				r.mu.Unlock()
				<-r.sem
				r.wg.Done()
			}()
			defer containPanic(r.svc.d.Logger, "continuation run", "continuation_id", c.ID.String())
			more, _ := r.svc.Run(ctx, c.ProjectID, c.ID)
			if more {
				r.svc.Kick(c.ProjectID)
			}
		}(c)
	}
}

// Run claims one continuation and executes its transitions iteratively
// (never recursively) until it waits, terminates, is paused or the step
// budget is used; the lease is released afterwards. It reports whether
// more work remains immediately. A continuation leased by another
// instance is not touched (WBS 14.92).
func (s *ContinuationService) Run(ctx context.Context, projectID, id uuid.UUID) (bool, error) {
	settings, _, err := s.d.Settings.Get(ctx, projectID)
	if err != nil {
		return false, err
	}
	ok, err := s.d.Repo.Claim(ctx, id, s.d.Owner, s.d.Now(), settings.LeaseDuration)
	if err != nil || !ok {
		return false, err
	}
	defer func() { _ = s.d.Repo.Release(context.WithoutCancel(ctx), id, s.d.Owner) }()
	for i := 0; i < maxStepsPerClaim; i++ {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		more, err := s.Step(ctx, projectID, id)
		if err != nil {
			s.d.Logger.Warn("continuation step failed", "continuation_id", id.String(), "error", err.Error())
			return false, err
		}
		if !more {
			return false, nil
		}
		if ok, err := s.d.Repo.Claim(ctx, id, s.d.Owner, s.d.Now(), settings.LeaseDuration); err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

// ProcessProject synchronously runs every claimable continuation of the
// project until none can advance (explicit trigger; used by tests and
// administrative tooling).
func (s *ContinuationService) ProcessProject(ctx context.Context, projectID uuid.UUID) error {
	for round := 0; round < 64; round++ {
		list, err := s.d.Repo.ListNonterminal(ctx, projectID)
		if err != nil {
			return err
		}
		progressed := false
		for _, c := range list {
			before := c.Version
			if _, err := s.Run(ctx, projectID, c.ID); err != nil && !errors.Is(err, persistence.ErrStaleVersion) {
				return err
			}
			after, err := s.d.Repo.Get(ctx, projectID, c.ID)
			if err != nil {
				return err
			}
			if after.Version != before {
				progressed = true
			}
		}
		// Next continuations created by consumption are picked up by
		// the next round.
		again, err := s.d.Repo.ListNonterminal(ctx, projectID)
		if err != nil {
			return err
		}
		if !progressed && len(again) == len(list) {
			return nil
		}
	}
	return nil
}

// --- restart recovery (P14-57 .. P14-59) --------------------------------------

// Recover is the startup reconciliation (WBS 14.94-14.100): every accepted
// result Phase 14 has not consumed yet is consumed (exactly one next
// continuation per INCOMPLETE result) and the runner is kicked so
// persisted continuations resume from their exact position through the
// restart-safe steps.
func (s *ContinuationService) Recover(ctx context.Context) (int, error) {
	todo, err := s.d.Repo.ListUnconsumed(ctx, 0)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, u := range todo {
		d, err := s.d.Dispatches.Get(ctx, u.ProjectID, u.DispatchID)
		if err != nil {
			continue
		}
		if d.ContinuationID != nil {
			if _, err := s.reconcileDispatch(ctx, *d.ContinuationID, u.ProjectID); err != nil {
				s.d.Logger.Warn("continuation recovery failed", "dispatch_id", u.DispatchID.String(), "error", err.Error())
				continue
			}
			n++
			continue
		}
		res, err := s.d.Results.Result(ctx, u.ProjectID, u.ResultID)
		if err != nil {
			continue
		}
		full, err := s.d.Results.ResultByDispatch(ctx, u.ProjectID, res.Result.Metadata.DispatchID)
		if err != nil {
			continue
		}
		if active, err := s.d.Repo.GetCurrentForWorkUnit(ctx, u.ProjectID, *d.WorkUnitID); err == nil && active != nil {
			continue
		}
		if c, err := s.createFromResult(ctx, d, full); err != nil {
			s.d.Logger.Warn("continuation recovery failed", "result_id", u.ResultID.String(), "error", err.Error())
		} else if c != nil {
			n++
		}
	}
	s.Kick(uuid.Nil)
	return n, nil
}

// --- lifecycle (P14-55) -------------------------------------------------------

// OnProjectStatus is the lifecycle hook (WBS 14.63-14.65): cancellation
// cancels every nonterminal continuation (an in-flight dispatch follows
// Phase 12 semantics; its result is consumed without a next
// continuation); RUNNING (start/resume) kicks the runner; a pause needs no
// write — steps stop before the next external side effect.
func (s *ContinuationService) OnProjectStatus(ctx context.Context, projectID uuid.UUID, status project.ProjectStatus) {
	switch status {
	case project.StatusCANCELLED:
		if _, err := s.CancelProject(ctx, projectID); err != nil {
			s.d.Logger.Warn("continuation cancellation failed", "project_id", projectID.String(), "error", err.Error())
		}
	case project.StatusRUNNING:
		s.Kick(projectID)
	}
}

// CancelProject cancels the project's nonterminal continuations.
func (s *ContinuationService) CancelProject(ctx context.Context, projectID uuid.UUID) (int, error) {
	list, err := s.d.Repo.ListNonterminal(ctx, projectID)
	if err != nil {
		return 0, err
	}
	n := 0
	for i := range list {
		c := &list[i]
		for try := 0; try < 5; try++ {
			_, err := s.advance(ctx, c, continuation.StatusCancelled, "project cancelled", persistence.ContinuationMutation{})
			if err == nil {
				n++
				break
			}
			if !errors.Is(err, persistence.ErrStaleVersion) {
				break
			}
			fresh, gerr := s.d.Repo.Get(ctx, projectID, c.ID)
			if gerr != nil || fresh.Status.IsTerminal() {
				break
			}
			c = fresh
		}
	}
	return n, nil
}
