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
	reasonOrphanReapDisabled  = "orphanReapDisabled"
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

	// idle carries the second, independently-knobbed reaping path (BOS-886).
	// It is a separate struct rather than more fields on cfg because its
	// defaults are deliberately the inverse of the orphan path's: an idle reap
	// keeps the chat row and is therefore recoverable, so it ships on.
	idle IdleReapDeps

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
	// strikes maps a (session name, reason) pair to the instant that pane was
	// FIRST observed eligible for THAT reason. A candidate is only reaped once
	// the observation is itself at least the grace window old (BOS-846 D5,
	// following BOS-87), and an observation that clears the reason clears the
	// marker. Pruned every sweep for names no longer live — per the BOS-477
	// finding, a per-session map that is never pruned leaks for the daemon's
	// lifetime.
	//
	// The reason is part of the key (BOS-886 D7). Keying by name alone would
	// let a pane's orphan sighting and its idle sighting confirm each other,
	// and the orphan path clears its marker on precisely the branch every
	// accounted-for pane takes — so an idle strike sharing that key could never
	// accumulate at all.
	strikes map[strikeKey]time.Time

	done chan struct{} // closed when Run's goroutine exits
}

// strikeKey identifies one confirmation marker: which pane, and what it was
// seen doing.
type strikeKey struct {
	name   string
	reason string
}

// IdleReapDeps bundles everything the idle-reap path (BOS-886) needs. It is a
// named struct rather than four more positional constructor arguments so the
// wiring site has to say which is which — this path kills panes, and a silently
// transposed argument is the kind of mistake that reaches every install.
//
// Every dependency fails closed when absent: no tracker means no evidence and
// therefore no reap; no oracle means no provable transcript and therefore no
// reap; no kill seam means candidates are logged and left alive.
type IdleReapDeps struct {
	Config      config.TmuxIdleReapConfig
	Tracker     *Tracker
	Transcripts TranscriptOracle
	KillChat    killChatFunc
}

