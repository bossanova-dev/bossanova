// Package evidence keeps, per session, the verify evidence summary recorded on
// the session's current PR head. The summary comes from the generic
// machine-readable evidence line (lib/bossalib/evidence) inside a PR
// conversation comment, so bossd never depends on a repo-local marker.
//
// Reads are cheap by construction: the display poller reports each session's
// PR on every poll until the PR ends, then reports the head the PR ended on
// once per poll cycle, and the reader reads a PR's comments
// only when a head is due — once when this process first sees the head (a
// fresh head, or any head after a bossd restart), at most once per refresh
// interval while the verify stage holds the head (parked or verifying) or has
// concluded on an open PR's head, once on the observation where verify lets go
// of the head, once on the first observation where verify has concluded on an
// open PR's head, once on the first observation where the PR has ended (merged
// or closed) whatever was read before, and once per refresh interval after a
// failed read until one succeeds. Verify posts its evidence before it
// concludes, and it can claim, conclude, re-run and merge a head between two
// polls, so no poll need see it hold the head: the concluded reads pick up a
// re-run on an open PR within one refresh interval, and the ended read is what
// guarantees the head the session ends on is read after verify finished. Any
// other head that has been read successfully is never read again.
//
// Because an ended PR is still observed every cycle, a failed ended read is
// retried by the same once-per-interval rule as any other failed read. Retain
// receives every session the poller still lists (all non-archived sessions)
// once per cycle. A merged or closed PR keeps the entry read for its head (the
// session timeline reads only this cache, so merging must not erase the
// evidence step) for as long as its session is listed, and only a new head
// replaces it; nothing is dropped by time. The state of a session neither
// listed nor observed since the previous cycle — archived or deleted — is
// dropped.
package evidence

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	libevidence "github.com/recurser/bossalib/evidence"
	"github.com/recurser/bossalib/vcs"
)

// DefaultRefreshInterval bounds how often one head's comments are re-read.
const DefaultRefreshInterval = 5 * time.Minute

// DefaultReadTimeout bounds one evidence comment read. Observe runs inline in
// the display poller's serial loop, so an unbounded read (a stalled gh
// subprocess, a long paginated comment list) would stall every session's
// display status, not just this one's.
const DefaultReadTimeout = 30 * time.Second

// CommentFinder is the PR comment read the reader needs; vcs.Provider
// satisfies it.
type CommentFinder interface {
	FindPRCommentsWithMarker(ctx context.Context, repoPath string, prID int, marker string) ([]vcs.PRComment, error)
}

// Observation is one poll's view of a session's PR.
type Observation struct {
	SessionID string
	RepoPath  string
	PRNumber  int
	HeadSHA   string
	// IsPROpen is false for a merged or closed PR: verify is over for
	// HeadSHA, which is read once more and its entry kept.
	IsPROpen bool
	// IsVerifyActive is true while the verify stage holds the head: parked
	// for a human or verifying.
	IsVerifyActive bool
	// IsVerifyConcluded is true when verify has finished on HeadSHA (a
	// completed boss/verify status) and does not hold it. Verify posts its
	// evidence before it concludes, so this marks a head whose evidence is
	// final even when no poll saw verify hold it.
	IsVerifyConcluded bool
}

// Entry is the evidence summary recorded on a session's current head.
type Entry struct {
	Summary libevidence.Summary
	// RecordedAt is when the evidence comment was last written.
	RecordedAt time.Time
}

// Option configures a Reader.
type Option func(*Reader)

// WithClock injects the reader's clock (tests).
func WithClock(now func() time.Time) Option {
	return func(r *Reader) {
		if now != nil {
			r.now = now
		}
	}
}

// WithRefreshInterval overrides DefaultRefreshInterval; a non-positive value
// is ignored.
func WithRefreshInterval(d time.Duration) Option {
	return func(r *Reader) {
		if d > 0 {
			r.interval = d
		}
	}
}

// WithReadTimeout overrides DefaultReadTimeout; a non-positive value is
// ignored.
func WithReadTimeout(d time.Duration) Option {
	return func(r *Reader) {
		if d > 0 {
			r.readTimeout = d
		}
	}
}

// WithOnChange notifies consumers after the current summary changes or a new
// head invalidates it. The callback runs synchronously outside the reader lock,
// so it may read the cache and publish a newly hydrated session.
func WithOnChange(onChange func(context.Context, string)) Option {
	return func(r *Reader) { r.onChange = onChange }
}

