package sessionports

import "time"

// clock is the tracker's view of time, injected so tests drive TTL, backoff, and
// staleness deterministically instead of sleeping.
type clock interface {
	Now() time.Time
}

// realClock is the production clock backed by the wall clock.
type realClock struct{}

// Now returns the current wall-clock time.
func (realClock) Now() time.Time { return time.Now() }
