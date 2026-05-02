// Package cron runs scheduled prompt jobs. The scheduler loads enabled jobs
// from the CronJobStore at startup, registers each with a robfig/cron/v3
// Cron instance, and on each tick spawns a session (via the task orchestrator's
// SessionCreator) scoped to the owning cron job.
//
// Concurrency: simultaneous fires are capped by a counting semaphore (default
// 3); extra fires block until a slot frees up. Stop(ctx) drains the cron-managed
// goroutines plus any direct fire/RunNow calls before returning.
//
// Overlap suppression: at each fire, the scheduler checks cron_jobs.last_run_session_id.
// If the previous session is still in a non-terminal state and not archived,
// the fire is skipped. last_run_session_id is persisted on every successful
// CreateSession via MarkFireStarted; last_run_outcome is written later by the
// finalize path (flight leg 4).
package cron

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/cronutil"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/taskorchestrator"
)

// DefaultMaxConcurrent is the default cap on simultaneous cron fires.
const DefaultMaxConcurrent = 3

// Config bundles Scheduler dependencies. Store and Sessions are used for
// load/overlap checks; Repos resolves the per-job base branch; Creator spawns
// the actual session.
type Config struct {
	Store         db.CronJobStore
	Sessions      db.SessionStore
	Repos         db.RepoStore
	Creator       taskorchestrator.SessionCreator
	MaxConcurrent int
	Logger        zerolog.Logger

	// NowFunc, if non-nil, overrides time.Now for fire timestamps and
	// next-run computations. Tests inject a fixed clock here.
	NowFunc func() time.Time
}

// Scheduler runs scheduled prompt jobs.
type Scheduler struct {
	store    db.CronJobStore
	sessions db.SessionStore
	repos    db.RepoStore
	creator  taskorchestrator.SessionCreator
	logger   zerolog.Logger

	cron *cron.Cron
	sem  chan struct{}

	// entriesMu guards entries; each entry carries both the cron EntryID
	// (for removal) and the parsed schedule (for computing next_run_at
	// independently of the cron runner's internal state).
	entriesMu sync.Mutex
	entries   map[string]scheduledEntry

	// wg tracks in-flight fires (cron-driven and direct). Stop waits on
	// this in addition to cron.Stop()'s own context.
	wg sync.WaitGroup

	nowFunc func() time.Time
}

type scheduledEntry struct {
	entryID cron.EntryID
	sched   cron.Schedule
	fn      func() // the raw fire func, stored so Tick can invoke it synchronously
	jobID   string
	lastRun time.Time // last time Tick fired this entry (zero = never)
}

// New constructs a Scheduler. If cfg.MaxConcurrent <= 0 it falls back to
// DefaultMaxConcurrent. The returned scheduler is not started until Start.
func New(cfg Config) *Scheduler {
	maxC := cfg.MaxConcurrent
	if maxC <= 0 {
		maxC = DefaultMaxConcurrent
	}
	nowFn := cfg.NowFunc
	if nowFn == nil {
		nowFn = time.Now
	}
	logger := cfg.Logger.With().Str("component", "cron-scheduler").Logger()
	return &Scheduler{
		store:    cfg.Store,
		sessions: cfg.Sessions,
		repos:    cfg.Repos,
		creator:  cfg.Creator,
		logger:   logger,
		cron: cron.New(
			cron.WithLocation(time.UTC),
			cron.WithChain(cron.Recover(cronLogger{logger})),
		),
		sem:     make(chan struct{}, maxC),
		entries: make(map[string]scheduledEntry),
		nowFunc: nowFn,
	}
}

