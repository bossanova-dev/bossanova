package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/safego"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/domain/agenttelemetry"
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

// wedgeReadBudget bounds one background persisted read. Parsing a 5.7 GB
// codex rollout took 25s; a read past the budget fails and backs off.
const wedgeReadBudget = 2 * time.Minute

// wedgeReadWait is how long the caller that starts a read waits for it before
// it answers with what is already known. A read that outlasts it finishes in
// the background, serves later callers and reports the change (SetOnChange).
// Callers that hydrate many sessions share one such wait (ShareReadWait).
const wedgeReadWait = 200 * time.Millisecond

// wedgeConcurrentReads caps the background reads parsing at once across every
// session: each can keep a core busy for a whole wedgeReadBudget on a
// multi-gigabyte transcript. A read waits for a slot before its budget starts.
const wedgeConcurrentReads = 2

// TranscriptTallier tallies one chat's transcript over [since, until).
type TranscriptTallier func(ctx context.Context, worktreePath string, chat *domain.AgentChat, since, until time.Time) (agenttelemetry.Counts, error)

// WedgeActivityCache memoises, per session, the persisted output time
// WedgeActivity reads for the session's newest give-up. The known output only
// grows: a later read that finds less, or fails, never lowers it, and a newer
// give-up starts over. A read is due wedgeActivityTTL after the last attempt
// while no output is known or after a failed attempt, and wedgeRecoveryTTL
// after it once output is known. An entry not queried for wedgeIdleTTL is
// dropped. Reads run in the background, one per session at a time, so no
// caller's request waits on a transcript parse or starts a second one, and at
// most wedgeConcurrentReads of them parse at a time. Each chat transcript's
// output is kept per file version, so an unchanged transcript is never parsed
// twice. Share one cache between every hydrator in the process so they hand
// clients the same value. Safe for concurrent use; construct with
// NewWedgeActivityCache.
type WedgeActivityCache struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]wedgeActivityEntry
	// warnedAt is when a failed read of each session was last logged as a
	// warning, so a failing read warns once per TTL.
	warnedAt map[string]time.Time
	// inflight is each session's running background read.
	inflight map[string]wedgeRead
	// slots holds one token per background read parsing now.
	slots chan struct{}
	// onChange is told each session whose known output a read advanced.
	onChange func(sessionID string)
	// transcripts is each chat's output after a give-up, per file version.
	transcripts map[string]transcriptOutput
	// readWait is wedgeReadWait; tests that need a read to land before the
	// caller answers raise it.
	readWait time.Duration
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

type wedgeRead struct {
	// done is closed once the read has recorded its result.
	done <-chan struct{}
}

// transcriptOutput is one chat transcript's newest output after since, read
// from the files identified by version.
type transcriptOutput struct {
	since   time.Time
	version string
	output  time.Time
	usedAt  time.Time
}

// NewWedgeActivityCache returns an empty cache.
func NewWedgeActivityCache() *WedgeActivityCache {
	return &WedgeActivityCache{
		now:         time.Now,
		entries:     make(map[string]wedgeActivityEntry),
		warnedAt:    make(map[string]time.Time),
		inflight:    make(map[string]wedgeRead),
		slots:       make(chan struct{}, wedgeConcurrentReads),
		transcripts: make(map[string]transcriptOutput),
		readWait:    wedgeReadWait,
	}
}

