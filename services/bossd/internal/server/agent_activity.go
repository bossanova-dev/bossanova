package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/recurser/bossalib/agenttelemetry"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/sessionattention"
)

// wedgeActivityTTL bounds how often one session's persisted output after a
// give-up is re-read while nothing has shown it recovered, and how long a
// failed read backs off before it is retried: hydration runs on every
// ListSessions poll and every reverse-stream delta.
const wedgeActivityTTL = 30 * time.Second

// wedgeRecoveryTTL is how long output found after a give-up is served before
// it is re-read. The output already proves the give-up recovered, so the
// re-read only refreshes the activity time (a stopped chat reaches no live
// tracker).
const wedgeRecoveryTTL = 5 * time.Minute

// wedgeIdleTTL is how long a session's entry outlives its last query. It is
// far longer than either re-read interval, so a session that is still being
// polled keeps the output it is known to have produced; it only bounds the map
// to sessions queried recently.
const wedgeIdleTTL = time.Hour

// TranscriptTallier tallies one chat's transcript over [since, until).
type TranscriptTallier func(ctx context.Context, worktreePath string, chat *models.AgentChat, since, until time.Time) (agenttelemetry.Counts, error)

// WedgeActivityCache memoises, per session, the persisted output time
// WedgeActivity reads for the session's newest give-up. The known output only
// grows: a later read that finds less, or fails, never lowers it, and a newer
// give-up starts over. A read is due wedgeActivityTTL after the last attempt
// while no output is known or after a failed attempt, and wedgeRecoveryTTL
// after it once output is known. An entry not queried for wedgeIdleTTL is
// dropped. Share one cache between every hydrator in the process so they hand
// clients the same value. Safe for concurrent use; construct with
// NewWedgeActivityCache.
type WedgeActivityCache struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]wedgeActivityEntry
	// warnedAt is when a failed read of each session was last logged as a
	// warning, so a failing read warns once per TTL.
	warnedAt map[string]time.Time
}

type wedgeActivityEntry struct {
	giveUpAt time.Time
	// output is the newest output known after giveUpAt; zero when none.
	output time.Time
	// readAt is the last read attempt, completed or failed.
	readAt time.Time
	// failed is whether that attempt failed.
	failed bool
	// usedAt is the last query.
	usedAt time.Time
}

// NewWedgeActivityCache returns an empty cache.
func NewWedgeActivityCache() *WedgeActivityCache {
	return &WedgeActivityCache{
		now:      time.Now,
		entries:  make(map[string]wedgeActivityEntry),
		warnedAt: make(map[string]time.Time),
	}
}