// Start loads all enabled cron jobs, registers each, and starts the cron
// runner. Jobs with invalid schedules are logged and skipped; they do not
// abort startup.
func (s *Scheduler) Start(ctx context.Context) error {
	jobs, err := s.store.ListEnabled(ctx)
	if err != nil {
		return fmt.Errorf("list enabled cron jobs: %w", err)
	}
	for _, job := range jobs {
		if err := s.AddJob(job); err != nil {
			s.logger.Warn().Err(err).
				Str("cron_job_id", job.ID).
				Str("schedule", job.Schedule).
				Msg("skipping cron job with invalid schedule")
			continue
		}
	}
	s.cron.Start()
	s.logger.Info().Int("loaded", len(s.entries)).Msg("cron scheduler started")
	return nil
}

// Stop shuts down the cron runner and waits for all in-flight fires to
// finish. Returns ctx.Err() on deadline; a successful drain returns nil.
func (s *Scheduler) Stop(ctx context.Context) error {
	cronCtx := s.cron.Stop()

	select {
	case <-cronCtx.Done():
	case <-ctx.Done():
		return ctx.Err()
	}

	// Also wait for direct fire/RunNow invocations that weren't dispatched
	// by the cron runner.
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// AddJob registers job with the cron runner. Returns an error if the schedule
// or timezone is invalid; the job is not registered in that case.
func (s *Scheduler) AddJob(job *models.CronJob) error {
	sched, err := s.buildSchedule(job)
	if err != nil {
		return err
	}

	id := job.ID
	fn := func() { s.fireJob(id) }
	entryID := s.cron.Schedule(sched, cron.FuncJob(fn))

	s.entriesMu.Lock()
	s.entries[id] = scheduledEntry{entryID: entryID, sched: sched, fn: fn, jobID: id}
	s.entriesMu.Unlock()

	s.logger.Info().
		Str("cron_job_id", id).
		Str("name", job.Name).
		Str("schedule", job.Schedule).
		Msg("registered cron job")
	return nil
}

// RemoveJob deregisters the cron job with the given id. A missing id is a
// no-op (logged at debug level).
func (s *Scheduler) RemoveJob(id string) {
	s.entriesMu.Lock()
	entry, ok := s.entries[id]
	if ok {
		delete(s.entries, id)
	}
	s.entriesMu.Unlock()

	if !ok {
		s.logger.Debug().Str("cron_job_id", id).Msg("remove: no such cron job registered")
		return
	}
	s.cron.Remove(entry.entryID)
	s.logger.Info().Str("cron_job_id", id).Msg("deregistered cron job")
}

// UpdateJob re-registers the job. Equivalent to RemoveJob + AddJob; kept as a
// single entry point so callers can swap in a new schedule/timezone atomically
// from their point of view.
func (s *Scheduler) UpdateJob(job *models.CronJob) error {
	s.RemoveJob(job.ID)
	return s.AddJob(job)
}

// RunNow fires the given job synchronously, bypassing the schedule. Returns
// the created session on success, a non-empty skippedReason if the fire was
// skipped (job disabled or previous run still active), or an error if the
// fire failed outright. Honors the concurrency cap.
//
// The caller's ctx is intentionally NOT propagated into the spawn pipeline:
// claude.Start binds the spawned process to its ctx via cmd.Cancel, so a
// gRPC request ctx (which cancels as soon as RunCronJobNow returns) would
// SIGTERM Claude before the prompt finishes. fireJob uses context.Background()
// for the same reason — RunNow now matches that. Stop(ctx) still drains the
// in-flight fires via the WaitGroup, so daemon shutdown remains bounded.
func (s *Scheduler) RunNow(ctx context.Context, jobID string) (*models.Session, string, error) {
	// Honor an already-cancelled caller ctx so we don't kick off work after
	// the request gave up — but otherwise detach.
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	return s.fire(context.Background(), jobID)
}

// Tick is a test-only deterministic clock driver. It iterates over every
// registered job, computes the next fire time from the job's last Tick fire
// (zero = epoch), and invokes the job function synchronously (not in a
// goroutine) for every job whose next scheduled time is ≤ now.
//
// Tick does NOT interact with robfig/cron's internal timer loop; it is a
// parallel execution path used exclusively in tests to drive jobs forward
// without sleeping. The concurrency semaphore and overlap-skip logic in
// fire() are still honoured because Tick calls fireJob → fire.
//
// Calling Tick on a Scheduler that has been Start()-ed is safe but unusual;
// in tests you will typically skip Start and drive time manually via Tick.
func (s *Scheduler) Tick(now time.Time) {
	s.entriesMu.Lock()
	// Snapshot the entries so we don't hold the lock while calling fn (which
	// may itself re-acquire the lock via peekNextFire).
	type snap struct {
		jobID   string
		sched   cron.Schedule
		fn      func()
		lastRun time.Time
	}
	snaps := make([]snap, 0, len(s.entries))
	for _, e := range s.entries {
		snaps = append(snaps, snap{
			jobID:   e.jobID,
			sched:   e.sched,
			fn:      e.fn,
			lastRun: e.lastRun,
		})
	}
	s.entriesMu.Unlock()

	for _, sn := range snaps {
		next := sn.sched.Next(sn.lastRun)
		if !next.IsZero() && !next.After(now) {
			// Update lastRun before calling fn so re-entrant Tick calls don't
			// fire the same job twice.
			s.entriesMu.Lock()
			if e, ok := s.entries[sn.jobID]; ok {
				e.lastRun = now
				s.entries[sn.jobID] = e
			}
			s.entriesMu.Unlock()

			sn.fn()
		}
	}
}

// buildSchedule parses the job's schedule and wraps it in a timezone-aware
// shim so the cron runner (which is UTC-based) still fires at the correct
// wall-clock time in the job's IANA zone.
func (s *Scheduler) buildSchedule(job *models.CronJob) (cron.Schedule, error) {
	inner, err := cronutil.Parse(job.Schedule)
	if err != nil {
		return nil, fmt.Errorf("parse schedule %q: %w", job.Schedule, err)
	}
	tzName := ""
	if job.Timezone != nil {
		tzName = *job.Timezone
	}
	loc, err := cronutil.ResolveTimezone(tzName)
	if err != nil {
		return nil, fmt.Errorf("resolve timezone %q: %w", tzName, err)
	}
	return &tzSchedule{inner: inner, loc: loc}, nil
}

// tzSchedule wraps a cron.Schedule so Next is evaluated at wall-clock time in
// the supplied location. The returned Time inherits that location; cron
// compares it against its own UTC clock and wall-clock semantics are
// preserved regardless of the cron instance's configured zone.
type tzSchedule struct {
	inner cron.Schedule
	loc   *time.Location
}

func (s *tzSchedule) Next(t time.Time) time.Time {
	return cronutil.NextAt(s.inner, t, s.loc)
}

// fireJob is the cron runner entry point. It delegates to fire and swallows
// the result — cron has no channel for it. Logs any skip/error.
func (s *Scheduler) fireJob(jobID string) {
	ctx := context.Background()
	if _, reason, err := s.fire(ctx, jobID); err != nil {
		s.logger.Error().Err(err).
			Str("cron_job_id", jobID).
			Msg("scheduled fire failed")
	} else if reason != "" {
		s.logger.Info().
			Str("cron_job_id", jobID).
			Str("reason", reason).
			Msg("scheduled fire skipped")
	}
}

// fire acquires a concurrency slot, performs the freshness + overlap checks,
// and asks the session creator to spawn a cron-scoped session. Returns the
// created session, a non-empty skippedReason, or an error.
//
// Skip reasons (non-error skips): "disabled", "db_fetch_error", "overlap_prev_active".
func (s *Scheduler) fire(ctx context.Context, jobID string) (*models.Session, string, error) {
	s.wg.Add(1)
	defer s.wg.Done()

	// Concurrency cap. Block until a slot frees up, honoring ctx.
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return nil, "", ctx.Err()
	}

	logger := s.logger.With().Str("cron_job_id", jobID).Logger()

	// Re-fetch: the job may have been disabled/deleted/re-scheduled between
	// the tick and this fire.
	job, err := s.store.Get(ctx, jobID)
	if err != nil {
		logger.Warn().Err(err).Msg("fire: could not fetch cron job; skipping")
		return nil, "db_fetch_error", nil
	}
	if !job.Enabled {
		logger.Info().Msg("fire: job disabled between tick and fire; skipping")
		return nil, "disabled", nil
	}
	if reason, active := s.previousRunActive(ctx, job); active {
		logger.Info().
			Str("reason", reason).
			Str("last_run_session_id", strOrEmpty(job.LastRunSessionID)).
			Msg("fire: previous run still active; skipping")
		return nil, reason, nil
	}

	// Generate a per-fire hook token so the Stop hook can authenticate back
	// to the daemon's loopback listener (wired in flight leg 5).
	token, err := generateHookToken()
	if err != nil {
		logger.Error().Err(err).Msg("fire: failed to generate hook token")
		return nil, "", fmt.Errorf("generate hook token: %w", err)
	}

	// Resolve the base branch from the repo. The cron path bypasses the
	// server's CreateSession handler (which defaults this), so we apply the
	// same fallback here. Without it, lifecycle.StartSession runs
	// `git worktree add ... origin/` against an empty ref and fails.
	repo, err := s.repos.Get(ctx, job.RepoID)
	if err != nil {
		logger.Error().Err(err).Msg("fire: failed to load repo for base branch")
		if markErr := s.markFireFailed(ctx, job); markErr != nil {
			logger.Warn().Err(markErr).Msg("fire: also failed to mark outcome=fire_failed")
		}
		return nil, "", fmt.Errorf("load repo for cron job %s: %w", job.ID, err)
	}

	opts := taskorchestrator.CreateSessionOpts{
		RepoID:     job.RepoID,
		Title:      job.Name,
		Plan:       job.Prompt,
		BaseBranch: repo.DefaultBaseBranch,
		// Per-fire branch name. Without this, every fire of the same cron
		// job tries to create the same branch (e.g. cron-test) and the
		// second fire trips ErrBranchExists once the first run's branch
		// is left behind by a PR, an archived orphan, or a SIGTERM'd
		// session. Including the unix timestamp keeps consecutive fires
		// (≥1 minute apart by cron's smallest granularity) collision-free.
		BranchName: cronBranchName(job.Name, s.nowFunc()),
		CronJobID:  job.ID,
		DeferPR:    true,
		HookToken:  token,
	}

	sess, err := s.creator.CreateSession(ctx, opts)
	if err != nil {
		logger.Error().Err(err).Msg("fire: session create failed")
		if markErr := s.markFireFailed(ctx, job); markErr != nil {
			logger.Warn().Err(markErr).Msg("fire: also failed to mark outcome=fire_failed")
		}
		return nil, "", fmt.Errorf("create session for cron job %s: %w", job.ID, err)
	}

	// Persist last_run_session_id so the next tick's overlap check sees
	// this session as the current run. next_run_at is computed from the
	// parsed schedule so the DB reflects the runner's next decision.
	firedAt := s.nowFunc()
	nextAt := s.peekNextFire(jobID, firedAt)
	var nextArg *time.Time
	if !nextAt.IsZero() {
		nextArg = &nextAt
	}
	if markErr := s.store.MarkFireStarted(ctx, job.ID, sess.ID, firedAt, nextArg); markErr != nil {
		// Non-fatal: the session is already spawned. Overlap detection
		// will fall back to whatever last_run_session_id was previously,
		// which is stale but safe (worst case: a duplicate fire).
		logger.Warn().Err(markErr).
			Str("session_id", sess.ID).
			Msg("fire: MarkFireStarted failed; overlap check for next tick may be stale")
	}

	logger.Info().
		Str("session_id", sess.ID).
		Str("name", job.Name).
		Msg("fire: cron session spawned")
	return sess, "", nil
}

