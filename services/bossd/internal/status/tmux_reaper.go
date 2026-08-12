package status

import (
	"context"
	"regexp"
	"sync"
	"time"

	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/safego"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/tmux"
	"github.com/rs/zerolog"
)

// Buckets a live tmux session can be classified into (BOS-846 D1). The three-
// bucket split is copied from services/boss/internal/daemon/mcpprocess.go:
// BOS-349 mass-killed a peer daemon's plugins for weeks because its sweep only
// had "matched" and "unmatched". Anything bossd cannot positively attribute to
// itself lands in bucketUnattributable, which is logged and never signalled.
const (
	bucketKeep           = "keep"
	bucketUnattributable = "unattributable"
	bucketReap           = "reap"
)

// Reasons attached to each classification decision. They are stable strings so
// an operator reading dry-run output can grep for one.
const (
	reasonNotBossOwned        = "notBossOwned"
	reasonAccountedFor        = "accountedFor"
	reasonEmptyWhitelist      = "emptyWhitelist"
	reasonWithinGrace         = "withinGrace"
	reasonUnstamped           = "unstamped"
	reasonForeignDaemon       = "foreignDaemon"
	reasonFirstStrike         = "firstStrike"
	reasonAwaitingConfirm     = "awaitingConfirmation"
	reasonUnaccountedForTwice = "unaccountedForTwice"
)

// bossNamePattern is the strict form every name minted by tmux.SessionName and
// tmux.ChatSessionName takes (BOS-846 D2). sqlutil.NewID() is 8 random bytes
// hex-encoded, so each id is 16 lowercase hex characters and each truncated
// component is exactly 8. The looser `boss-` prefix that bindDetachKeys matches
// on would also match a user's own `boss-notes`, which is why this sweep does
// not use it.
var bossNamePattern = regexp.MustCompile(`^boss-([0-9a-f]{8})-[0-9a-f]{8}$`)

// bossOwnedName reports whether name is a pane this daemon's naming convention
// could have produced: the strict regex above AND a repo component that is the
// 8-character prefix of some id in repoPrefixes. Both checks are required — the
// regex alone would accept `boss-deadbeef-deadbeef` for a repo that has never
// existed on this host.
//
// It is pure so the name gate is testable without tmux, a database or a clock.
func bossOwnedName(name string, repoPrefixes map[string]struct{}) bool {
	m := bossNamePattern.FindStringSubmatch(name)
	if m == nil {
		return false
	}
	_, ok := repoPrefixes[m[1]]
	return ok
}

// killSessionFunc is the destructive seam (BOS-846 D8, following BOS-333's
// reapFinalizer). Routing the kill through a field rather than calling
// tmux.Client.KillSession inline is what makes dry-run nearly free and what
// stops `make test-bossd` from reaping the developer's own live agent panes —
// the concrete failure BOS-349 hit when an unstubbed signalling seam made a
// unit test run a real sweep.
type killSessionFunc func(ctx context.Context, name string) error

// TmuxReaper periodically asks tmux what is actually running, subtracts every
// name bossd can account for, and kills what is left.
//
// It is the only bossd component that starts from tmux rather than from a
// database row, which is exactly the blind spot it exists to close: every other
// cleanup path kills the name a row carries, so a pane whose name was never
// persisted — or whose row is already gone — is invisible to all of them.
//
// It mirrors TmuxStatusPoller's Run/Done shape and, like it, re-reads
// everything each tick; the only state carried between sweeps is the
// confirmation-strike map.
type TmuxReaper struct {
	chats    db.AgentChatStore
	sessions db.SessionStore
	repos    db.RepoStore
	tmux     *tmux.Client
	logger   zerolog.Logger

	cfg config.TmuxReaperConfig

	// daemonID is this bossd instance's identity, compared against the
	// tmux.DaemonIDEnvKey stamp baked into every pane this daemon creates. An
	// empty daemonID can never match a stamp, so a daemon that cannot identify
	// itself attributes nothing to itself and reaps only unstamped panes (and
	// only then with reap_unstamped set).
	daemonID string

	// now is injectable so grace and confirmation windows are testable without
	// sleeping, following Lifecycle.clock.
	now func() time.Time

	// kill is the injectable destructive seam. Never call tmux.KillSession
	// directly from a sweep.
	kill killSessionFunc

	mu sync.Mutex
	// strikes maps a tmux session name to the instant it was FIRST observed
	// unaccounted-for. A candidate is only reaped once that observation is
	// itself at least the grace window old (BOS-846 D5, following BOS-87), and
	// any accounted-for observation clears the marker. Pruned every sweep for
	// names no longer live — per the BOS-477 finding, a per-session map that is
	// never pruned leaks for the daemon's lifetime.
	strikes map[string]time.Time

	done chan struct{} // closed when Run's goroutine exits
}

