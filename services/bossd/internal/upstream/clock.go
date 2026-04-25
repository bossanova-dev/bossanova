package upstream

import "time"

// Clock abstracts the time primitives the stream client and coalescer
// rely on. Kept small on purpose: only After and Now, which is enough
// for the backoff loop (After) and the coalescer flush ticker (Now +
// AfterFunc for deterministic test advancement). The shape mirrors the
// bosso-side Clock so engineers switching sides don't have to relearn
// the interface.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
	AfterFunc(d time.Duration, fn func()) Timer
}

// Timer is the subset of *time.Timer the coalescer relies on. Stop
// returns true if the call stopped the timer, false if it has already
// expired or been stopped — matching the standard library contract.
type Timer interface {
	Stop() bool
}

// realClock is the production Clock. All methods delegate to the time
// package so there's no behaviour difference between tests that use
// realClock and production.
type realClock struct{}

func (realClock) Now() time.Time                             { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time     { return time.After(d) }
func (realClock) AfterFunc(d time.Duration, fn func()) Timer { return time.AfterFunc(d, fn) }