// peekNextFire returns the next scheduled time for jobID strictly after `from`,
// computed from the registered schedule. Returns the zero Time if the job is
// not registered (e.g. removed between CreateSession and this call).
func (s *Scheduler) peekNextFire(jobID string, from time.Time) time.Time {
	s.entriesMu.Lock()
	entry, ok := s.entries[jobID]
	s.entriesMu.Unlock()
	if !ok {
		return time.Time{}
	}
	return entry.sched.Next(from)
}

// previousRunActive reports whether the job's most recent session is still
// in a non-terminal, non-archived state. A missing last_run_session_id or a
// deleted session row are both treated as "not active".
func (s *Scheduler) previousRunActive(ctx context.Context, job *models.CronJob) (string, bool) {
	if job.LastRunSessionID == nil || *job.LastRunSessionID == "" {
		return "", false
	}
	sess, err := s.sessions.Get(ctx, *job.LastRunSessionID)
	if err != nil {
		// Session row is gone (cleanup outcome, manual delete). Safe to fire.
		return "", false
	}
	if sess.ArchivedAt != nil || isTerminalState(sess.State) {
		return "", false
	}
	return "overlap_prev_active", true
}

// markFireFailed records last_run_outcome = fire_failed when CreateSession
// returns an error. next_run_at is cleared so the DB reflects "no run was
// scheduled"; the cron runner will still tick on its own schedule.
func (s *Scheduler) markFireFailed(ctx context.Context, job *models.CronJob) error {
	return s.store.UpdateLastRun(ctx, job.ID, db.UpdateCronJobLastRunParams{
		RanAt:   s.nowFunc(),
		Outcome: models.CronJobOutcomeFireFailed,
	})
}

