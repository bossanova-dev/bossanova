// Package keyedgate provides a keyed set of bounded, context-aware capacity-1
// serialization gates.
//
// It exists because BOS-717 needed the same primitive in two packages that
// cannot import each other: the daemon's session start/target locks
// (services/bossd/internal/session) and the per-repo gate that serializes
// mutating git against one shared clone (services/bossd/internal/git, which
// internal/session imports). Duplicating the acquire logic — whose value is
// entirely in two easily-missed select races and a ref-counted map lifecycle —
// is exactly the kind of copy that drifts, so it lives here instead.
//
// A sync.Mutex cannot be acquired with a deadline, which is the whole point:
// the failure this primitive exists to prevent is an untimed wait on a wedged
// holder. Every acquire is therefore bounded and reports how long it waited, on
// both the success and the failure path, so a caller can log contention before
// it becomes a hang.
package keyedgate

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// gate is a capacity-1 serialization gate. sem is a buffered channel used as a
// context-aware mutex; refs counts live acquirers so the registry entry can be
// reaped when the last one releases.
type gate struct {
	sem  chan struct{}
	refs int
}

// Registry is a keyed set of gates with a ref-counted lifecycle: an entry is
// created on first acquire and deleted when its last acquirer releases, so the
// map cannot grow without bound over a process's lifetime.
//
// The zero value is usable; Name only labels the errors this registry returns.
// A Registry must not be copied after first use.
type Registry struct {
	// Name labels acquire errors ("acquire <name> lock ...") so a log line
	// identifies which registry timed out.
	Name string

	mu    sync.Mutex
	gates map[string]*gate
}

// Acquire takes the gate for key, waiting at most timeout (and no longer than
// ctx allows). On success it returns a release func the caller must call
// exactly once — though calling it more than once is harmless: release is
// idempotent, because the alternative failure mode is a permanent block on an
// empty channel, which is the precise hazard this package exists to remove.
//
// waited is returned on both the success and the failure path so callers can
// log contention either way.
func (r *Registry) Acquire(ctx context.Context, key string, timeout time.Duration) (release func(), waited time.Duration, err error) {
	// Honor an already-cancelled context before contending: a caller that has
	// given up must never take the gate. Without this, the select below can pick
	// the (ready) sem-send over a (ready) ctx.Done.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, 0, fmt.Errorf("acquire %s lock %q: %w", r.Name, key, ctxErr)
	}

	r.mu.Lock()
	if r.gates == nil {
		r.gates = map[string]*gate{}
	}
	g := r.gates[key]
	if g == nil {
		g = &gate{sem: make(chan struct{}, 1)}
		r.gates[key] = g
	}
	g.refs++
	r.mu.Unlock()

	// Decrement the ref (reaping the entry at zero) — used both when acquisition
	// is abandoned and inside the successful-acquisition release.
	unref := func() {
		r.mu.Lock()
		g.refs--
		if g.refs == 0 {
			delete(r.gates, key)
		}
		r.mu.Unlock()
	}

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	started := time.Now()
	select {
	case g.sem <- struct{}{}:
		// select picks a ready case at random, so the sem-send can win even when
		// waitCtx is already done (the holder released at the instant the deadline
		// fired). Re-check so an abandoned caller releases the gate and bails.
		if ctxErr := waitCtx.Err(); ctxErr != nil {
			<-g.sem
			unref()
			return nil, time.Since(started), fmt.Errorf("acquire %s lock %q: %w", r.Name, key, ctxErr)
		}
		// sync.Once, not a bare closure: a second call would receive on an empty
		// capacity-1 channel and block forever, and would drive refs negative so
		// the entry is never reaped and a later acquirer is handed a SECOND live
		// gate for the same key — silently losing mutual exclusion. Callers here
		// use the defer-plus-flag pattern where an accidental double release is
		// one edit away, so make it structurally impossible instead.
		return sync.OnceFunc(func() {
			<-g.sem
			unref()
		}), time.Since(started), nil
	case <-waitCtx.Done():
		unref()
		return nil, time.Since(started), fmt.Errorf("acquire %s lock %q: %w", r.Name, key, waitCtx.Err())
	}
}