// NewTmuxReaper builds a reaper wired to the production kill seam. Callers that
// need to observe or suppress the destructive call — every test — replace it
// via setKillSeam.
func NewTmuxReaper(
	chats db.AgentChatStore,
	sessions db.SessionStore,
	repos db.RepoStore,
	tmuxClient *tmux.Client,
	daemonID string,
	cfg config.TmuxReaperConfig,
	logger zerolog.Logger,
) *TmuxReaper {
	r := &TmuxReaper{
		chats:    chats,
		sessions: sessions,
		repos:    repos,
		tmux:     tmuxClient,
		logger:   logger,
		cfg:      cfg,
		daemonID: daemonID,
		now:      time.Now,
		strikes:  make(map[string]time.Time),
		done:     make(chan struct{}),
	}
	r.kill = func(ctx context.Context, name string) error {
		return tmuxClient.KillSession(ctx, name)
	}
	return r
}

// setKillSeam replaces the destructive call. Tests use it to record instead of
// execute; production never calls it.
func (r *TmuxReaper) setKillSeam(fn killSessionFunc) { r.kill = fn }

// setClock replaces the reaper's clock so grace and confirmation windows can be
// crossed without sleeping.
func (r *TmuxReaper) setClock(fn func() time.Time) { r.now = fn }

// Run starts the sweep goroutine. It stops when ctx is cancelled.
//
// A disabled reaper closes done immediately and never sweeps: the feature is
// off by default, and a disabled sweep that still listed sessions every five
// minutes would be pure cost.
func (r *TmuxReaper) Run(ctx context.Context) {
	if !r.cfg.IsEnabled() {
		r.logger.Info().Msg("tmux reaper: disabled (settings.tmux_reaper.enabled is not true)")
		close(r.done)
		return
	}
	interval := r.cfg.SweepInterval()
	r.logger.Info().
		Dur("sweepInterval", interval).
		Dur("gracePeriod", r.cfg.GracePeriod()).
		Bool("dryRun", r.cfg.IsDryRun()).
		Bool("reapUnstamped", r.cfg.ReapsUnstamped()).
		Msg("tmux reaper: starting")
	safego.Go(r.logger, func() {
		defer close(r.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := r.SweepOnce(ctx); err != nil {
					// Already logged at the failure site; the sweep is
					// fail-closed, so the next tick simply retries.
					continue
				}
			}
		}
	})
}

// Done returns a channel closed when Run's goroutine exits.
func (r *TmuxReaper) Done() <-chan struct{} { return r.done }

