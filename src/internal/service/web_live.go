package service

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	"overseer/internal/persistence"
	"overseer/internal/readmodel"
)

// LiveFeedConfig tunes the live event feed (WBS 17.161).
type LiveFeedConfig struct {
	// Interval is the per-project poll interval (default 500 ms).
	Interval time.Duration
	// Buffer is the per-subscriber buffer (default 256 events).
	Buffer int
	// StallTicks is how many consecutive polls a subscriber may stay
	// behind with a full buffer before it is disconnected (default 3).
	StallTicks int
}

// Subscription close reasons.
const (
	FeedClosedSlowConsumer = "slow_consumer"
	FeedClosedUnsubscribed = "unsubscribed"
	FeedClosedShutdown     = "shutdown"
)

// Subscription is one live stream of a project's events.
type Subscription struct {
	// C delivers events in project_seq order; it is closed when the
	// subscription ends (see Reason).
	C <-chan readmodel.LiveEvent

	feed    *LiveEventFeed
	project uuid.UUID
	ch      chan readmodel.LiveEvent
	cursor  int64
	reason  string
	closed  bool
	// stalled counts consecutive ticks with a full buffer while behind.
	stalled int
}

// Reason returns why the subscription ended ("" while open).
func (s *Subscription) Reason() string {
	s.feed.mu.Lock()
	defer s.feed.mu.Unlock()
	return s.reason
}

// Close ends the subscription.
func (s *Subscription) Close() { s.feed.remove(s, FeedClosedUnsubscribed) }

type projectFeed struct {
	subs   map[*Subscription]bool
	last   int64
	cancel context.CancelFunc
	done   chan struct{}
}

// LiveEventFeed fans committed execution events out to SSE subscribers
// (WBS 17.74–17.78, 17.161). It only reads persisted events: one poller per
// subscribed project reads past its last project_seq; each subscriber has a
// bounded buffer. A subscriber whose buffer fills falls behind and is caught
// up from the database; one that stays full for StallTicks polls is
// disconnected (slow_consumer) and resumes later with Last-Event-ID. Event producers (the engine, bootstrap)
// never interact with the feed and can never be blocked by a browser.
type LiveEventFeed struct {
	repo persistence.WebReadRepository
	cfg  LiveFeedConfig

	mu       sync.Mutex
	projects map[uuid.UUID]*projectFeed
	stopped  bool
}

// NewLiveEventFeed builds a feed.
func NewLiveEventFeed(repo persistence.WebReadRepository, cfg LiveFeedConfig) *LiveEventFeed {
	if cfg.Interval <= 0 {
		cfg.Interval = 500 * time.Millisecond
	}
	if cfg.Buffer <= 0 {
		cfg.Buffer = 256
	}
	if cfg.StallTicks <= 0 {
		cfg.StallTicks = 3
	}
	return &LiveEventFeed{repo: repo, cfg: cfg, projects: map[uuid.UUID]*projectFeed{}}
}

// Subscribe starts a subscription delivering events with project_seq >
// afterSeq. Missed committed events are replayed first (WBS 17.76).
func (f *LiveEventFeed) Subscribe(ctx context.Context, projectID uuid.UUID, afterSeq int64) (*Subscription, error) {
	last, err := f.repo.MaxProjectSeq(ctx, projectID)
	if err != nil {
		return nil, translate(err)
	}
	if afterSeq < 0 {
		afterSeq = 0
	}
	ch := make(chan readmodel.LiveEvent, f.cfg.Buffer)
	s := &Subscription{C: ch, feed: f, project: projectID, ch: ch, cursor: afterSeq}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopped {
		s.closed, s.reason = true, FeedClosedShutdown
		close(ch)
		return s, nil
	}
	pf := f.projects[projectID]
	if pf == nil {
		pctx, cancel := context.WithCancel(context.Background())
		pf = &projectFeed{subs: map[*Subscription]bool{}, last: last, cancel: cancel, done: make(chan struct{})}
		f.projects[projectID] = pf
		go f.poll(pctx, projectID, pf)
	}
	pf.subs[s] = true
	return s, nil
}

