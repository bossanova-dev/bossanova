package server

// Archive deferral (BOS-1380).
//
// Archiving a session kills every chat's tmux pane and removes the worktree, so
// an archive that lands while one of the session's chats is mid-turn tears the
// worktree out from under a working agent. That happened on two routes: an
// in-session merge triggering archive-after-merge, and a session archiving
// itself. The deferrer sits in front of the immediate executor
// (Server.ArchiveSessionAndNotify): a request for a session with a busy chat is
// recorded as pending and runs once every chat is idle; a request for a session
// whose chats are all idle runs immediately, exactly as before.
//
// Pending archives live in memory only, and a daemon restart drops them. That
// is deliberate: merge-driven archives are re-derived after a restart by the
// merged-but-unarchived sweep and the display poller, which now route through
// this seam too, and an explicit `boss archive` (or a self-archive) lost to a
// restart simply leaves the session unarchived, which loses no data and can be
// re-issued. The pending window is one agent turn.

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"sync"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/safego"
	"github.com/rs/zerolog"

	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/status"
)

// requesterSettle is how long after an archive request the requesting chat is
// still treated as busy unless the tracker has observed it settle. The chat
// that asked to archive its own session is mid-turn by definition, even when
// the tmux poller has not yet marked it WORKING; without this window a poller
// lag lets a self-archive kill its own caller (the BOS-1372 hazard).
const requesterSettle = 10 * time.Second

// archiveDeferralInterval is the fallback re-evaluation tick. Not every
// transition that frees a session fires the chat-status hook (a deleted chat,
// a tracker entry going stale, a requester's settle window elapsing), so the
// loop re-checks pending sessions on this cadence as well as on a nudge.
const archiveDeferralInterval = 5 * time.Second

// errArchiveAborted is what joiners of an in-flight archive see when its
// executor panicked instead of returning.
var errArchiveAborted = errors.New("archive aborted")

// ArchiveRequest carries the options of one archive request.
type ArchiveRequest struct {
	// Force archives now even if a chat is working.
	Force bool
	// RequesterAgentSessionID is the chat that made the request, when the
	// request comes from inside a session. It is treated as busy until the
	// tracker records it settled (see requesterSettle).
	RequesterAgentSessionID string
}

// ArchiveOutcome reports what an archive request did.
type ArchiveOutcome struct {
	// Archived is true when the archive ran synchronously.
	Archived bool
	// Pending is true when the archive is deferred until every chat is idle.
	Pending bool
	// BlockingAgentSessionID names the chat a pending archive is waiting on.
	BlockingAgentSessionID string
}

// archiveChatLister is the slice of db.AgentChatStore the deferrer reads.
type archiveChatLister interface {
	ListBySession(ctx context.Context, sessionID string) ([]*models.AgentChat, error)
}

// archiveSessionGetter is the slice of db.SessionStore the deferrer reads.
type archiveSessionGetter interface {
	Get(ctx context.Context, id string) (*models.Session, error)
}

// archivingFlagger is the slice of status.DisplayTracker the deferrer writes:
// the flag that surfaces as Session.archive_pending ("Archiving…").
type archivingFlagger interface {
	SetArchiving(sessionID string, archiving bool)
}

// pendingArchive is one deferred archive. requesters maps each requesting
// chat's agent session id to when its latest request was accepted. gen changes
// on every write to the entry, so a decision made from a copy of it can be
// checked against the entry as it stands before anything is claimed.
type pendingArchive struct {
	requesters map[string]time.Time
	gen        uint64
}

// inflightArchive is an archive that has been claimed and is executing. A
// request arriving while one is in flight joins it instead of running a second
// archive; err is set before done closes.
type inflightArchive struct {
	done chan struct{}
	err  error
}

// archiveDeferrer holds pending archives and fires each once its session's
// chats are all idle. Every dependency is optional except execute: a nil chat
// lister or tracker means no chat can be observed busy.
type archiveDeferrer struct {
	chats    archiveChatLister
	tracker  *status.Tracker
	display  archivingFlagger
	sessions archiveSessionGetter
	execute  func(ctx context.Context, sessionID string) error
	logger   zerolog.Logger

	now      func() time.Time
	interval time.Duration
	// fireBudget bounds one fired archive, detached from any caller.
	fireBudget time.Duration

	// mu guards pending, inflight and seq. Deciding to fire a session and
	// claiming it (removing its pending entry, adding it to inflight) happen
	// in one critical section, checked against the gen the decision read, so
	// exactly one path archives a session and no accepted requester guard is
	// skipped.
	mu       sync.Mutex
	pending  map[string]*pendingArchive
	inflight map[string]*inflightArchive
	seq      uint64
	nudge    chan struct{}
}

