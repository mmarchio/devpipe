package workertest

import (
	"testing"
	"time"

	"overseer/internal/worker"
)

// T12.69.2: the mock worker passes the contract where capabilities apply.
func TestP1225MockPassesContract(t *testing.T) {
	var cur *MockWorker
	RunContract(t, Target{
		New: func(t *testing.T) worker.Worker {
			cur = NewMockWorker(FullCapabilities())
			return cur
		},
		DelayNext: func(d time.Duration) { cur.DelayNext(d) },
		Dir:       "/repos/project-a",
	})
}

// A worker without session or cwd capabilities is still a valid worker
// (T12.97.2): capability-gated checks skip.
func TestP1225MinimalMockPassesContract(t *testing.T) {
	RunContract(t, Target{
		New: func(t *testing.T) worker.Worker { return NewMockWorker(worker.WorkerCapabilities{}) },
		Dir: "/repos/project-a",
	})
}