// SweepOnce performs one reconciliation and returns the number of sessions
// actually killed. It is fail-closed (BOS-846 D6): a tmux read that is not the
// affirmative "no server running" signal, or any store read error, ends the
// sweep having killed nothing and returns the error. Nothing partial is ever
// acted on.
func (r *TmuxReaper) SweepOnce(ctx context.Context) (int, error) {
	live, err := r.tmux.ListSessions(ctx)
	if err != nil {
		r.logger.Warn().Err(err).Msg("tmux reaper: list-sessions failed; sweep aborted with zero kills")
		return 0, err
	}

	repoPrefixes, err := r.repoPrefixes(ctx)
	if err != nil {
		r.logger.Warn().Err(err).Msg("tmux reaper: repo read failed; sweep aborted with zero kills")
		return 0, err
	}

	whitelist, err := r.accountedFor(ctx)
	if err != nil {
		r.logger.Warn().Err(err).Msg("tmux reaper: store read failed; sweep aborted with zero kills")
		return 0, err
	}

	now := r.now()
	grace := r.cfg.GracePeriod()
	dryRun := r.cfg.IsDryRun()

	var bossShaped, keep, unattributable, candidates, reaped int
	liveNames := make(map[string]struct{}, len(live))

	for _, s := range live {
		liveNames[s.Name] = struct{}{}
		if !bossOwnedName(s.Name, repoPrefixes) {
			// Not ours by name. Logged at Trace only: on a developer's host this
			// is every shell they have open, so anything louder would drown the
			// decisions that matter.
			r.logger.Trace().
				Str("tmuxSession", s.Name).
				Str("bucket", bucketKeep).
				Str("reason", reasonNotBossOwned).
				Msg("tmux reaper: session is not boss-owned")
			r.clearStrike(s.Name)
			continue
		}
		bossShaped++

		bucket, reason := r.classify(ctx, s, whitelist, now, grace)
		switch bucket {
		case bucketKeep:
			keep++
			r.logger.Debug().
				Str("tmuxSession", s.Name).
				Str("bucket", bucket).
				Str("reason", reason).
				Time("sessionCreated", s.Created).
				Bool("dryRun", dryRun).
				Msg("tmux reaper: keeping session")
		case bucketUnattributable:
			unattributable++
			r.logger.Info().
				Str("tmuxSession", s.Name).
				Str("bucket", bucket).
				Str("reason", reason).
				Time("sessionCreated", s.Created).
				Bool("dryRun", dryRun).
				Msg("tmux reaper: session is unattributable; leaving it alive")
		case bucketReap:
			candidates++
			evt := r.logger.Info().
				Str("tmuxSession", s.Name).
				Str("bucket", bucket).
				Str("reason", reason).
				Time("sessionCreated", s.Created).
				Bool("dryRun", dryRun)
			if dryRun {
				evt.Msg("tmux reaper: would reap orphaned session (dry run)")
				continue
			}
			if err := r.kill(ctx, s.Name); err != nil {
				evt.Err(err).Msg("tmux reaper: failed to reap orphaned session")
				continue
			}
			r.clearStrike(s.Name)
			reaped++
			evt.Msg("tmux reaper: reaped orphaned session")
		}
	}

	r.pruneStrikes(liveNames)

	// Emitted on EVERY tick, including one that reaped nothing: a summary only
	// on non-zero would make the sweep invisible exactly when an operator is
	// deciding whether to arm it.
	r.logger.Info().
		Int("live", len(live)).
		Int("bossShaped", bossShaped).
		Int("keep", keep).
		Int("unattributable", unattributable).
		Int("candidates", candidates).
		Int("reaped", reaped).
		Bool("dryRun", dryRun).
		Msg("tmux reaper: sweep complete")

	return reaped, nil
}

// classify places one boss-shaped live session into a bucket. The order of the
// gates is the order of the acceptance criteria, and each one can only ever
// spare a pane.
func (r *TmuxReaper) classify(
	ctx context.Context,
	s tmux.LiveSession,
	whitelist map[string]struct{},
	now time.Time,
	grace time.Duration,
) (bucket, reason string) {
	if _, ok := whitelist[s.Name]; ok {
		r.clearStrike(s.Name)
		return bucketKeep, reasonAccountedFor
	}

	// Degenerate case, called out explicitly by D6: a whitelist that came back
	// empty while boss-shaped panes are live is far more likely to be a broken
	// read than a host on which every single pane is an orphan.
	if len(whitelist) == 0 {
		return bucketUnattributable, reasonEmptyWhitelist
	}

	// tmux's own session_created clock (D4). The two alternatives are refuted
	// in-repo: a pipe-pane log mtime never ages while an idle TUI repaints, and
	// agent_chats.created_at is blinded by the create race this reaper exists
	// to cover. A pane is reapable at exactly grace, not a tick later.
	if now.Sub(s.Created) < grace {
		return bucketKeep, reasonWithinGrace
	}

	// Ownership must be proven, not assumed (D7). Two bossd instances on one
	// host have separate databases, so daemon A's whitelist cannot contain
	// daemon B's panes.
	stamp, stamped := r.tmux.ShowEnv(ctx, s.Name, tmux.DaemonIDEnvKey)
	switch {
	case !stamped:
		if !r.cfg.ReapsUnstamped() {
			return bucketUnattributable, reasonUnstamped
		}
	case stamp == "" || stamp != r.daemonID:
		return bucketUnattributable, reasonForeignDaemon
	}

	// Confirmation strike (D5). Age answers "was the pane young?"; it does not
	// answer "was the DB momentarily unable to account for it?" — the shape of
	// BOS-426, where a periodic sweep finalized sessions still being created.
	first, seen := r.recordStrike(s.Name, now)
	if !seen {
		return bucketKeep, reasonFirstStrike
	}
	if now.Sub(first) < grace {
		return bucketKeep, reasonAwaitingConfirm
	}

	return bucketReap, reasonUnaccountedForTwice
}