// lookup returns the output known for sessionID's give-up and whether a read
// is due. A nil cache knows nothing and is always due.
func (c *WedgeActivityCache) lookup(sessionID string, giveUpAt time.Time) (time.Time, bool) {
	if c == nil {
		return time.Time{}, true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	e, ok := c.entries[sessionID]
	if !ok || !e.giveUpAt.Equal(giveUpAt) {
		return time.Time{}, true
	}
	e.usedAt = now
	c.entries[sessionID] = e
	ttl := wedgeRecoveryTTL
	if e.output.IsZero() || e.failed {
		ttl = wedgeActivityTTL
	}
	return e.output, now.Sub(e.readAt) >= ttl
}

// record stores a read of sessionID's give-up: found is what it found, failed
// whether it was incomplete. The known output keeps the newer of the two.
// Entries idle for wedgeIdleTTL are dropped.
func (c *WedgeActivityCache) record(sessionID string, giveUpAt, found time.Time, failed bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for id, e := range c.entries {
		if now.Sub(e.usedAt) >= wedgeIdleTTL {
			delete(c.entries, id)
		}
	}
	e, ok := c.entries[sessionID]
	if !ok || !e.giveUpAt.Equal(giveUpAt) {
		e = wedgeActivityEntry{giveUpAt: giveUpAt}
	}
	if found.After(e.output) {
		e.output = found
	}
	e.readAt, e.failed, e.usedAt = now, failed, now
	c.entries[sessionID] = e
}

// shouldWarn reports whether a failed read of sessionID is due a warning:
// none was logged for it within wedgeActivityTTL. A nil cache always warns.
func (c *WedgeActivityCache) shouldWarn(sessionID string) bool {
	if c == nil {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for id, at := range c.warnedAt {
		if now.Sub(at) >= wedgeActivityTTL {
			delete(c.warnedAt, id)
		}
	}
	if _, warned := c.warnedAt[sessionID]; warned {
		return false
	}
	c.warnedAt[sessionID] = now
	return true
}

// WedgeActivity makes a session's last agent activity restart-proof. The live
// chat tracker is the only source of pb.Session.last_agent_activity_at, and it
// is empty after a bossd restart and skips stopped chats, so after a lost-login
// give-up it cannot tell a recovered session from a wedged one. Every reader of
// that recovery (the TUI hint, the web hint, the session timeline) judges it on
// last_agent_activity_at and on a merged or closed PR (alertstate.WedgeRecovery),
// so bossd fills that one field from the persisted evidence too, on the one
// path where it decides something: the PR is not done.
type WedgeActivity struct {
	// Runs lists the session's agent runs; nil reads none.
	Runs func(ctx context.Context, sessionID string) ([]db.AgentRun, error)
	// Tally reads one chat's transcript; nil uses tallyInteractiveAgentRunForServer.
	Tally TranscriptTallier
	// Cache memoises the persisted read; nil reads every time.
	Cache *WedgeActivityCache
	// Logger records a persisted read that failed; the zero Logger discards.
	Logger zerolog.Logger
}

// WedgeQuery is one session's input to RestartProofActivity.
type WedgeQuery struct {
	SessionID    string
	WorktreePath string
	// Live is the live chat tracker's newest output; zero when none.
	Live time.Time
	// GiveUpAt is when the session's newest lost-login or usage-limit event
	// gave up (sessionattention.NewestGiveUpAt over the events the caller
	// judges the incident on); zero when it did not give up. A give-up with
	// no timestamp cannot be ordered against any output, so it is zero too.
	GiveUpAt time.Time
	// PRDone is true when the session's PR is merged or closed. That
	// recovers every give-up already (alertstate.WedgeRecovery.PRDone), so
	// the persisted read decides nothing and is skipped.
	PRDone bool
	// Chats are the session's chats whose transcripts are read.
	Chats []*models.AgentChat
}

// RestartProofActivity returns the session's newest real agent output time.
// It is q.Live unless the session's newest lost-login or usage-limit event
// gave up (q.GiveUpAt), live is not after it, and the PR is not done. Then it
// is the newest of live and the persisted output after the give-up: the
// newest message with output tokens in any chat's transcript, and the newest
// productive agent run that started after it (its stop, or its start while
// open or once a bossd restart closed it). A productive run produced output
// tokens or stopped cleanly; it covers a run whose transcript is not keyed by
// any chat (a headless replacement run). A zero-token message is how an auth
// failure is transcribed, so it never counts. The transcript and run reads
// happen only on this path.
func (w WedgeActivity) RestartProofActivity(ctx context.Context, q WedgeQuery) time.Time {
	if q.PRDone || q.GiveUpAt.IsZero() || q.Live.After(q.GiveUpAt) {
		return q.Live
	}
	output, due := w.Cache.lookup(q.SessionID, q.GiveUpAt)
	if due {
		found, err := w.persistedOutputAfter(ctx, q.SessionID, q.WorktreePath, q.GiveUpAt, q.Chats)
		switch {
		case err == nil:
			w.Cache.record(q.SessionID, q.GiveUpAt, found, false)
		case ctx.Err() != nil:
			// The caller went away: not the transcript's fault, so no
			// backoff. What the read found is still served below.
			w.logFailedRead(ctx, q.SessionID, err)
		default:
			// A failed read backs off for wedgeActivityTTL rather than
			// re-reading on every poll, and keeps the known output.
			w.logFailedRead(ctx, q.SessionID, err)
			w.Cache.record(q.SessionID, q.GiveUpAt, found, true)
		}
		if found.After(output) {
			output = found
		}
	}
	if output.After(q.Live) {
		return output
	}
	return q.Live
}

// persistedOutputAfter returns the newest persisted output strictly after
// giveUpAt, or zero, and a non-nil error when the read did not complete: the
// context ended, the run list failed, or a transcript failed to read. A chat
// with no transcript (os.ErrNotExist) has nothing to find, so it does not make
// the read incomplete.
func (w WedgeActivity) persistedOutputAfter(ctx context.Context, sessionID, worktreePath string, giveUpAt time.Time, chats []*models.AgentChat) (time.Time, error) {
	var (
		newest time.Time
		errs   []error
	)
	if w.Runs != nil {
		runs, err := w.Runs(ctx, sessionID)
		if err != nil {
			errs = append(errs, fmt.Errorf("list agent runs: %w", err))
		}
		for _, r := range runs {
			if !productiveRun(r) || !r.StartedAt.After(giveUpAt) {
				continue
			}
			// A run's output came after its start. Its stop is the agent's
			// exit, except a daemon_restart stop: ReconcileOpen stamps the
			// restart time on runs that were open, which is no evidence of
			// output, so such a run gives only its start.
			at := r.StartedAt
			if r.StoppedAt != nil && r.StoppedAt.After(at) && r.StopReason != db.AgentRunStopDaemonRestart {
				at = *r.StoppedAt
			}
			if at.After(newest) {
				newest = at
			}
		}
	}
	tally := w.Tally
	if tally == nil {
		tally = tallyInteractiveAgentRunForServer
	}
	now := time.Now()
	for _, chat := range chats {
		if ctx.Err() != nil {
			break
		}
		counts, err := tally(ctx, worktreePath, chat, giveUpAt, now)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("tally chat %s: %w", chat.AgentSessionID, err))
			continue
		}
		if counts.LastOutputAt.After(giveUpAt) && counts.LastOutputAt.After(newest) {
			newest = counts.LastOutputAt
		}
	}
	if err := ctx.Err(); err != nil {
		errs = append(errs, err)
	}
	return newest, errors.Join(errs...)
}