// lookup returns the output known for sessionID's give-up and whether a read
// is due.
func (c *WedgeActivityCache) lookup(sessionID string, giveUpAt time.Time) (time.Time, bool) {
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

// SetOnChange registers fn to be told, from the read's goroutine, each session
// whose known output a background read advanced. A caller that stopped waiting
// for the read answered with the older value, so fn is how that value is
// corrected where nothing polls (the reverse stream to bosso).
func (c *WedgeActivityCache) SetOnChange(fn func(sessionID string)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onChange = fn
}

type wedgeWaitDeadlineKey struct{}

// ShareReadWait returns ctx carrying one read wait for every session hydrated
// under it: a read started under the returned context is waited for only until
// one readWait from now, so a request hydrating many sessions waits at most
// that long in total. A nil cache returns ctx.
func (c *WedgeActivityCache) ShareReadWait(ctx context.Context) context.Context {
	if c == nil {
		return ctx
	}
	return context.WithValue(ctx, wedgeWaitDeadlineKey{}, time.Now().Add(c.readWait))
}

// readWaitFor is how long a caller under ctx waits for the read it started.
func (c *WedgeActivityCache) readWaitFor(ctx context.Context) time.Duration {
	wait := c.readWait
	if deadline, ok := ctx.Value(wedgeWaitDeadlineKey{}).(time.Time); ok {
		wait = min(wait, time.Until(deadline))
	}
	return wait
}

// record stores a read of sessionID's give-up: found is what it found, failed
// whether it was incomplete. The known output keeps the newer of the two, and
// the returned listener is non-nil when the read advanced it. Entries idle for
// wedgeIdleTTL are dropped.
func (c *WedgeActivityCache) record(sessionID string, giveUpAt, found time.Time, failed bool) func(string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for id, e := range c.entries {
		if now.Sub(e.usedAt) >= wedgeIdleTTL {
			delete(c.entries, id)
		}
	}
	for id, t := range c.transcripts {
		if now.Sub(t.usedAt) >= wedgeIdleTTL {
			delete(c.transcripts, id)
		}
	}
	e, ok := c.entries[sessionID]
	if !ok || !e.giveUpAt.Equal(giveUpAt) {
		e = wedgeActivityEntry{giveUpAt: giveUpAt}
	}
	var advanced func(string)
	if found.After(e.output) {
		e.output = found
		advanced = c.onChange
	}
	e.readAt, e.failed, e.usedAt = now, failed, now
	c.entries[sessionID] = e
	return advanced
}

// startRead runs read in the background for sessionID unless a read of the
// session is already running, and returns the channel closed once the running
// read finishes and whether this call started it. The read waits for one of
// the cache's slots, then runs within wedgeReadBudget. Its context keeps ctx's
// values but not its cancellation: callers that go away leave the read to
// finish for whoever asks next.
func (c *WedgeActivityCache) startRead(ctx context.Context, logger zerolog.Logger, sessionID string, read func(context.Context)) (<-chan struct{}, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r, ok := c.inflight[sessionID]; ok {
		return r.done, false
	}
	done := safego.Go(logger, func() {
		defer func() {
			c.mu.Lock()
			delete(c.inflight, sessionID)
			c.mu.Unlock()
		}()
		c.slots <- struct{}{}
		defer func() { <-c.slots }()
		readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), wedgeReadBudget)
		defer cancel()
		read(readCtx)
	})
	c.inflight[sessionID] = wedgeRead{done: done}
	return done, true
}

// transcriptOutputAt returns chat's output after since known for the
// transcript files identified by version.
func (c *WedgeActivityCache) transcriptOutputAt(chatID string, since time.Time, version string) (time.Time, bool) {
	if c == nil {
		return time.Time{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.transcripts[chatID]
	if !ok || !t.since.Equal(since) || t.version != version {
		return time.Time{}, false
	}
	t.usedAt = c.now()
	c.transcripts[chatID] = t
	return t.output, true
}

// recordTranscriptOutput stores chat's output after since read from the
// transcript files identified by version.
func (c *WedgeActivityCache) recordTranscriptOutput(chatID string, since time.Time, version string, output time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.transcripts[chatID] = transcriptOutput{since: since, version: version, output: output, usedAt: c.now()}
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
	Runs func(ctx context.Context, sessionID string) ([]domain.AgentRun, error)
	// Tally reads one chat's transcript, the files interactiveTranscriptPaths
	// resolves; nil uses tallyInteractiveAgentRunForServer.
	Tally TranscriptTallier
	// Cache memoises the persisted read and runs it in the background; nil
	// reads inline under the caller's context on every call.
	Cache *WedgeActivityCache
	// Logger records a persisted read that failed, at most once per session
	// per wedgeActivityTTL; the zero Logger discards.
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
	Chats []*domain.AgentChat
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
	if output := w.persistedOutput(ctx, q); output.After(q.Live) {
		return output
	}
	return q.Live
}

// persistedOutput returns the persisted output known after q's give-up. With
// no cache it reads inline under ctx. With one it serves the cached value and,
// when a read is due, starts one in the background and waits for it at most
// the cache's readWait, or what is left of a wait ShareReadWait put on ctx, so
// a slow read never holds the caller's request.
func (w WedgeActivity) persistedOutput(ctx context.Context, q WedgeQuery) time.Time {
	read := func(ctx context.Context) (time.Time, bool) {
		found, err := w.persistedOutputAfter(ctx, q.SessionID, q.WorktreePath, q.GiveUpAt, q.Chats)
		if err != nil && w.Cache.shouldWarn(q.SessionID) {
			w.Logger.Warn().Err(err).Str("session_id", q.SessionID).Msg("restart-proof agent activity: persisted read failed; backing off")
		}
		return found, err != nil
	}
	if w.Cache == nil {
		found, _ := read(ctx)
		return found
	}
	output, due := w.Cache.lookup(q.SessionID, q.GiveUpAt)
	if !due {
		return output
	}
	done, started := w.Cache.startRead(ctx, w.Logger, q.SessionID, func(ctx context.Context) {
		found, failed := read(ctx)
		if onChange := w.Cache.record(q.SessionID, q.GiveUpAt, found, failed); onChange != nil {
			onChange(q.SessionID)
		}
	})
	waitFor := w.Cache.readWaitFor(ctx)
	if !started || waitFor <= 0 {
		return output
	}
	wait := time.NewTimer(waitFor)
	defer wait.Stop()
	select {
	case <-done:
		output, _ = w.Cache.lookup(q.SessionID, q.GiveUpAt)
	case <-wait.C:
	case <-ctx.Done():
	}
	return output
}

// persistedOutputAfter returns the newest persisted output strictly after
// giveUpAt, or zero, and a non-nil error when the read did not complete: the
// context ended, the run list failed, or a transcript failed to read. A chat
// with no transcript (os.ErrNotExist) has nothing to find, so it does not make
// the read incomplete.
func (w WedgeActivity) persistedOutputAfter(ctx context.Context, sessionID, worktreePath string, giveUpAt time.Time, chats []*domain.AgentChat) (time.Time, error) {
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
			if r.StoppedAt != nil && r.StoppedAt.After(at) && r.StopReason != domain.AgentRunStopDaemonRestart {
				at = *r.StoppedAt
			}
			if at.After(newest) {
				newest = at
			}
		}
	}
	now := time.Now()
	for _, chat := range chats {
		if ctx.Err() != nil {
			break
		}
		output, err := w.transcriptOutputAfter(ctx, worktreePath, chat, giveUpAt, now)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("tally chat %s: %w", chat.AgentSessionID, err))
			continue
		}
		if output.After(giveUpAt) && output.After(newest) {
			newest = output
		}
	}
	if err := ctx.Err(); err != nil {
		errs = append(errs, err)
	}
	return newest, errors.Join(errs...)
}