func newArchiveDeferrer(
	chats archiveChatLister,
	tracker *status.Tracker,
	display archivingFlagger,
	sessions archiveSessionGetter,
	execute func(ctx context.Context, sessionID string) error,
	logger zerolog.Logger,
) *archiveDeferrer {
	return &archiveDeferrer{
		chats:      chats,
		tracker:    tracker,
		display:    display,
		sessions:   sessions,
		execute:    execute,
		logger:     logger,
		now:        time.Now,
		interval:   archiveDeferralInterval,
		fireBudget: archiveRPCBudget,
		pending:    map[string]*pendingArchive{},
		inflight:   map[string]*inflightArchive{},
		nudge:      make(chan struct{}, 1),
	}
}

// request archives sessionID now when no chat is busy (or Force is set), and
// otherwise records a pending archive and raises the archiving flag. Repeated
// requests for one session share a single pending entry, and a request that
// arrives while the session's archive is already executing joins it.
func (d *archiveDeferrer) request(ctx context.Context, sessionID string, req ArchiveRequest) (ArchiveOutcome, error) {
	acceptedAt := d.now()
	for {
		if err := ctx.Err(); err != nil {
			return ArchiveOutcome{}, err
		}
		d.mu.Lock()
		if f, ok := d.inflight[sessionID]; ok {
			d.mu.Unlock()
			return d.join(ctx, f)
		}
		if req.Force {
			delete(d.pending, sessionID)
			f := d.claimLocked(sessionID)
			d.mu.Unlock()
			return d.runClaimed(ctx, sessionID, f)
		}
		gen, requesters := d.requestersLocked(sessionID, req.RequesterAgentSessionID, acceptedAt)
		d.mu.Unlock()

		blocking, err := d.blockingChat(ctx, sessionID, requesters)
		if err != nil {
			return ArchiveOutcome{}, err
		}

		d.mu.Lock()
		if _, running := d.inflight[sessionID]; running || d.genLocked(sessionID) != gen {
			// The entry changed while we evaluated (another request added a
			// requester or claimed the archive, the sweep claimed it, a cancel
			// dropped it): decide again against what stands now.
			d.mu.Unlock()
			continue
		}
		if blocking == "" {
			// Nothing is busy, including any earlier requester still pending.
			delete(d.pending, sessionID)
			f := d.claimLocked(sessionID)
			d.mu.Unlock()
			return d.runClaimed(ctx, sessionID, f)
		}
		entry, ok := d.pending[sessionID]
		if !ok {
			entry = &pendingArchive{requesters: map[string]time.Time{}}
			d.pending[sessionID] = entry
		}
		for id, at := range requesters {
			if prev, seen := entry.requesters[id]; !seen || at.After(prev) {
				entry.requesters[id] = at
			}
		}
		d.seq++
		entry.gen = d.seq
		d.mu.Unlock()
		if d.display != nil {
			d.display.SetArchiving(sessionID, true)
		}
		d.logger.Info().
			Str("session", sessionID).
			Str("blocking_agent_session_id", blocking).
			Msg("archive deferred until every chat is idle")
		return ArchiveOutcome{Pending: true, BlockingAgentSessionID: blocking}, nil
	}
}

// requestersLocked returns the pending entry's gen (0 when there is none) and
// a copy of its requesters plus requester, accepted at acceptedAt. A repeat
// request from a chat already recorded takes the newer acceptance time: each
// self-archive request is made mid-turn, so it earns a fresh settle window.
// Callers hold d.mu.
func (d *archiveDeferrer) requestersLocked(sessionID, requester string, acceptedAt time.Time) (uint64, map[string]time.Time) {
	out := map[string]time.Time{}
	var gen uint64
	if entry, ok := d.pending[sessionID]; ok {
		gen = entry.gen
		for id, at := range entry.requesters {
			out[id] = at
		}
	}
	if requester != "" {
		if prev, seen := out[requester]; !seen || acceptedAt.After(prev) {
			out[requester] = acceptedAt
		}
	}
	return gen, out
}