// isTerminalState reports whether a session has reached the end of its
// lifecycle (Merged or Closed). Anything else counts as "still active" for
// overlap-check purposes.
func isTerminalState(st machine.State) bool {
	return st == machine.Merged || st == machine.Closed
}

func strOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// cronBranchSlugRE strips characters that aren't safe in a git ref. Mirrors
// the slugging in services/bossd/internal/git/worktree.go but lives here so
// the cron path can pre-compute the branch and feed it through CreateOpts.
var cronBranchSlugRE = regexp.MustCompile(`[^a-z0-9]+`)

// cronBranchName returns a branch name unique to this fire. Format:
//
//	cron-<slug>-<unix>
//
// Cron's smallest granularity is one minute, so a unix-second suffix is
// always unique across consecutive fires of the same job. Even successful
// runs that leave their branch behind (PR not yet merged) won't block the
// next fire, and SIGTERM'd / archived orphans can't trip ErrBranchExists.
func cronBranchName(jobName string, now time.Time) string {
	slug := strings.ToLower(strings.TrimSpace(jobName))
	slug = cronBranchSlugRE.ReplaceAllString(slug, "-")
	slug = strings.Trim(slug, "-")
	if slug == "" {
		slug = "job"
	}
	// Truncate the slug so the final branch (slug + "-" + 10-digit unix +
	// "cron-" prefix) stays well under git's 255-byte ref limit.
	const maxSlug = 40
	if len(slug) > maxSlug {
		slug = strings.TrimRight(slug[:maxSlug], "-")
	}
	return fmt.Sprintf("cron-%s-%d", slug, now.Unix())
}

// generateHookToken returns a 32-byte hex string (64 hex chars) used by the
// finalize Stop hook to authenticate back to the daemon.
func generateHookToken() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// cronLogger bridges robfig/cron's logger interface to zerolog so recovered
// panics and internal warnings surface in the daemon's normal log stream.
type cronLogger struct {
	l zerolog.Logger
}

func (c cronLogger) Info(msg string, keysAndValues ...any) {
	c.l.Info().Fields(keysAndValues).Msg("cron: " + msg)
}

func (c cronLogger) Error(err error, msg string, keysAndValues ...any) {
	c.l.Error().Err(err).Fields(keysAndValues).Msg("cron: " + msg)
}
