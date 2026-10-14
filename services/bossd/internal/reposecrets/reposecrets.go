// Package reposecrets keeps a repo's API keys (Linear, Sentry) in the shared
// "bossanova" OS keyring instead of plaintext SQLite columns (BOS-1587).
//
// The package is deliberately keyring-free: Store talks to a minimal Ring, and
// the real OS keyring adapter lives in the oskeyring subpackage, which only the
// daemon binary links. Package db depends on this package for the Secrets type,
// so keeping github.com/99designs/keyring (and its dbus / keychain transitive
// dependencies) out of it keeps every db-importing test binary free of them —
// the same split proofenv / proofenvkeyring makes.
//
// Secret VALUES never reach a log line or an error string from this package.
package reposecrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"

	"github.com/rs/zerolog/log"

	"github.com/recurser/bossalib/safego"
)

// itemPrefix is the fixed prefix of every repo-secrets keyring item key.
const itemPrefix = "repo-secrets:"

// ErrUnavailable reports that the keyring could not be opened at all (for
// example a headless Linux box with no secret service). Callers fall back to
// their previous storage on this error and only on this error.
var ErrUnavailable = errors.New("repo secret keyring unavailable")

// ErrNotFound is what a Ring returns for a key it does not hold.
var ErrNotFound = errors.New("repo secret item not found")

// Secrets is the per-repo secret record stored as one JSON keyring item.
// sentry_org is not a secret and stays in SQLite.
type Secrets struct {
	LinearAPIKey string `json:"linear_api_key,omitempty"`
	SentryAPIKey string `json:"sentry_api_key,omitempty"`
}

// IsEmpty reports whether neither key is set.
func (s Secrets) IsEmpty() bool {
	return s.LinearAPIKey == "" && s.SentryAPIKey == ""
}

// Ring is the minimal keyring surface Store needs. Get returns ErrNotFound for
// an absent key; Remove of an absent key returns nil or ErrNotFound.
type Ring interface {
	Get(key string) ([]byte, error)
	Set(key string, data []byte) error
	Remove(key string) error
}

// ItemKey is the keyring item key a repo's secrets are stored under.
func ItemKey(repoID string) string {
	return itemPrefix + repoID
}

// Store reads and writes repo secrets through a Ring, caching reads per repo so
// listing repos does not hit the keychain each time. The cache is invalidated
// (replaced) on Set and Delete. A Store is safe for concurrent use.
//
// Ring calls take no context and a keychain call can block indefinitely (an
// approval prompt, a wedged secret service), so every Ring call is an op on
// its item's FIFO queue. One worker goroutine per busy item runs the item's
// ops in the order they were queued; the caller waits for its op or for ctx,
// whichever comes first:
//   - an op on one repo's item never blocks another repo's operations, only
//     later operations on the same item;
//   - Get, Set and Delete return ctx.Err() as soon as ctx ends, even while the
//     item's Ring call is still blocked. An abandoned op stays queued as data,
//     not as a waiting goroutine, so callers giving up on a stuck item cost no
//     goroutines;
//   - ops on one item run strictly in queue order. The worker skips an op whose
//     ctx has ended by the time it reaches it, so an abandoned Set never lands
//     after a later Set or Delete of the same item. An op that had already
//     started when its ctx ended may still complete, so a caller that saw
//     ctx.Err() from Set or Delete must treat the item's state as unknown, and
//     can queue its compensation with Restore, which lands after that op and
//     before anything queued later;
//   - a Get serves the cache only while its item has no op queued or running,
//     and then serves it even after its ctx has ended, since it makes no Ring call;
//     otherwise it queues behind them and sees their outcome. Only the worker
//     fills the cache, together with the Ring call that justifies the value;
//     Forget may drop an entry at any time, which only sends the next Get to
//     the Ring.
//
// Opening the Ring is the one step shared by every item: a Ring open that
// hangs holds up the first op of every item until it returns.
type Store struct {
	open func() (Ring, error)

	// openMu serializes opening the Ring; ring is set once under it.
	openMu sync.Mutex
	ring   Ring

	// mu guards cache and queues and is never held across a Ring call.
	mu     sync.Mutex
	cache  map[string]Secrets
	queues map[string]*itemQueue
}

// itemQueue is one busy item's ops. It exists in Store.queues exactly while
// the item's worker runs: from the first op queued on an idle item until the
// worker finds the queue empty.
type itemQueue struct {
	waiting  []*op // in queue order; the running op is no longer here
	inFlight bool
}

// op is one queued Ring call. The worker sets err and then closes done.
type op struct {
	ctx    context.Context
	repoID string
	run    func(Ring) error
	// detached ops (Restore) have no caller waiting, so the worker logs
	// their failure.
	detached bool
	err      error
	done     chan struct{}
}