// genLocked returns the pending entry's gen, or 0 when there is none. Callers
// hold d.mu.
func (d *archiveDeferrer) genLocked(sessionID string) uint64 {
	if entry, ok := d.pending[sessionID]; ok {
		return entry.gen
	}
	return 0
}

// claimLocked marks sessionID's archive in flight. Callers hold d.mu and have
// checked that none is already in flight.
func (d *archiveDeferrer) claimLocked(sessionID string) *inflightArchive {
	f := &inflightArchive{done: make(chan struct{})}
	d.inflight[sessionID] = f
	return f
}

// finish records a claimed archive's result and releases the claim.
func (d *archiveDeferrer) finish(sessionID string, f *inflightArchive, err error) {
	d.mu.Lock()
	f.err = err
	if d.inflight[sessionID] == f {
		delete(d.inflight, sessionID)
	}
	d.mu.Unlock()
	close(f.done)
}

// runClaimed executes a claimed archive synchronously on the caller's ctx.
func (d *archiveDeferrer) runClaimed(ctx context.Context, sessionID string, f *inflightArchive) (outcome ArchiveOutcome, err error) {
	// Release the claim even if execute panics, so joiners are not stranded.
	err = errArchiveAborted
	defer func() { d.finish(sessionID, f, err) }()
	err = d.execute(ctx, sessionID)
	if err != nil {
		return ArchiveOutcome{}, err
	}
	return ArchiveOutcome{Archived: true}, nil
}

// join waits for an in-flight archive and reports its result.
func (d *archiveDeferrer) join(ctx context.Context, f *inflightArchive) (ArchiveOutcome, error) {
	select {
	case <-f.done:
	case <-ctx.Done():
		return ArchiveOutcome{}, ctx.Err()
	}
	if f.err != nil {
		return ArchiveOutcome{}, f.err
	}
	return ArchiveOutcome{Archived: true}, nil
}

// blockingChat returns the agent session id of the first busy chat of
// sessionID in sorted order, or "" when none is busy.
func (d *archiveDeferrer) blockingChat(ctx context.Context, sessionID string, requesters map[string]time.Time) (string, error) {
	if d.chats == nil {
		return "", nil
	}
	chats, err := d.chats.ListBySession(ctx, sessionID)
	if err != nil {
		return "", err
	}
	ids := make([]string, 0, len(chats))
	for _, c := range chats {
		if c != nil && c.AgentSessionID != "" {
			ids = append(ids, c.AgentSessionID)
		}
	}
	sort.Strings(ids)
	var entries map[string]*status.Entry
	if d.tracker != nil {
		entries = d.tracker.GetBatch(ids)
	}
	now := d.now()
	for _, id := range ids {
		// A requester that is not one of this session's chats is not killed by
		// the archive, so it gets no guard.
		if acceptedAt, ok := requesters[id]; ok {
			if d.requesterBusy(id, entries[id], acceptedAt, now) {
				return id, nil
			}
			continue
		}
		if d.chatBusy(id, entries[id]) {
			return id, nil
		}
	}
	return "", nil
}

// chatBusy reports whether a chat is mid-turn: its RAW tracker status (before
// PromoteWaiting, so a WAITING chat promoted from WORKING still counts) is
// WORKING or LIMITED and the tracker does not flag it stalled. A stale entry
// already reads STOPPED and a chat with no entry is not busy.
func (d *archiveDeferrer) chatBusy(id string, e *status.Entry) bool {
	if e == nil {
		return false
	}
	if e.Status != pb.ChatStatus_CHAT_STATUS_WORKING && e.Status != pb.ChatStatus_CHAT_STATUS_LIMITED {
		return false
	}
	return d.tracker == nil || !d.tracker.Stalled(id)
}

