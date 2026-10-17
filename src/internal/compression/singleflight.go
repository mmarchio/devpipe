package compression

import "sync"

// SingleFlight collapses concurrent identical requests (same key = same
// compression request identity) into one execution in this process (WBS
// 9.42): every caller receives the same result or failure; distinct keys
// run independently.
type SingleFlight struct {
	mu    sync.Mutex
	calls map[string]*flightCall
}

type flightCall struct {
	wg  sync.WaitGroup
	val any
	err error
}

// Do runs fn once per in-flight key. shared reports whether the result was
// produced by another caller's execution.
func (g *SingleFlight) Do(key string, fn func() (any, error)) (val any, err error, shared bool) {
	g.mu.Lock()
	if g.calls == nil {
		g.calls = map[string]*flightCall{}
	}
	if c, ok := g.calls[key]; ok {
		g.mu.Unlock()
		c.wg.Wait()
		return c.val, c.err, true
	}
	c := &flightCall{}
	c.wg.Add(1)
	g.calls[key] = c
	g.mu.Unlock()

	func() {
		defer func() {
			if r := recover(); r != nil {
				c.err = errPanicked
			}
		}()
		c.val, c.err = fn()
	}()
	c.wg.Done()

	g.mu.Lock()
	delete(g.calls, key)
	g.mu.Unlock()
	return c.val, c.err, false
}

var errPanicked = &CompressionError{Kind: ErrorUnknown, Message: "single-flight call panicked"}