// New returns a Store that opens its Ring lazily through open. A failed open is
// retried on the next call, so a keyring that becomes available later (a
// secret service that started after bossd) is picked up without a restart.
func New(open func() (Ring, error)) *Store {
	return &Store{open: open, cache: make(map[string]Secrets), queues: make(map[string]*itemQueue)}
}

// errNoKeyring is why a NewUnavailable store's keyring never opens.
var errNoKeyring = errors.New("no repo secret keyring configured")

// NewUnavailable returns a Store whose keyring never opens: every Get, Set and
// Delete fails with ErrUnavailable, so callers take their no-keyring fallback.
func NewUnavailable() *Store {
	return New(func() (Ring, error) { return nil, errNoKeyring })
}

// openRing returns the opened Ring, opening it on first use.
func (s *Store) openRing() (Ring, error) {
	s.openMu.Lock()
	defer s.openMu.Unlock()
	if s.ring != nil {
		return s.ring, nil
	}
	ring, err := s.open()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if ring == nil {
		return nil, ErrUnavailable
	}
	s.ring = ring
	return ring, nil
}

// cached returns repoID's cached secrets, if any.
func (s *Store) cached(repoID string) (Secrets, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sec, ok := s.cache[repoID]
	return sec, ok
}

// idleCached returns repoID's cached secrets when the item has no op queued or
// running; a busy item's cache may be about to change.
func (s *Store) idleCached(repoID string) (Secrets, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, busy := s.queues[repoID]; busy {
		return Secrets{}, false
	}
	sec, ok := s.cache[repoID]
	return sec, ok
}

// setCache records (ok) or drops (!ok) repoID's cached secrets. Only the
// item's worker calls it.
func (s *Store) setCache(repoID string, sec Secrets, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ok {
		s.cache[repoID] = sec
	} else {
		delete(s.cache, repoID)
	}
}

// Pending reports how many ops are queued or running on repoID's item,
// including ops whose callers have already given up. It is for tests and
// diagnostics.
func (s *Store) Pending(repoID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	q, ok := s.queues[repoID]
	if !ok {
		return 0
	}
	n := len(q.waiting)
	if q.inFlight {
		n++
	}
	return n
}

// enqueue appends o to its item's queue, starting the item's worker when the
// item was idle.
func (s *Store) enqueue(o *op) {
	s.mu.Lock()
	q, busy := s.queues[o.repoID]
	if !busy {
		q = &itemQueue{}
		s.queues[o.repoID] = q
	}
	q.waiting = append(q.waiting, o)
	s.mu.Unlock()
	if !busy {
		// The worker owns its own lifetime: it exits when the queue drains,
		// and every op it takes is completed (done closed) even on a panic.
		_ = safego.Go(log.Logger, func() { s.drain(o.repoID, q) })
	}
}

// drain runs q's ops in order until the queue is empty, then retires q.
func (s *Store) drain(repoID string, q *itemQueue) {
	for {
		s.mu.Lock()
		q.inFlight = false
		if len(q.waiting) == 0 {
			delete(s.queues, repoID)
			s.mu.Unlock()
			return
		}
		o := q.waiting[0]
		q.waiting[0] = nil
		q.waiting = q.waiting[1:]
		q.inFlight = true
		s.mu.Unlock()
		s.runOp(o)
	}
}

// runOp runs one op unless its ctx has already ended, records its error and
// completes it.
func (s *Store) runOp(o *op) {
	defer close(o.done)
	defer func() {
		if r := recover(); r != nil {
			// The panic value can quote Ring input, so only the stack is logged.
			log.Error().Str("repo_id", o.repoID).Str("stack", string(debug.Stack())).
				Msg("repo secrets keyring call panicked")
			o.err = fmt.Errorf("repo secrets keyring call for %s panicked", o.repoID)
		}
	}()
	o.err = s.exec(o)
	switch {
	case o.err == nil || !o.detached:
	case errors.Is(o.err, ErrUnavailable):
		// The keyring never opened in this process, so no write of this run
		// needs putting back. A removal is skipped too, though: a deleted
		// repo whose keys reached the keyring in an earlier daemon run keeps
		// that item, since nothing retries the removal once the row is gone.
		log.Debug().Err(o.err).Str("repo_id", o.repoID).Msg("keyring unavailable; queued repo secrets keyring write skipped")
	default:
		log.Warn().Err(o.err).Str("repo_id", o.repoID).
			Msg("queued repo secrets keyring write failed; keyring may not match the database")
	}
}

func (s *Store) exec(o *op) error {
	if err := o.ctx.Err(); err != nil {
		return err
	}
	ring, err := s.openRing()
	if err != nil {
		return err
	}
	return o.run(ring)
}