// SetOnChange wires a consumer after its dependencies have been constructed.
func (r *Reader) SetOnChange(onChange func(context.Context, string)) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.onChange = onChange
	r.mu.Unlock()
}

// headState is what the reader knows about one session's current head.
type headState struct {
	head      string
	entry     *Entry
	fetchedAt time.Time
	// failed is true when the last read of this head could not reach the
	// comments, so it is retried after the refresh interval.
	failed bool
	// verifyActive is the previous observation's IsVerifyActive, so the
	// observation where verify lets go of the head can be read once.
	verifyActive bool
	// concludedRead is true once a read was scheduled on an observation
	// where verify had concluded on the open PR's head. Verify claiming the
	// head again needs no reset: the observation where it lets go reads
	// anyway.
	concludedRead bool
	// endedRead is true once a read was scheduled on an observation of the
	// ended PR, kept apart from concludedRead because verify can re-run and
	// merge between two polls after a concluded read. An open observation
	// (a reopened PR) resets it.
	endedRead bool
	// seq numbers the reads scheduled for this head. Only the newest
	// scheduled read stores its result, so an older read that finishes last
	// cannot overwrite a newer one.
	seq uint64
	// cycle is the Reader's cycle count when the session was last observed,
	// so Retain keeps a session observed after the cycle listed sessions.
	cycle uint64
}

// due reports whether the head's comments should be read on an observation
// at now where verify holds the head (active), has finished with it on the
// open PR (concluded), or the PR has ended.
func (st *headState) due(now time.Time, interval time.Duration, active, concluded, ended bool) bool {
	if st.fetchedAt.IsZero() {
		return true
	}
	elapsed := now.Sub(st.fetchedAt) >= interval
	switch {
	case (active || concluded) && elapsed:
		return true
	case st.verifyActive && !active:
		return true
	case concluded && !st.concludedRead:
		return true
	case ended && !st.endedRead:
		return true
	case st.failed && elapsed:
		return true
	}
	return false
}

// Reader caches each session's current-head evidence summary. The zero value
// is not usable; construct with NewReader. A nil *Reader is a no-op.
type Reader struct {
	finder   CommentFinder
	logger   zerolog.Logger
	now      func() time.Time
	interval time.Duration
	// readTimeout bounds each comment read.
	readTimeout time.Duration
	onChange    func(context.Context, string)

	mu       sync.Mutex
	sessions map[string]*headState
	// cycle counts Retain calls.
	cycle uint64
}

