package session

import "sync"

// flightGroup collapses concurrent calls for the same key into one execution,
// so a thundering herd of cold requests fires a single Create/snapshot
// request. This is a minimal, dependency-free singleflight.
type flightGroup struct {
	mu sync.Mutex
	m  map[string]*flightCall
}

type flightCall struct {
	wg  sync.WaitGroup
	val any
	err error
}

// Do runs fn for key, deduplicating concurrent callers. Duplicate callers block
// until the in-flight call finishes and share its result; shared reports whether
// this caller piggy-backed on another's execution.
func (g *flightGroup) Do(key string, fn func() (any, error)) (val any, err error, shared bool) {
	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[string]*flightCall)
	}
	if c, ok := g.m[key]; ok {
		g.mu.Unlock()
		c.wg.Wait()
		return c.val, c.err, true
	}
	c := &flightCall{}
	c.wg.Add(1)
	g.m[key] = c
	g.mu.Unlock()

	c.val, c.err = fn()
	c.wg.Done()

	g.mu.Lock()
	delete(g.m, key)
	g.mu.Unlock()
	return c.val, c.err, false
}
