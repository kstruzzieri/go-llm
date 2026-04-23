package compat

import "github.com/kstruzzieri/go-llm/provider"

// reserveFor returns the general-pool size and the high-priority reserve size
// for a requested max concurrency. The total is always equal to the input
// (clamped to >= 1) so the global cap stays honest.
//
//	n <= 1:  (1, 0)      minimum viable concurrency, no reserve
//	2 <= n <= 3: (n, 0)  small caps do not reserve (reserve would starve general)
//	n >= 4:  (n-1, 1)    one slot reserved for PriorityHigh/PriorityCritical
func reserveFor(n int) (general, reserved int) {
	if n < 1 {
		n = 1
	}
	switch {
	case n == 1:
		return 1, 0
	case n < 4:
		return n, 0
	default:
		return n - 1, 1
	}
}

// semaphore bounds concurrent request execution. It keeps a general pool for
// all requests and an optional reserve pool only usable by PriorityHigh and
// PriorityCritical. High-priority requests try the general pool first and
// only tap the reserve when the general pool is full.
type semaphore struct {
	general chan struct{}
	reserve chan struct{}
}

func newSemaphore(max int) *semaphore {
	gen, res := reserveFor(max)
	return &semaphore{
		general: make(chan struct{}, gen),
		reserve: make(chan struct{}, res),
	}
}

// acquire reserves a slot for the given priority. Returns a release func
// and true on success; if the pool is full it returns nil, false immediately
// without blocking. Callers convert false into 429 Too Many Requests.
func (s *semaphore) acquire(p provider.Priority) (release func(), ok bool) {
	select {
	case s.general <- struct{}{}:
		return func() { <-s.general }, true
	default:
	}
	if p >= provider.PriorityHigh && cap(s.reserve) > 0 {
		select {
		case s.reserve <- struct{}{}:
			return func() { <-s.reserve }, true
		default:
		}
	}
	return nil, false
}