// Subscribers returns the number of open subscriptions of a project.
func (f *LiveEventFeed) Subscribers(projectID uuid.UUID) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if pf := f.projects[projectID]; pf != nil {
		return len(pf.subs)
	}
	return 0
}

// Pollers returns the number of running project pollers.
func (f *LiveEventFeed) Pollers() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.projects)
}

// Stop ends every subscription and poller (shutdown, before the database
// closes).
func (f *LiveEventFeed) Stop() {
	f.mu.Lock()
	f.stopped = true
	var waits []chan struct{}
	for id, pf := range f.projects {
		for s := range pf.subs {
			f.closeLocked(s, FeedClosedShutdown)
		}
		pf.cancel()
		waits = append(waits, pf.done)
		delete(f.projects, id)
	}
	f.mu.Unlock()
	for _, w := range waits {
		<-w
	}
}

func (f *LiveEventFeed) closeLocked(s *Subscription, reason string) {
	if s.closed {
		return
	}
	s.closed, s.reason = true, reason
	close(s.ch)
}

func (f *LiveEventFeed) remove(s *Subscription, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeLocked(s, reason)
	pf := f.projects[s.project]
	if pf == nil {
		return
	}
	delete(pf.subs, s)
	if len(pf.subs) == 0 {
		pf.cancel()
		delete(f.projects, s.project)
	}
}

// poll is the per-project poller.
func (f *LiveEventFeed) poll(ctx context.Context, projectID uuid.UUID, pf *projectFeed) {
	defer close(pf.done)
	t := time.NewTicker(f.cfg.Interval)
	defer t.Stop()
	f.tick(ctx, projectID, pf)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f.tick(ctx, projectID, pf)
		}
	}
}

// tick catches lagging subscribers up (bounded by their free buffer) and
// broadcasts new events to caught-up ones.
func (f *LiveEventFeed) tick(ctx context.Context, projectID uuid.UUID, pf *projectFeed) {
	f.mu.Lock()
	last := pf.last
	var lagging []*Subscription
	for s := range pf.subs {
		if s.cursor >= last {
			s.stalled = 0
			continue
		}
		if len(s.ch) == cap(s.ch) {
			s.stalled++
			if s.stalled >= f.cfg.StallTicks {
				// Persistently full while behind: disconnect; the client
				// resumes with Last-Event-ID (WBS 17.161).
				f.closeLocked(s, FeedClosedSlowConsumer)
				delete(pf.subs, s)
				continue
			}
		} else {
			s.stalled = 0
		}
		lagging = append(lagging, s)
	}
	f.mu.Unlock()

	for _, s := range lagging {
		f.mu.Lock()
		free := cap(s.ch) - len(s.ch)
		cursor, closed := s.cursor, s.closed
		f.mu.Unlock()
		if closed || free <= 0 {
			continue
		}
		evs, err := f.repo.ListProjectEventsAfter(ctx, projectID, cursor, free)
		if err != nil {
			continue
		}
		f.mu.Lock()
		for _, e := range evs {
			if e.ProjectSeq > last || s.closed {
				break
			}
			s.ch <- readmodel.FromExecutionEvent(e.ProjectSeq, e)
			s.cursor = e.ProjectSeq
		}
		f.mu.Unlock()
	}

	evs, err := f.repo.ListProjectEventsAfter(ctx, projectID, last, 500)
	if err != nil || len(evs) == 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range evs {
		le := readmodel.FromExecutionEvent(e.ProjectSeq, e)
		for s := range pf.subs {
			if s.closed || s.cursor != pf.last {
				continue
			}
			select {
			case s.ch <- le:
				s.cursor = e.ProjectSeq
			default:
				// Full buffer: the subscriber falls behind and is caught
				// up from the database on later ticks (never blocking).
			}
		}
		pf.last = e.ProjectSeq
	}
	if len(pf.subs) == 0 {
		pf.cancel()
		if f.projects[projectID] == pf {
			delete(f.projects, projectID)
		}
	}
}
