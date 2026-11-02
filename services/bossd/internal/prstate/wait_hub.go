package prstate

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossd/internal/ghusage"
)

// unhealthyRecheckInterval is how often a pending wait re-reads while its
// repo's webhook delivery is unhealthy, or after one of its reads failed.
// These tick reads bypass the GitHub read cache: for an unhealthy repo the
// cache serves a snapshot up to snapshotFallbackMaxAge (five minutes) old
// (services/bossd/internal/adapter/github/readcache.go), so a cached tick
// would re-serve the same stale snapshot.
const unhealthyRecheckInterval = 60 * time.Second

// ErrWaitHubClosed is returned by a wait the daemon's shutdown ended, or one
// started after it.
var ErrWaitHubClosed = errors.New("daemon shutting down")

// SnapshotReader reads one PR's state (*Reader).
type SnapshotReader interface {
	Read(ctx context.Context, repo string, pr int, opts Options) (Snapshot, error)
}

// WebhookHealth reports whether webhooks currently reach this daemon for a
// repo (*upstream.WebhookHealth).
type WebhookHealth interface {
	WebhookDeliveryHealthy(repo string) bool
}

// WaitRequest is one wait. Repo is the registered owner/name the reads use.
type WaitRequest struct {
	Repo      string
	PR        int
	Condition WaitCondition
	// SinceHeadSHA is the head-changed baseline; empty uses the first read's
	// head.
	SinceHeadSHA string
	Timeout      time.Duration
	Fresh        bool
	// AcceptNoGateRan lets checks-settled meet on a green head where no gate
	// ran.
	AcceptNoGateRan bool
}

// WaitResult is how a wait ended and the last good snapshot it read.
type WaitResult struct {
	Outcome  WaitOutcome
	Snapshot Snapshot
}

type waitKey struct {
	repo string
	pr   int
}

// waiter is one pending wait. wake holds at most one pending wake, so wakes
// that land during a re-read collapse into one more re-read.
type waiter struct {
	key  waitKey
	wake chan struct{}
}

// WaitHub blocks PR waits until a webhook invalidation names their PR or repo.
// The webhook dispatcher notifies it; each wait re-reads on its own RPC
// handler goroutine, so the hub owns no goroutine. All methods are nil-safe.
type WaitHub struct {
	reader SnapshotReader

	mu      sync.Mutex
	waiters map[waitKey]map[*waiter]struct{}
	health  WebhookHealth
	closed  chan struct{}
	once    sync.Once
}

// NewWaitHub returns a hub whose waits read through reader.
func NewWaitHub(reader SnapshotReader) *WaitHub {
	return &WaitHub{reader: reader, waiters: map[waitKey]map[*waiter]struct{}{}, closed: make(chan struct{})}
}

// repoKey is the lowercase GitHub owner/name of an origin URL or owner/name,
// the same normalisation the webhook health tracker keys deliveries by.
func repoKey(repo string) string {
	if nwo := vcs.GitHubNWO(repo); nwo != "" {
		return strings.ToLower(nwo)
	}
	return strings.ToLower(strings.TrimSpace(repo))
}

// SetWebhookHealth wires the delivery-health tracker the fallback tick
// consults. nil counts as unhealthy, so waits fall back to the slow timer.
func (h *WaitHub) SetWebhookHealth(health WebhookHealth) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.health = health
	h.mu.Unlock()
}

// NotifyPR wakes every wait on repo#pr. It never blocks and never reads.
func (h *WaitHub) NotifyPR(repo string, pr int) {
	h.notify(func(k waitKey) bool { return k == waitKey{repo: repoKey(repo), pr: pr} })
}

// NotifyRepo wakes every wait in repo. It never blocks and never reads.
func (h *WaitHub) NotifyRepo(repo string) {
	key := repoKey(repo)
	h.notify(func(k waitKey) bool { return k.repo == key })
}

func (h *WaitHub) notify(match func(waitKey) bool) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for k, set := range h.waiters {
		if !match(k) {
			continue
		}
		for w := range set {
			select {
			case w.wake <- struct{}{}:
			default:
			}
		}
	}
}

// Close ends every pending wait with ErrWaitHubClosed; later waits fail at
// once.
func (h *WaitHub) Close() {
	if h == nil {
		return
	}
	h.once.Do(func() { close(h.closed) })
}