// requesterBusy reports whether the requesting chat still counts as busy. It
// does until the tracker records a non-WORKING, non-LIMITED status for it
// received at least requesterSettle after the request was accepted. After the
// settle window a chat the tracker has no live reading for (no entry, or a
// STOPPED one) is not observed by anything and does not block; a stalled
// requester does not block either.
func (d *archiveDeferrer) requesterBusy(id string, e *status.Entry, acceptedAt, now time.Time) bool {
	settledAt := acceptedAt.Add(requesterSettle)
	if now.Before(settledAt) {
		return true
	}
	if e == nil || e.Status == pb.ChatStatus_CHAT_STATUS_STOPPED {
		return false
	}
	if d.tracker != nil && d.tracker.Stalled(id) {
		return false
	}
	if e.Status == pb.ChatStatus_CHAT_STATUS_WORKING || e.Status == pb.ChatStatus_CHAT_STATUS_LIMITED {
		return true
	}
	return e.ReceivedAt.Before(settledAt)
}

// signal wakes the loop without blocking. Safe before Run starts.
func (d *archiveDeferrer) signal() {
	select {
	case d.nudge <- struct{}{}:
	default:
	}
}

// has reports whether sessionID has a pending archive.
func (d *archiveDeferrer) has(sessionID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.pending[sessionID]
	return ok
}

// cancel drops a pending archive and clears the archiving flag. It reports
// whether there was one.
func (d *archiveDeferrer) cancel(sessionID string) bool {
	if !d.drop(sessionID) {
		return false
	}
	if d.display != nil {
		d.display.SetArchiving(sessionID, false)
	}
	d.logger.Info().Str("session", sessionID).Msg("pending archive cancelled")
	return true
}

// drop removes the pending entry without touching the archiving flag.
func (d *archiveDeferrer) drop(sessionID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.pending[sessionID]
	delete(d.pending, sessionID)
	return ok
}

// pendingSnapshot is a copy of one pending entry as the sweep read it.
type pendingSnapshot struct {
	gen        uint64
	requesters map[string]time.Time
}

// snapshot returns the pending session ids (sorted) and a copy of each entry.
func (d *archiveDeferrer) snapshot() ([]string, map[string]pendingSnapshot) {
	d.mu.Lock()
	defer d.mu.Unlock()
	ids := make([]string, 0, len(d.pending))
	snaps := make(map[string]pendingSnapshot, len(d.pending))
	for id, entry := range d.pending {
		ids = append(ids, id)
		cp := make(map[string]time.Time, len(entry.requesters))
		for r, at := range entry.requesters {
			cp[r] = at
		}
		snaps[id] = pendingSnapshot{gen: entry.gen, requesters: cp}
	}
	sort.Strings(ids)
	return ids, snaps
}

// run is the deferrer's single loop. It re-evaluates every pending session on
// a nudge or the fallback tick and fires each one whose chats are all idle.
// Fired archives are handed to track so the daemon's archive drain joins them.
// On ctx cancellation it drops every pending entry (they are not durable) and
// returns; it launches nothing after that, which is what lets main.go join it
// as an archive producer before the drain closes.
func (d *archiveDeferrer) run(ctx context.Context, track session.ArchiveWorkerTracker) {
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			d.dropAll()
			return
		case <-d.nudge:
		case <-ticker.C:
		}
		d.sweep(ctx, track)
	}
}

// dropAll empties the pending set without clearing flags: it runs only at
// shutdown, when nothing renders them anymore.
func (d *archiveDeferrer) dropAll() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pending = map[string]*pendingArchive{}
}

// sweep evaluates every pending session once.
func (d *archiveDeferrer) sweep(ctx context.Context, track session.ArchiveWorkerTracker) {
	ids, snaps := d.snapshot()
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		if d.sessions != nil {
			sess, err := d.sessions.Get(ctx, id)
			if errors.Is(err, sql.ErrNoRows) || (err == nil && (sess == nil || sess.ArchivedAt != nil)) {
				// Gone or already archived by another path: nothing to wait for.
				d.cancel(id)
				continue
			}
			if err != nil {
				d.logger.Warn().Err(err).Str("session", id).Msg("pending archive: session lookup failed")
				continue
			}
		}
		blocking, err := d.blockingChat(ctx, id, snaps[id].requesters)
		if err != nil {
			d.logger.Warn().Err(err).Str("session", id).Msg("pending archive: chat lookup failed")
			continue
		}
		if blocking != "" {
			continue
		}
		// Claim only the entry this evaluation read. A request that changed it
		// meanwhile (a new requester guard) or a cancel that dropped it wins;
		// a changed entry is re-evaluated on the next pass.
		d.mu.Lock()
		entry, ok := d.pending[id]
		if !ok || entry.gen != snaps[id].gen {
			d.mu.Unlock()
			if ok {
				d.signal()
			}
			continue
		}
		delete(d.pending, id)
		var f *inflightArchive
		if _, busy := d.inflight[id]; !busy {
			f = d.claimLocked(id)
		}
		d.mu.Unlock()
		if f != nil {
			d.fire(id, f, track)
		}
	}
}

