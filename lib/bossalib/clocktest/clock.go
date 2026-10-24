// Package clocktest provides a manually advanced clock for deterministic tests.
package clocktest

import (
	"context"
	"sync"
	"time"
)

// Clock is safe for concurrent registration and inspection. Advance and Set must
// be driven by one test goroutine. Callbacks run synchronously, without the lock,
// at their deadlines; they may register or stop timers on the same clock.
type Clock struct {
	mu      sync.Mutex
	now     time.Time
	timers  []*timer
	sleeps  []time.Duration
	waits   []time.Duration
	changed chan struct{}
}

type timer struct {
	clock    *Clock
	deadline time.Time
	fn       func()
	done     bool
}

func New(start time.Time) *Clock { return &Clock{now: start, changed: make(chan struct{})} }
func (c *Clock) Now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

// Set sets the time, firing due timers when moving forward.
func (c *Clock) Set(at time.Time) { c.Advance(at.Sub(c.Now())) }

// Advance fires timers in deadline order, including timers registered by a
// callback before the target time. Equal deadlines preserve registration order.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	for {
		var next *timer
		for _, t := range c.timers {
			if !t.done && !t.deadline.After(target) && (next == nil || t.deadline.Before(next.deadline)) {
				next = t
			}
		}
		if next == nil {
			break
		}
		next.done = true
		if next.deadline.After(c.now) {
			c.now = next.deadline
		}
		c.notifyLocked()
		c.mu.Unlock()
		next.fn()
		c.mu.Lock()
	}
	c.now = target
	c.notifyLocked()
	c.mu.Unlock()
}

func (c *Clock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.AfterFunc(d, func() { ch <- c.Now() })
	return ch
}

func (c *Clock) AfterFunc(d time.Duration, fn func()) interface{ Stop() bool } {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &timer{clock: c, deadline: c.now.Add(d), fn: fn}
	c.timers = append(c.timers, t)
	c.waits = append(c.waits, d)
	c.notifyLocked()
	return t
}
func (t *timer) Stop() bool {
	c := t.clock
	c.mu.Lock()
	defer c.mu.Unlock()
	if t.done {
		return false
	}
	t.done = true
	c.notifyLocked()
	return true
}

// Sleep advances immediately and records successful sleeps.
func (c *Clock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.sleeps = append(c.sleeps, d)
	c.mu.Unlock()
	c.Advance(d)
	return nil
}
func (c *Clock) Sleeps() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.sleeps...)
}

// Waits returns the durations requested by all registered timers.
func (c *Clock) Waits() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.waits...)
}

// PendingDurations returns the remaining duration of every active timer.
func (c *Clock) PendingDurations() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	var ds []time.Duration
	for _, t := range c.timers {
		if !t.done {
			ds = append(ds, t.deadline.Sub(c.now))
		}
	}
	return ds
}
func (c *Clock) Pending() int { return len(c.PendingDurations()) }

// BlockUntil waits for at least n active timers, bounded by ctx.
func (c *Clock) BlockUntil(ctx context.Context, n int) error {
	return c.WaitUntil(ctx, func() bool { return c.Pending() >= n })
}

// WaitUntil waits for a clock-state predicate, checking it after every clock
// change. The predicate may call inspection methods on c.
func (c *Clock) WaitUntil(ctx context.Context, ready func() bool) error {
	for {
		c.mu.Lock()
		changed := c.changed
		c.mu.Unlock()
		if ready() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}
func (c *Clock) notifyLocked() { close(c.changed); c.changed = make(chan struct{}) }