func (h *WaitHub) register(repo string, pr int) (*waiter, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	select {
	case <-h.closed:
		return nil, ErrWaitHubClosed
	default:
	}
	w := &waiter{key: waitKey{repo: repoKey(repo), pr: pr}, wake: make(chan struct{}, 1)}
	if h.waiters[w.key] == nil {
		h.waiters[w.key] = map[*waiter]struct{}{}
	}
	h.waiters[w.key][w] = struct{}{}
	return w, nil
}

func (h *WaitHub) unregister(w *waiter) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.waiters[w.key], w)
	if len(h.waiters[w.key]) == 0 {
		delete(h.waiters, w.key)
	}
}

func (h *WaitHub) healthy(repo string) bool {
	h.mu.Lock()
	health := h.health
	h.mu.Unlock()
	return health != nil && health.WebhookDeliveryHealthy(repo)
}

// Wait blocks until req's condition is met or unreachable, the timeout
// passes, ctx ends or the hub closes. The waiter registers before its first
// read, so a webhook that lands during that read still wakes it. A failed
// first read is returned; a failed re-read or confirmation keeps the last good
// snapshot and is retried on the next recheck tick even while webhooks are
// healthy, so no event is needed to recover from it. A checks-settled answer
// from a cached read costs one more, fresh read (see resolve).
func (h *WaitHub) Wait(ctx context.Context, req WaitRequest) (WaitResult, error) {
	if h == nil {
		return WaitResult{}, ErrWaitHubClosed
	}
	w, err := h.register(req.Repo, req.PR)
	if err != nil {
		return WaitResult{}, err
	}
	defer h.unregister(w)
	snap, err := h.reader.Read(ctx, req.Repo, req.PR, Options{Fresh: req.Fresh, Label: ghusage.LabelPRWait})
	if err != nil {
		return WaitResult{}, err
	}
	since := req.SinceHeadSHA
	if since == "" {
		since = snap.Status.HeadSHA
	}
	out, snap, err := h.resolve(ctx, req, snap, since, req.Fresh)
	if out != waitPending {
		return WaitResult{Outcome: out, Snapshot: snap}, nil
	}
	deadline := time.NewTimer(req.Timeout)
	defer deadline.Stop()
	recheck := time.NewTicker(unhealthyRecheckInterval)
	defer recheck.Stop()
	retry := err != nil
	for {
		fresh := req.Fresh
		select {
		case <-ctx.Done():
			return WaitResult{}, ctx.Err()
		case <-h.closed:
			return WaitResult{}, ErrWaitHubClosed
		case <-deadline.C:
			return WaitResult{Outcome: WaitTimedOut, Snapshot: snap}, nil
		case <-w.wake:
		case <-recheck.C:
			if !retry && h.healthy(req.Repo) {
				continue
			}
			fresh = true
		}
		next, err := h.reader.Read(ctx, req.Repo, req.PR, Options{Fresh: fresh, Label: ghusage.LabelPRWait})
		if retry = err != nil; retry {
			continue
		}
		out, snap, err = h.resolve(ctx, req, next, since, fresh)
		retry = err != nil
		if out != waitPending {
			return WaitResult{Outcome: out, Snapshot: snap}, nil
		}
	}
}

// resolve evaluates snap and returns the outcome with the snapshot it rests
// on. A cached read's status and checks are separate cache entries and the
// checks carry no head SHA, so a checks-settled answer from a cached read is
// not provably about the status head: resolve confirms it on one fresh read
// and decides on that. snapFresh reports that snap already came from a fresh
// read. A failed confirmation keeps waiting on snap and returns the error.
func (h *WaitHub) resolve(ctx context.Context, req WaitRequest, snap Snapshot, since string, snapFresh bool) (WaitOutcome, Snapshot, error) {
	out := evaluateWait(req, snap, since)
	if out != WaitMet || req.Condition != WaitChecksSettled || snapFresh {
		return out, snap, nil
	}
	fresh, err := h.reader.Read(ctx, req.Repo, req.PR, Options{Fresh: true, Label: ghusage.LabelPRWait})
	if err != nil {
		return waitPending, snap, err
	}
	return evaluateWait(req, fresh, since), fresh, nil
}