// fire launches the archive for sessionID detached from any caller, bounded by
// fireBudget, and hands its completion to track. Lifecycle.ArchiveSession owns
// the archiving flag from here: its deferred clear ends the spinner on both
// success and failure. A failure is logged and not retried; for merged
// sessions the reconcile sweep re-requests on its next tick.
func (d *archiveDeferrer) fire(sessionID string, f *inflightArchive, track session.ArchiveWorkerTracker) {
	done := safego.Go(d.logger, func() {
		ctx, cancel := context.WithTimeout(context.Background(), d.fireBudget)
		defer cancel()
		err := errArchiveAborted
		defer func() { d.finish(sessionID, f, err) }()
		err = d.execute(ctx, sessionID)
		if err != nil {
			d.logger.Warn().Err(err).Str("session", sessionID).Msg("deferred archive failed")
			return
		}
		d.logger.Info().Str("session", sessionID).Msg("deferred archive completed")
	})
	if track != nil {
		track(sessionID, done)
	}
}

// --- Server surface ---

// archiveDeferral returns the server's deferrer, building it on first use so
// a Server assembled as a struct literal (as most tests do) still gets one.
func (s *Server) archiveDeferral() *archiveDeferrer {
	s.archiveDeferrerOnce.Do(func() {
		var chats archiveChatLister
		if s.agentChats != nil {
			chats = s.agentChats
		}
		var display archivingFlagger
		if s.displayTracker != nil {
			display = s.displayTracker
		}
		var sessions archiveSessionGetter
		if s.sessions != nil {
			sessions = s.sessions
		}
		s.archiveDeferrer = newArchiveDeferrer(chats, s.chatStatus, display, sessions, s.ArchiveSessionAndNotify, s.logger)
	})
	return s.archiveDeferrer
}

// RequestArchive is the single entry point for archiving a session. With no
// busy chat (or with Force) it archives synchronously via
// ArchiveSessionAndNotify; otherwise it defers the archive until every chat is
// idle and reports the chat it is waiting on.
func (s *Server) RequestArchive(ctx context.Context, sessionID string, req ArchiveRequest) (ArchiveOutcome, error) {
	return s.archiveDeferral().request(ctx, sessionID, req)
}

// RequestArchiveAutomatic adapts RequestArchive to the session.SessionArchiver
// shape the automatic archive paths take (archive-after-merge, the dependabot
// auto-archive, the merged-row sweep). A deferred archive is a success.
func (s *Server) RequestArchiveAutomatic(ctx context.Context, sessionID string) error {
	_, err := s.RequestArchive(ctx, sessionID, ArchiveRequest{})
	return err
}

// NudgePendingArchive asks the deferrer to re-evaluate now. It is called from
// the chat-status hook on every status transition; sessionID is accepted for
// the call site's clarity and to skip the wake when nothing is pending there.
func (s *Server) NudgePendingArchive(sessionID string) {
	d := s.archiveDeferral()
	if sessionID != "" && !d.has(sessionID) {
		return
	}
	d.signal()
}

// CancelPendingArchive drops a pending archive for sessionID and clears its
// archiving flag. It reports whether one was pending.
func (s *Server) CancelPendingArchive(sessionID string) bool {
	return s.archiveDeferral().cancel(sessionID)
}

// RunArchiveDeferrer starts the deferrer loop on ctx and returns its done
// channel. Archives it fires are handed to track. main.go joins the loop as an
// archive producer, so it must stop before the archive drain closes.
func (s *Server) RunArchiveDeferrer(ctx context.Context, track session.ArchiveWorkerTracker) <-chan struct{} {
	d := s.archiveDeferral()
	return safego.Go(s.logger, func() { d.run(ctx, track) })
}