// recordStrike returns the instant name was first observed unaccounted-for and
// whether such an observation already existed. The first call for a name stores
// now and reports seen=false.
func (r *TmuxReaper) recordStrike(name string, now time.Time) (first time.Time, seen bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if first, ok := r.strikes[name]; ok {
		return first, true
	}
	r.strikes[name] = now
	return now, false
}

// clearStrike drops any confirmation marker for name. Called whenever a session
// is accounted for again, so a candidate that recovers a DB claim starts from
// zero rather than inheriting a stale strike.
func (r *TmuxReaper) clearStrike(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.strikes, name)
}

// pruneStrikes drops markers for names tmux no longer reports. Without this the
// map grows for the daemon's lifetime (the BOS-477 finding).
func (r *TmuxReaper) pruneStrikes(liveNames map[string]struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for name := range r.strikes {
		if _, ok := liveNames[name]; !ok {
			delete(r.strikes, name)
		}
	}
}

// strikeCount reports how many confirmation markers are held. Test-only
// observability for the leak assertion.
func (r *TmuxReaper) strikeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.strikes)
}

// repoPrefixes returns the 8-character id prefix of every known repo, the
// second half of the name gate.
func (r *TmuxReaper) repoPrefixes(ctx context.Context) (map[string]struct{}, error) {
	repos, err := r.repos.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]struct{}, len(repos))
	for _, repo := range repos {
		if repo == nil || len(repo.ID) < 8 {
			continue
		}
		out[repo.ID[:8]] = struct{}{}
	}
	return out, nil
}

// accountedFor assembles the whitelist by union (BOS-846 D3). Recomputation only
// ever WIDENS the set, which is why attach.go's warning against recomputing a
// pane's name does not apply here: that warning governs IDENTIFYING a pane,
// where an 8-character truncation collision would mislead, whereas a collision
// in a protective set can only spare an orphan, never kill a live pane.
//
// The three legs are:
//
//   - recorded sessions.tmux_session_name (near-dead by design, kept so a legacy
//     row's pane is never reaped);
//   - recomputed tmux.SessionName / tmux.ChatSessionName for every row;
//   - recorded agent_chats.tmux_session_name.
//
// The recomputed chat leg is the load-bearing one: StartTmuxChat creates the
// pane ~70 lines and one agent launch before it persists the name, so a pane is
// genuinely live with no DB claim for that whole window. Deleting it makes this
// reaper kill panes out from under chats that are still being created.
func (r *TmuxReaper) accountedFor(ctx context.Context) (map[string]struct{}, error) {
	out := map[string]struct{}{}

	recordedSessionNames, err := r.sessions.ListTmuxSessionNames(ctx)
	if err != nil {
		return nil, err
	}
	for _, name := range recordedSessionNames {
		if name != "" {
			out[name] = struct{}{}
		}
	}

	// An empty repoID asks for every session across every repo; every other
	// SessionStore list is repo-scoped.
	sessions, err := r.sessions.List(ctx, "")
	if err != nil {
		return nil, err
	}
	// sessionRepo lets the chat leg recompute a name: a chat row carries its
	// session id, not its repo id.
	sessionRepo := make(map[string]string, len(sessions))
	for _, s := range sessions {
		if s == nil {
			continue
		}
		sessionRepo[s.ID] = s.RepoID
		if s.TmuxSessionName != nil && *s.TmuxSessionName != "" {
			out[*s.TmuxSessionName] = struct{}{}
		}
		if s.RepoID != "" && s.ID != "" {
			out[tmux.SessionName(s.RepoID, s.ID)] = struct{}{}
		}
	}

	chats, err := r.chats.ListRoutableChats(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range chats {
		if c == nil {
			continue
		}
		if c.TmuxSessionName != nil && *c.TmuxSessionName != "" {
			out[*c.TmuxSessionName] = struct{}{}
		}
		repoID := sessionRepo[c.SessionID]
		if repoID != "" && c.AgentSessionID != "" {
			out[tmux.ChatSessionName(repoID, c.AgentSessionID)] = struct{}{}
		}
	}

	return out, nil
}