// NewReader returns a Reader that reads comments through finder. A nil finder
// yields a reader that never reads and never has a summary.
func NewReader(finder CommentFinder, logger zerolog.Logger, opts ...Option) *Reader {
	r := &Reader{
		finder:      finder,
		logger:      logger,
		now:         time.Now,
		interval:    DefaultRefreshInterval,
		readTimeout: DefaultReadTimeout,
		sessions:    make(map[string]*headState),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Observe records one poll of a session's PR and, when the head is due (see
// the package doc), re-reads the evidence comment. A summary whose head is not
// obs.HeadSHA is ignored; a new head drops the old head's summary at once. A
// failed read is logged and waits out the interval before it is retried, so a
// broken read cannot turn into a read per poll. An observation with no head
// changes nothing but counts as observing the session for Retain.
func (r *Reader) Observe(ctx context.Context, obs Observation) {
	if r == nil || obs.SessionID == "" {
		return
	}
	head := strings.TrimSpace(obs.HeadSHA)
	// Verify cannot hold an ended PR's head, and while it holds a head it has
	// not concluded there.
	ended := !obs.IsPROpen
	active := !ended && obs.IsVerifyActive
	concluded := !ended && !active && obs.IsVerifyConcluded

	r.mu.Lock()
	now := r.now()
	st := r.sessions[obs.SessionID]
	if head == "" || r.finder == nil {
		if st != nil {
			st.cycle = r.cycle
		}
		r.mu.Unlock()
		return
	}
	invalidated := st != nil && !sameHead(st.head, head) && st.entry != nil
	if st == nil || !sameHead(st.head, head) {
		st = &headState{head: head}
		r.sessions[obs.SessionID] = st
	}
	st.cycle = r.cycle
	due := st.due(now, r.interval, active, concluded, ended)
	st.verifyActive = active
	if !ended {
		st.endedRead = false
	}
	var seq uint64
	if due {
		seq = st.schedule(now)
		if concluded {
			st.concludedRead = true
		}
		if ended {
			st.endedRead = true
		}
	}
	onChange := r.onChange
	r.mu.Unlock()
	if invalidated && onChange != nil {
		onChange(ctx, obs.SessionID)
	}
	if !due {
		return
	}

	entry, err := r.read(ctx, obs, head)
	r.store(ctx, obs.SessionID, st, seq, entry, err)
}

// schedule marks a read of st's head as started at now and returns its
// sequence number. r.mu must be held.
func (st *headState) schedule(now time.Time) uint64 {
	st.fetchedAt = now
	st.seq++
	return st.seq
}

// store records the result of the read numbered seq for st. It stores only
// from the newest read scheduled for the state the read was for: a concurrent
// Observe for a newer head has replaced the state and must not get this
// entry, and the scheduled poll and a webhook refresh can each have a read
// of one head in flight, where an older read finishing last would
// overwrite the newer result, for good on a head that is not read again. A
// failed read says nothing about the comments, so it keeps the head's
// known-good entry; a successful read that finds no usable comment clears it.
func (r *Reader) store(ctx context.Context, sessionID string, st *headState, seq uint64, entry *Entry, err error) {
	r.mu.Lock()
	cur := r.sessions[sessionID]
	if cur != st || cur.seq != seq {
		r.mu.Unlock()
		return
	}
	cur.failed = err != nil
	changed := false
	if err == nil {
		changed = (cur.entry == nil) != (entry == nil)
		if cur.entry != nil && entry != nil {
			changed = cur.entry.Summary != entry.Summary || !cur.entry.RecordedAt.Equal(entry.RecordedAt)
		}
		cur.entry = entry
	}
	onChange := r.onChange
	r.mu.Unlock()
	if changed && onChange != nil {
		onChange(ctx, sessionID)
	}
}

// Retain drops the state of every session that is not in live and has not
// been observed since the previous Retain. The display poller calls it once
// per poll cycle with every session it listed — every non-archived session,
// whatever its PR state — so an archived or deleted session's state goes at
// the next cycle, while a merged session keeps its entry for as long as the
// session exists. Sparing a session observed
// since the previous cycle keeps one created after the cycle listed sessions
// (and observed by a webhook refresh) until a cycle can list it. A read in
// flight for a dropped state finds it gone and stores nothing.
func (r *Reader) Retain(live map[string]struct{}) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, st := range r.sessions {
		if _, ok := live[id]; !ok && st.cycle != r.cycle {
			delete(r.sessions, id)
		}
	}
	r.cycle++
}

// read fetches the marked comments and returns the newest usable one's
// summary, or nil when none is usable for head. A non-nil error means the
// comments could not be read at all, which is not evidence that none is usable. A comment is usable only when
// a trusted author wrote it (vcs.IsTrustedReviewAuthor: anyone can comment on
// a public PR and copy the marker) and its evidence line parses for head; an
// unusable newer comment never hides an older usable one.
func (r *Reader) read(ctx context.Context, obs Observation, head string) (*Entry, error) {
	ctx, cancel := context.WithTimeout(ctx, r.readTimeout)
	defer cancel()
	comments, err := r.finder.FindPRCommentsWithMarker(ctx, obs.RepoPath, obs.PRNumber, libevidence.Marker)
	if err != nil {
		r.logger.Warn().Err(err).
			Str("session_id", obs.SessionID).
			Int("pr_number", obs.PRNumber).
			Msg("evidence reader: read PR comments failed; retrying after the refresh interval")
		return nil, err
	}
	for _, c := range comments {
		if !vcs.IsTrustedReviewAuthor(c.AuthorLogin, c.AuthorAssociation) {
			continue
		}
		summary, ok := libevidence.Parse(c.Body)
		if !ok || !sameHead(summary.Head, head) {
			continue
		}
		return &Entry{Summary: summary, RecordedAt: c.WrittenAt}, nil
	}
	return nil, nil
}

// Get returns the evidence summary recorded on the session's current head.
func (r *Reader) Get(sessionID string) (libevidence.Summary, bool) {
	entry, ok := r.Entry(sessionID)
	return entry.Summary, ok
}

// Entry returns the summary and its recording time for the session's current
// head.
func (r *Reader) Entry(sessionID string) (Entry, bool) {
	if r == nil {
		return Entry{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.sessions[sessionID]
	if st == nil || st.entry == nil {
		return Entry{}, false
	}
	return *st.entry, true
}

// sameHead compares two commit SHAs case-insensitively.
func sameHead(a, b string) bool {
	return a != "" && strings.EqualFold(a, b)
}