// transcriptOutputAfter returns the newest output in chat's transcript after
// since. Transcript files not modified since then cannot hold any, so they are
// not read; otherwise the output read from one version of the files (their
// paths, sizes and mtimes) is reused until they change. A chat whose files
// cannot be resolved is tallied without either shortcut.
func (w WedgeActivity) transcriptOutputAfter(ctx context.Context, worktreePath string, chat *domain.AgentChat, since, until time.Time) (time.Time, error) {
	tally := w.Tally
	if tally == nil {
		tally = tallyInteractiveAgentRunForServer
	}
	version, modified, versionErr := transcriptVersion(worktreePath, chat)
	if versionErr == nil {
		if !modified.After(since) {
			return time.Time{}, nil
		}
		if output, ok := w.Cache.transcriptOutputAt(chat.AgentSessionID, since, version); ok {
			return output, nil
		}
	}
	counts, err := tally(ctx, worktreePath, chat, since, until)
	if err != nil {
		return time.Time{}, err
	}
	if versionErr == nil {
		w.Cache.recordTranscriptOutput(chat.AgentSessionID, since, version, counts.LastOutputAt)
	}
	return counts.LastOutputAt, nil
}

// transcriptVersion identifies the bytes of chat's transcript files by their
// paths, sizes and mtimes, and returns the newest mtime.
func transcriptVersion(worktreePath string, chat *domain.AgentChat) (string, time.Time, error) {
	paths, err := interactiveTranscriptPaths(worktreePath, chat)
	if err != nil {
		return "", time.Time{}, err
	}
	var (
		version  strings.Builder
		modified time.Time
	)
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return "", time.Time{}, err
		}
		fmt.Fprintf(&version, "%s\x00%d\x00%d\x00", path, info.Size(), info.ModTime().UnixNano())
		if info.ModTime().After(modified) {
			modified = info.ModTime()
		}
	}
	return version.String(), modified, nil
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
// See docs/structure/contracts.md#skipping-a-bump-justify-it-in-the-diff.
func HydrateRestartProofActivity(ctx context.Context, p *pb.Session, chats []*domain.AgentChat, w WedgeActivity) {
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
func productiveRun(r domain.AgentRun) bool {
	return r.StopReason == domain.AgentRunStopClean || (r.OutputTokenCount != nil && *r.OutputTokenCount > 0)
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
func AgentRunsForWedge(store agentRunReader) func(ctx context.Context, sessionID string) ([]domain.AgentRun, error) {
	if store == nil {
		return nil
	}
	return func(ctx context.Context, sessionID string) ([]domain.AgentRun, error) {
		return store.List(ctx, wedgeRunFilter(sessionID))
	}
}

// wedgeRunFilter is the run listing WedgeActivity reads; the session timeline
// lists its runs with it too and hands them to WedgeActivity as they are.
func wedgeRunFilter(sessionID string) domain.AgentRunFilter {
	return domain.AgentRunFilter{
		SessionID:         sessionID,
		IncludeOpen:       true,
		IncludeAllReasons: true,
		IncludeBackfilled: true,
		OmitChildren:      true,
	}
}