// sweepDecision is one pane's classification, carrying enough context for the
// sweep to route the kill and for an operator to read the log line.
type sweepDecision struct {
	bucket string
	reason string

	// idle marks a decision produced by the idle gate rather than the orphan
	// one. On a reap it selects the chat teardown seam (clear the pane pointer,
	// then kill) over the raw orphan kill; on a keep it is what tells the log
	// which path's dry-run flag and clock actually governed the outcome.
	idle    bool
	owner   *chatOwner
	idleFor time.Duration
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
	idle IdleReapDeps,
	logger zerolog.Logger,
) *TmuxReaper {
	r := &TmuxReaper{
		chats:    chats,
		sessions: sessions,
		repos:    repos,
		tmux:     tmuxClient,
		logger:   logger,
		cfg:      cfg,
		idle:     idle,
		daemonID: daemonID,
		now:      time.Now,
		strikes:  make(map[strikeKey]time.Time),
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
// It sweeps when EITHER path is enabled. The two knobs are independent by
// design (BOS-886 D6), and gating the goroutine on the orphan enable alone
// would have made the on-by-default idle path silently inert on every install
// that never opted into orphan reaping — which is all of them. With both off,
// done closes immediately and nothing is listed: a disabled sweep that still
// asked tmux for its sessions every five minutes would be pure cost.
//
// What D6's independence does NOT cover: the sweep CADENCE. One goroutine
// serving both paths necessarily has one ticker, and it reads
// settings.tmux_reaper.sweep_interval_seconds even on a host where orphan
// reaping is off. That is the one timing an idle-only operator cannot set from
// their own block, and it only ever shifts WHEN a reap is noticed, never
// WHETHER a pane qualifies — every gate that decides that reads the idle block
// or the pane's own clock. Splitting it would mean a second goroutine listing
// the same tmux sessions twice, which is a worse trade than this note.
func (r *TmuxReaper) Run(ctx context.Context) {
	orphanOn, idleOn := r.cfg.IsEnabled(), r.idle.Config.IsEnabled()
	if !orphanOn && !idleOn {
		r.logger.Info().Msg("tmux reaper: disabled (neither settings.tmux_reaper.enabled nor settings.tmux_idle_reap.enabled is on)")
		close(r.done)
		return
	}
	interval := r.cfg.SweepInterval()
	r.logger.Info().
		Dur("sweepInterval", interval).
		Dur("gracePeriod", r.cfg.GracePeriod()).
		Bool("orphanReap", orphanOn).
		Bool("dryRun", r.cfg.IsDryRun()).
		Bool("reapUnstamped", r.cfg.ReapsUnstamped()).
		Bool("idleReap", idleOn).
		Bool("idleDryRun", r.idle.Config.IsDryRun()).
		Dur("idleThreshold", r.idle.Config.IdleThreshold()).
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

	whitelist, owners, err := r.accountedFor(ctx)
	if err != nil {
		r.logger.Warn().Err(err).Msg("tmux reaper: store read failed; sweep aborted with zero kills")
		return 0, err
	}

	now := r.now()
	grace := r.cfg.GracePeriod()
	dryRun := r.cfg.IsDryRun()
	idleDryRun := r.idle.Config.IsDryRun()

	var bossShaped, keep, unattributable, candidates, reaped, idleCandidates, idleReaped int
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
			r.clearStrikes(s.Name)
			continue
		}
		bossShaped++

		dec := r.classify(ctx, s, whitelist, owners, now, grace)
		switch dec.bucket {
		case bucketKeep:
			keep++
			r.decisionLog(r.logger.Debug(), s, dec, dryRun, idleDryRun).
				Msg("tmux reaper: keeping session")
		case bucketUnattributable:
			unattributable++
			r.decisionLog(r.logger.Info(), s, dec, dryRun, idleDryRun).
				Msg("tmux reaper: session is unattributable; leaving it alive")
		case bucketReap:
			if dec.idle {
				idleCandidates++
				if r.reapIdle(ctx, s, dec, idleDryRun) {
					idleReaped++
					reaped++
				}
				continue
			}
			candidates++
			evt := r.logger.Info().
				Str("tmuxSession", s.Name).
				Str("bucket", dec.bucket).
				Str("reason", dec.reason).
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
			r.clearStrike(s.Name, reasonUnaccountedForTwice)
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
		Int("idleCandidates", idleCandidates).
		Int("idleReaped", idleReaped).
		Bool("dryRun", dryRun).
		Bool("idleDryRun", idleDryRun).
		Msg("tmux reaper: sweep complete")

	return reaped, nil
}

// reapIdle performs the destructive half of an idle reap and reports whether
// the pane is actually gone. Every non-kill exit returns false so the candidate
// keeps its confirmation marker and is retried on the next sweep rather than
// being counted as reaped (BOS-886 D8): the guard child reads a pointed chat
// with a dead pane as "the agent exited" and finalizes the session, so a reap
// that half-lands must not be recorded as a success.
func (r *TmuxReaper) reapIdle(ctx context.Context, s tmux.LiveSession, dec sweepDecision, dryRun bool) bool {
	evt := r.logger.Info().
		Str("tmuxSession", s.Name).
		Str("bucket", dec.bucket).
		Str("reason", dec.reason).
		Str("chatId", dec.owner.chat.ID).
		Str("agentSessionId", dec.owner.chat.AgentSessionID).
		Str("sessionId", dec.owner.session.ID).
		Dur("idleFor", dec.idleFor).
		Time("sessionCreated", s.Created).
		Bool("dryRun", dryRun)
	if dryRun {
		evt.Msg("tmux reaper: would reap idle chat pane (dry run)")
		return false
	}
	if r.idle.KillChat == nil {
		evt.Msg("tmux reaper: idle chat pane left alive; no chat teardown seam is wired")
		return false
	}
	// Clear-then-kill lives inside the seam's canonical routine (D9), which
	// restores the pointer if the kill fails, so a failure here means the pane
	// is still up AND still pointed at — the consistent state the next sweep
	// can simply retry. s.Name is passed so the teardown kills the pane THIS
	// sweep resolved rather than one it re-derives from the row.
	if err := r.idle.KillChat(ctx, dec.owner.session.ID, dec.owner.chat.AgentSessionID, s.Name); err != nil {
		evt.Err(err).Msg("tmux reaper: failed to reap idle chat pane; retrying on the next sweep")
		return false
	}
	r.clearStrike(s.Name, reasonIdleTooLong)
	evt.Msg("tmux reaper: reaped idle chat pane")
	return true
}

// decisionLog stamps the fields common to every non-destructive sweep log line
// onto ev.
//
// It exists so the two arms cannot disagree about which path's dry-run flag they
// are reporting. Both buckets now carry decisions from EITHER gate, and a keep
// reading `reason=idleFirstStrike dryRun=false` while
// settings.tmux_idle_reap.dry_run is true states the opposite of the truth to
// the one operator who went looking. The flag follows the gate that produced the
// decision, and idleFor — the observed idle age D10 exists to surface — rides
// along on the lines that have one, so an operator watching a pane age toward a
// reap can see the clock instead of guessing at it.
func (r *TmuxReaper) decisionLog(
	ev *zerolog.Event,
	s tmux.LiveSession,
	dec sweepDecision,
	dryRun, idleDryRun bool,
) *zerolog.Event {
	ev = ev.
		Str("tmuxSession", s.Name).
		Str("bucket", dec.bucket).
		Str("reason", dec.reason).
		Time("sessionCreated", s.Created).
		Bool("idlePath", dec.idle)
	if dec.idle {
		ev = ev.Bool("dryRun", idleDryRun).Dur("idleFor", dec.idleFor)
	} else {
		ev = ev.Bool("dryRun", dryRun)
	}
	return ev
}

// classify places one boss-shaped live session into a bucket. The order of the
// gates is the order of the acceptance criteria, and each one can only ever
// spare a pane.
func (r *TmuxReaper) classify(
	ctx context.Context,
	s tmux.LiveSession,
	whitelist map[string]struct{},
	owners map[string]*chatOwner,
	now time.Time,
	grace time.Duration,
) sweepDecision {
	if _, ok := whitelist[s.Name]; ok {
		// A row accounts for this pane, so the ORPHAN question is settled and
		// its orphan marker is dropped. Before BOS-886 that was the end of it —
		// this branch returned keep unconditionally, and it short-circuited on
		// exactly the set idle-reap targets. It now falls through to the idle
		// gate, which is where the "kept" answer has to be re-earned.
		//
		// Note what this fall-through skips: the tmux.DaemonIDEnvKey stamp
		// check below, which the orphan path calls non-negotiable. That is
		// deliberate, and the asymmetry is in the evidence, not in the rigour.
		// The orphan path kills panes NO row accounts for, a set that by
		// construction contains every pane belonging to every OTHER bossd on
		// the host, so it has nothing but the stamp to tell them apart. The
		// idle path only ever kills a pane THIS daemon's own database
		// positively claims — resolveChatOwners demands exactly one owning chat
		// row — so the claim is the ownership proof, and it is a stronger one
		// than the stamp: a pane that outlived the daemon that stamped it is
		// still ours if our row still points at it, and requiring a stamp match
		// would make idle reaping inert across a daemon restart while adding no
		// protection the row claim does not already give.
		r.clearStrike(s.Name, reasonUnaccountedForTwice)
		return r.classifyIdle(ctx, s, owners, now)
	}

	// Nothing below this line may run with orphan reaping off: Run now also
	// starts for the idle path alone, so an unaccounted-for pane on a host that
	// never opted into orphan reaping must still be kept.
	if !r.cfg.IsEnabled() {
		r.clearStrike(s.Name, reasonUnaccountedForTwice)
		return sweepDecision{bucket: bucketKeep, reason: reasonOrphanReapDisabled}
	}

	// Degenerate case, called out explicitly by D6: a whitelist that came back
	// empty while boss-shaped panes are live is far more likely to be a broken
	// read than a host on which every single pane is an orphan.
	if len(whitelist) == 0 {
		return sweepDecision{bucket: bucketUnattributable, reason: reasonEmptyWhitelist}
	}

	// tmux's own session_created clock (D4). The two alternatives are refuted
	// in-repo: a pipe-pane log mtime never ages while an idle TUI repaints, and
	// agent_chats.created_at is blinded by the create race this reaper exists
	// to cover. A pane is reapable at exactly grace, not a tick later.
	if now.Sub(s.Created) < grace {
		return sweepDecision{bucket: bucketKeep, reason: reasonWithinGrace}
	}

	// Ownership must be proven, not assumed (D7). Two bossd instances on one
	// host have separate databases, so daemon A's whitelist cannot contain
	// daemon B's panes.
	stamp, stamped := r.tmux.ShowEnv(ctx, s.Name, tmux.DaemonIDEnvKey)
	switch {
	case !stamped:
		if !r.cfg.ReapsUnstamped() {
			return sweepDecision{bucket: bucketUnattributable, reason: reasonUnstamped}
		}
	case stamp == "" || stamp != r.daemonID:
		return sweepDecision{bucket: bucketUnattributable, reason: reasonForeignDaemon}
	}

	// Confirmation strike (D5). Age answers "was the pane young?"; it does not
	// answer "was the DB momentarily unable to account for it?" — the shape of
	// BOS-426, where a periodic sweep finalized sessions still being created.
	first, seen := r.recordStrike(s.Name, reasonUnaccountedForTwice, now)
	if !seen {
		return sweepDecision{bucket: bucketKeep, reason: reasonFirstStrike}
	}
	if now.Sub(first) < grace {
		return sweepDecision{bucket: bucketKeep, reason: reasonAwaitingConfirm}
	}

	return sweepDecision{bucket: bucketReap, reason: reasonUnaccountedForTwice}
}

// classifyIdle decides what to do with a pane the database DOES account for
// (BOS-886). Every gate can only spare it relative to the one before, and the
// pane is kept outright unless the idle path is enabled, resolves the name to
// exactly one chat, and finds positive tracker evidence that the chat has been
// sitting idle for the whole window.
func (r *TmuxReaper) classifyIdle(
	ctx context.Context,
	s tmux.LiveSession,
	owners map[string]*chatOwner,
	now time.Time,
) sweepDecision {
	if !r.idle.Config.IsEnabled() {
		// Pre-BOS-886 behaviour, reason string included: accounted for, kept.
		r.clearStrike(s.Name, reasonIdleTooLong)
		return sweepDecision{bucket: bucketKeep, reason: reasonAccountedFor}
	}

	owner := owners[s.Name]
	ev := idleReapEvidence{owner: owner}
	if owner != nil && owner.chat != nil && r.idle.Tracker != nil {
		ev.entry = r.idle.Tracker.Get(owner.chat.AgentSessionID)
	}
	if owner != nil && owner.chat != nil && owner.session != nil && r.idle.Transcripts != nil {
		// Lazy: the predicate only calls this once every cheaper gate has
		// passed, so an idle-reap sweep costs one plugin RPC per genuinely
		// stale chat rather than one per live pane.
		resumeID := owner.chat.AgentSessionID
		if owner.chat.ProviderSessionID != nil && *owner.chat.ProviderSessionID != "" {
			resumeID = *owner.chat.ProviderSessionID
		}
		ev.transcriptPresent = func() bool {
			return r.idle.Transcripts.TranscriptExists(ctx, owner.chat.AgentName, owner.session.WorktreePath, resumeID)
		}
	}

	eligible, reason, idleFor := evaluateIdleReap(ev, now, r.idle.Config.IdleThreshold())
	if !eligible {
		r.clearStrike(s.Name, reasonIdleTooLong)
		return sweepDecision{bucket: bucketKeep, reason: reason, idle: true, idleFor: idleFor}
	}

	// Confirmation strike on the idle path's OWN key (D7). Against an 8-hour
	// window the extra sweep costs nothing, and it is what protects a chat
	// whose tracker entry was momentarily missing or misreported.
	//
	// ONE confirming sighting is the whole gate: a second independent sweep
	// agreeing that the chat is idle is what the strike buys, and the reap
	// lands on that sweep. Do not add an elapsed-time condition here. Aging the
	// strike against the ORPHAN block's grace period — the obvious-looking way
	// to write this — is wrong twice over: it pushes the reap to a third sweep,
	// and it makes an idle-only host's reaping depend on a knob whose
	// documented purpose is the orphan create-race window and whose feature may
	// be switched off entirely (D6: the idle path is knobbed on its own).
	if _, seen := r.recordStrike(s.Name, reasonIdleTooLong, now); !seen {
		return sweepDecision{bucket: bucketKeep, reason: reasonIdleFirstStrike, idle: true, owner: owner, idleFor: idleFor}
	}

	return sweepDecision{bucket: bucketReap, reason: reason, idle: true, owner: owner, idleFor: idleFor}
}

// recordStrike returns the instant name was first observed eligible for reason
// and whether such an observation already existed. The first call for a
// (name, reason) pair stores now and reports seen=false.
func (r *TmuxReaper) recordStrike(name, reason string, now time.Time) (first time.Time, seen bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := strikeKey{name: name, reason: reason}
	if first, ok := r.strikes[key]; ok {
		return first, true
	}
	r.strikes[key] = now
	return now, false
}

// clearStrike drops the confirmation marker for one reason. Called whenever a
// pane stops qualifying for that reason, so a candidate that recovers — a DB
// claim reappears, or the chat produces output — starts from zero rather than
// inheriting a stale strike. Clearing is per-reason so a pane that stops
// looking orphaned keeps whatever idle history it has earned, and vice versa.
func (r *TmuxReaper) clearStrike(name, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.strikes, strikeKey{name: name, reason: reason})
}

// clearStrikes drops every marker for name, whatever the reason. Used on the
// not-boss-owned path, where the pane is out of scope for all of them.
func (r *TmuxReaper) clearStrikes(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key := range r.strikes {
		if key.name == name {
			delete(r.strikes, key)
		}
	}
}

// pruneStrikes drops markers for names tmux no longer reports. Without this the
// map grows for the daemon's lifetime (the BOS-477 finding).
func (r *TmuxReaper) pruneStrikes(liveNames map[string]struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key := range r.strikes {
		if _, ok := liveNames[key.name]; !ok {
			delete(r.strikes, key)
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
// It returns the owner map alongside it (BOS-886), built from the SAME store
// reads so the sweep pays for them once. That map is where the collision
// licence above does not apply and is explicitly withdrawn — see
// resolveChatOwners. Keeping the two side by side is deliberate: the whitelist
// is a protective union, the owner map is an identification map, and every
// future edit has to decide which of the two it is touching.
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
func (r *TmuxReaper) accountedFor(ctx context.Context) (map[string]struct{}, map[string]*chatOwner, error) {
	out := map[string]struct{}{}

	recordedSessionNames, err := r.sessions.ListTmuxSessionNames(ctx)
	if err != nil {
		return nil, nil, err
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
		return nil, nil, err
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
		return nil, nil, err
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

	return out, resolveChatOwners(sessions, chats), nil
}