// call queues run on repoID's item and waits for it or for ctx, whichever
// comes first.
func (s *Store) call(ctx context.Context, repoID string, run func(Ring) error) error {
	o := &op{ctx: ctx, repoID: repoID, run: run, done: make(chan struct{})}
	s.enqueue(o)
	select {
	case <-o.done:
		return o.err
	case <-ctx.Done():
		select {
		case <-o.done:
			// The op finished as ctx ended; report what it did.
			return o.err
		default:
			return ctx.Err()
		}
	}
}

// Get returns repoID's secrets. A repo with no item returns empty Secrets and
// a nil error.
func (s *Store) Get(ctx context.Context, repoID string) (Secrets, error) {
	// An idle cached item needs no keyring call, so serve it even when ctx
	// has ended: a listing whose shared read budget ran out on a hung item
	// must not blank the repos it can answer from the cache.
	if sec, ok := s.idleCached(repoID); ok {
		return sec, nil
	}
	if err := ctx.Err(); err != nil {
		return Secrets{}, err
	}
	var sec Secrets
	err := s.call(ctx, repoID, func(ring Ring) error {
		// Every op queued before this one has finished, so the cache is
		// current if it holds the item.
		if c, ok := s.cached(repoID); ok {
			sec = c
			return nil
		}
		data, err := ring.Get(ItemKey(repoID))
		if errors.Is(err, ErrNotFound) {
			s.setCache(repoID, Secrets{}, true)
			sec = Secrets{}
			return nil
		}
		if err != nil {
			return fmt.Errorf("read repo secrets for %s: %w", repoID, err)
		}
		var got Secrets
		if err := json.Unmarshal(data, &got); err != nil {
			// The JSON error can quote input bytes, so it is deliberately dropped.
			return fmt.Errorf("decode repo secrets for %s: malformed keyring item", repoID)
		}
		s.setCache(repoID, got, true)
		sec = got
		return nil
	})
	if err != nil {
		return Secrets{}, err
	}
	return sec, nil
}

// writeOp returns the Ring call that replaces repoID's item with sec, or
// removes it when sec is empty.
func (s *Store) writeOp(repoID string, sec Secrets) (func(Ring) error, error) {
	if sec.IsEmpty() {
		return func(ring Ring) error {
			s.setCache(repoID, Secrets{}, false)
			if err := ring.Remove(ItemKey(repoID)); err != nil && !errors.Is(err, ErrNotFound) {
				return fmt.Errorf("delete repo secrets for %s: %w", repoID, err)
			}
			s.setCache(repoID, Secrets{}, true)
			return nil
		}, nil
	}
	data, err := json.Marshal(sec)
	if err != nil {
		return nil, fmt.Errorf("encode repo secrets for %s: %w", repoID, err)
	}
	return func(ring Ring) error {
		// Whatever happens below, the cached value can no longer be trusted.
		s.setCache(repoID, Secrets{}, false)
		if err := ring.Set(ItemKey(repoID), data); err != nil {
			return fmt.Errorf("write repo secrets for %s: %w", repoID, err)
		}
		s.setCache(repoID, sec, true)
		return nil
	}, nil
}

// Set replaces repoID's secrets. Writing empty Secrets is equivalent to Delete.
func (s *Store) Set(ctx context.Context, repoID string, sec Secrets) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	run, err := s.writeOp(repoID, sec)
	if err != nil {
		return err
	}
	return s.call(ctx, repoID, run)
}

// Delete removes repoID's secrets. Deleting a repo with no item is not an error.
func (s *Store) Delete(ctx context.Context, repoID string) error {
	return s.Set(ctx, repoID, Secrets{})
}

// Restore queues a write of sec (a removal when sec is empty) behind every op
// already queued on repoID's item and returns without waiting for it. It is
// the compensation for a Set or Delete whose outcome the caller no longer
// trusts: the write lands after that op — even one abandoned mid-call — and
// before any op queued after Restore returns, so it never overwrites a newer
// value. It is never skipped for a caller giving up. The returned channel is
// closed once the write has run; a failure is logged by repo id.
func (s *Store) Restore(repoID string, sec Secrets) <-chan struct{} {
	o := &op{ctx: context.Background(), repoID: repoID, detached: true, done: make(chan struct{})}
	run, err := s.writeOp(repoID, sec)
	if err != nil {
		run = func(Ring) error { return err }
	}
	o.run = run
	s.enqueue(o)
	return o.done
}

// Forget drops repoID's cached value so the next Get reads the keyring again.
// The one-time SQLite move uses it to make its read-back verify the keyring
// rather than the cache Set just populated.
func (s *Store) Forget(repoID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cache, repoID)
}