// logFailedRead records an incomplete persisted read. A caller that went away
// (its context ended) is not a fault, so it is a debug line; any other failure
// warns, at most once per session per wedgeActivityTTL.
func (w WedgeActivity) logFailedRead(ctx context.Context, sessionID string, err error) {
	if ctx.Err() != nil {
		w.Logger.Debug().Err(err).Str("session_id", sessionID).Msg("restart-proof agent activity: persisted read abandoned")
		return
	}
	if w.Cache.shouldWarn(sessionID) {
		w.Logger.Warn().Err(err).Str("session_id", sessionID).Msg("restart-proof agent activity: persisted read failed; backing off")
	}
}

// HydrateRestartProofActivity applies WedgeActivity.RestartProofActivity to
// p.LastAgentActivityAt, judging the give-up on p's hydrated rotation events
// (the ones every client reading p judges the wedge on) and the PR on its
// display status. Call it after the display status, the rotation events and
// the live activity are hydrated (HydrateDisplayEntry, HydrateRotationEvents,
// HydrateAgentObservability).
//
// No dated apiversion bump, and why: this fills last_agent_activity_at from
// persisted evidence where it used to be empty after a restart. Its clients are
// the local boss TUI/CLI through GetSession/ListSessions (co-deployed with
// bossd) and the web and remote clients through bosso, whose read model the
// reverse stream feeds with this same hydrated session (version-skewed). No
// transform is needed or possible: the value now matches what the field
// already promises (the most recent real agent output), so an older client
// reads a truer value of the field it was built against; and bosso cannot tell
// a persisted value from a live one, so no down-convert or handler gate could
// reproduce the old empty value without also erasing genuine live activity.
// See docs/api-versioning.md#skipping-a-bump-justify-it-in-the-diff.
func HydrateRestartProofActivity(ctx context.Context, p *pb.Session, chats []*models.AgentChat, w WedgeActivity) {
	if p == nil {
		return
	}
	var live time.Time
	if ts := p.GetLastAgentActivityAt(); ts != nil {
		live = ts.AsTime()
	}
	q := WedgeQuery{
		SessionID: p.GetId(), WorktreePath: p.GetWorktreePath(), Live: live, Chats: chats,
		// The display status shares vcs.DisplayStatus's numbering.
		PRDone: sessionattention.PRDone(vcs.DisplayStatus(p.GetDisplayStatus())),
	}
	if giveUpAt, ok := sessionattention.NewestGiveUpAtProto(p.GetRotationEvents()); ok {
		q.GiveUpAt = giveUpAt
	}
	if at := w.RestartProofActivity(ctx, q); at.After(live) {
		p.LastAgentActivityAt = timestamppb.New(at)
	}
}

// productiveRun reports whether r shows a working login: it stopped cleanly or
// produced output tokens. A model-call count is not enough, because an auth
// failure is transcribed as a zero-token assistant message and tallied as one.
func productiveRun(r db.AgentRun) bool {
	return r.StopReason == db.AgentRunStopClean || (r.OutputTokenCount != nil && *r.OutputTokenCount > 0)
}

// wedgeActivity is the server's WedgeActivity: its agent-run store, its
// transcript tally, and its cache.
func (s *Server) wedgeActivity() WedgeActivity {
	w := WedgeActivity{Tally: s.transcriptTally, Cache: s.wedgeCache, Logger: s.logger}
	if s.agentRuns != nil {
		w.Runs = AgentRunsForWedge(s.agentRuns)
	}
	return w
}

// AgentRunsForWedge lists a session's agent runs the way WedgeActivity reads
// them: open runs, every stop reason, backfilled runs, no child spans.
func AgentRunsForWedge(store db.AgentRunStore) func(ctx context.Context, sessionID string) ([]db.AgentRun, error) {
	if store == nil {
		return nil
	}
	return func(ctx context.Context, sessionID string) ([]db.AgentRun, error) {
		return store.List(ctx, wedgeRunFilter(sessionID))
	}
}

// wedgeRunFilter is the run listing WedgeActivity reads; the session timeline
// lists its runs with it too and hands them to WedgeActivity as they are.
func wedgeRunFilter(sessionID string) db.AgentRunFilter {
	return db.AgentRunFilter{
		SessionID:         sessionID,
		IncludeOpen:       true,
		IncludeAllReasons: true,
		IncludeBackfilled: true,
		OmitChildren:      true,
	}
}
