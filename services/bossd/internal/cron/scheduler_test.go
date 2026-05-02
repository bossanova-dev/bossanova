package cron

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/taskorchestrator"
)

// --- Mocks ---------------------------------------------------------------

// fakeStore is a thin in-memory CronJobStore. Only the methods the Scheduler
// touches are implemented; the rest panic (unused in tests).
type fakeStore struct {
	mu          sync.Mutex
	jobs        map[string]*models.CronJob
	listEnabled func(ctx context.Context) ([]*models.CronJob, error) // optional override

	markStartedCalls []markStartedCall
	lastRunCalls     []lastRunCall
}

type markStartedCall struct {
	id        string
	sessionID string
	firedAt   time.Time
	nextRunAt *time.Time
}

type lastRunCall struct {
	id      string
	outcome models.CronJobOutcome
}

func newFakeStore() *fakeStore {
	return &fakeStore{jobs: map[string]*models.CronJob{}}
}

func (f *fakeStore) put(job *models.CronJob) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs[job.ID] = job
}

func (f *fakeStore) Get(ctx context.Context, id string) (*models.CronJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[id]
	if !ok {
		return nil, sql.ErrNoRows
	}
	clone := *j
	return &clone, nil
}

func (f *fakeStore) ListEnabled(ctx context.Context) ([]*models.CronJob, error) {
	if f.listEnabled != nil {
		return f.listEnabled(ctx)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*models.CronJob
	for _, j := range f.jobs {
		if j.Enabled {
			clone := *j
			out = append(out, &clone)
		}
	}
	return out, nil
}

func (f *fakeStore) MarkFireStarted(ctx context.Context, id string, sessionID string, firedAt time.Time, nextRunAt *time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.markStartedCalls = append(f.markStartedCalls, markStartedCall{
		id: id, sessionID: sessionID, firedAt: firedAt, nextRunAt: nextRunAt,
	})
	if j, ok := f.jobs[id]; ok {
		sid := sessionID
		j.LastRunSessionID = &sid
		if nextRunAt != nil {
			na := *nextRunAt
			j.NextRunAt = &na
		} else {
			j.NextRunAt = nil
		}
	}
	return nil
}

func (f *fakeStore) UpdateLastRun(ctx context.Context, id string, params db.UpdateCronJobLastRunParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastRunCalls = append(f.lastRunCalls, lastRunCall{id: id, outcome: params.Outcome})
	if j, ok := f.jobs[id]; ok {
		o := params.Outcome
		j.LastRunOutcome = &o
	}
	return nil
}

// Unused store methods — panic loudly so a regression in the scheduler that
// starts calling them is obvious in test output.
func (f *fakeStore) Create(ctx context.Context, p db.CreateCronJobParams) (*models.CronJob, error) {
	panic("not used")
}
func (f *fakeStore) List(ctx context.Context) ([]*models.CronJob, error) { panic("not used") }
func (f *fakeStore) ListByRepo(ctx context.Context, repoID string) ([]*models.CronJob, error) {
	panic("not used")
}
func (f *fakeStore) Update(ctx context.Context, id string, p db.UpdateCronJobParams) (*models.CronJob, error) {
	panic("not used")
}
func (f *fakeStore) Delete(ctx context.Context, id string) error { panic("not used") }

// fakeRepoStore is a minimal RepoStore used only by fire() to look up the
// repo's DefaultBaseBranch. Get returns a repo with DefaultBaseBranch="main"
// for any unknown ID so existing tests don't have to seed each repo by hand.
type fakeRepoStore struct {
	mu    sync.Mutex
	repos map[string]*models.Repo
}

func newFakeRepoStore() *fakeRepoStore {
	return &fakeRepoStore{repos: map[string]*models.Repo{}}
}

func (f *fakeRepoStore) put(repo *models.Repo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repos[repo.ID] = repo
}

func (f *fakeRepoStore) Get(ctx context.Context, id string) (*models.Repo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.repos[id]; ok {
		clone := *r
		return &clone, nil
	}
	return &models.Repo{ID: id, DefaultBaseBranch: "main"}, nil
}

func (f *fakeRepoStore) Create(ctx context.Context, p db.CreateRepoParams) (*models.Repo, error) {
	panic("not used")
}
func (f *fakeRepoStore) GetByPath(ctx context.Context, path string) (*models.Repo, error) {
	panic("not used")
}
func (f *fakeRepoStore) List(ctx context.Context) ([]*models.Repo, error) { panic("not used") }
func (f *fakeRepoStore) Update(ctx context.Context, id string, p db.UpdateRepoParams) (*models.Repo, error) {
	panic("not used")
}
func (f *fakeRepoStore) Delete(ctx context.Context, id string) error { panic("not used") }

// fakeSessionStore is a minimal SessionStore used only by previousRunActive.
type fakeSessionStore struct {
	mu       sync.Mutex
	sessions map[string]*models.Session
	getErr   error // force every Get to return this error
}

func newFakeSessionStore() *fakeSessionStore {
	return &fakeSessionStore{sessions: map[string]*models.Session{}}
}

func (f *fakeSessionStore) put(sess *models.Session) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions[sess.ID] = sess
}

func (f *fakeSessionStore) Get(ctx context.Context, id string) (*models.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	s, ok := f.sessions[id]
	if !ok {
		return nil, sql.ErrNoRows
	}
	return s, nil
}

// Stub out the rest of SessionStore. Scheduler only calls Get.
func (f *fakeSessionStore) Create(ctx context.Context, p db.CreateSessionParams) (*models.Session, error) {
	panic("not used")
}
func (f *fakeSessionStore) List(ctx context.Context, repoID string) ([]*models.Session, error) {
	panic("not used")
}
func (f *fakeSessionStore) ListActive(ctx context.Context, repoID string) ([]*models.Session, error) {
	panic("not used")
}
func (f *fakeSessionStore) ListActiveWithRepo(ctx context.Context, repoID string) ([]*db.SessionWithRepo, error) {
	panic("not used")
}
func (f *fakeSessionStore) ListWithRepo(ctx context.Context, repoID string) ([]*db.SessionWithRepo, error) {
	panic("not used")
}
func (f *fakeSessionStore) ListArchived(ctx context.Context, repoID string) ([]*models.Session, error) {
	panic("not used")
}
func (f *fakeSessionStore) Update(ctx context.Context, id string, p db.UpdateSessionParams) (*models.Session, error) {
	panic("not used")
}
func (f *fakeSessionStore) Archive(ctx context.Context, id string) error   { panic("not used") }
func (f *fakeSessionStore) Resurrect(ctx context.Context, id string) error { panic("not used") }
func (f *fakeSessionStore) Delete(ctx context.Context, id string) error    { panic("not used") }
func (f *fakeSessionStore) AdvanceOrphanedSessions(ctx context.Context) (int64, error) {
	panic("not used")
}
func (f *fakeSessionStore) UpdateStateConditional(_ context.Context, _ string, _, _ int) (bool, error) {
	panic("not used")
}
func (f *fakeSessionStore) ListByState(_ context.Context, _ int) ([]*models.Session, error) {
	panic("not used")
}

// fakeCreator is a SessionCreator mock. Each call optionally blocks on a
// gate (for concurrency-cap tests) and can be configured to error.
type fakeCreator struct {
	mu    sync.Mutex
	calls []taskorchestrator.CreateSessionOpts
	err   error

	// gate, if non-nil, is read from at the start of every CreateSession
	// call. Send one value to release one call; close to release all.
	gate chan struct{}

	// inFlight counts concurrently active CreateSession calls.
	inFlight    atomic.Int32
	maxInFlight atomic.Int32
	entered     atomic.Int32 // number of calls that made it past the gate
	enterCh     chan struct{}
}

func newFakeCreator() *fakeCreator {
	return &fakeCreator{enterCh: make(chan struct{}, 128)}
}

func (f *fakeCreator) CreateSession(ctx context.Context, opts taskorchestrator.CreateSessionOpts) (*models.Session, error) {
	// Track entry (past the sem acquire, at the top of CreateSession) so
	// concurrency tests can distinguish "waiting on sem" from "waiting on
	// the gate we control". inFlight mirrors entry → exit.
	f.entered.Add(1)
	cur := f.inFlight.Add(1)
	for {
		m := f.maxInFlight.Load()
		if cur <= m || f.maxInFlight.CompareAndSwap(m, cur) {
			break
		}
	}
	defer f.inFlight.Add(-1)

	select {
	case f.enterCh <- struct{}{}:
	default:
	}

	// Gate, if set, blocks until closed or signaled. Used by cap + Stop tests
	// to freeze callers in CreateSession and observe state.
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	f.mu.Lock()
	f.calls = append(f.calls, opts)
	id := fmt.Sprintf("sess-%d", len(f.calls))
	f.mu.Unlock()

	if f.err != nil {
		return nil, f.err
	}
	return &models.Session{ID: id, RepoID: opts.RepoID, Title: opts.Title}, nil
}

// --- Helpers -------------------------------------------------------------

// testMaxConcurrent is the cap used by all tests that don't explicitly want
// to probe the semaphore. Kept above 1 so happy-path tests can't accidentally
// serialise on the sem.
const testMaxConcurrent = 3

func newTestScheduler(t *testing.T, store *fakeStore, sessions *fakeSessionStore, creator *fakeCreator) *Scheduler {
	t.Helper()
	return newTestSchedulerWithRepos(t, store, sessions, newFakeRepoStore(), creator)
}

func newTestSchedulerWithRepos(t *testing.T, store *fakeStore, sessions *fakeSessionStore, repos *fakeRepoStore, creator *fakeCreator) *Scheduler {
	t.Helper()
	cfg := Config{
		Store:         store,
		Sessions:      sessions,
		Repos:         repos,
		Creator:       creator,
		MaxConcurrent: testMaxConcurrent,
		Logger:        zerolog.New(io.Discard),
		NowFunc:       func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}
	return New(cfg)
}

func makeJob(id, schedule string, enabled bool) *models.CronJob {
	return &models.CronJob{
		ID:       id,
		RepoID:   "repo-1",
		Name:     "job-" + id,
		Prompt:   "do the thing",
		Schedule: schedule,
		Enabled:  enabled,
	}
}

// --- Start loads jobs ----------------------------------------------------

func TestStart_LoadsEnabledSkipsDisabled(t *testing.T) {
	store := newFakeStore()
	store.put(makeJob("a", "@every 1m", true))
	store.put(makeJob("b", "@every 1m", false)) // disabled
	store.put(makeJob("c", "@every 1m", true))

	s := newTestScheduler(t, store, newFakeSessionStore(), newFakeCreator())
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = s.Stop(context.Background()) }()

	s.entriesMu.Lock()
	defer s.entriesMu.Unlock()
	if len(s.entries) != 2 {
		t.Errorf("entries = %d, want 2 (a + c)", len(s.entries))
	}
	if _, ok := s.entries["a"]; !ok {
		t.Error("missing entry for a")
	}
	if _, ok := s.entries["b"]; ok {
		t.Error("disabled job b should not be registered")
	}
	if _, ok := s.entries["c"]; !ok {
		t.Error("missing entry for c")
	}
}

func TestStart_BadScheduleLogsAndSkips(t *testing.T) {
	store := newFakeStore()
	store.put(makeJob("ok1", "@every 1m", true))
	store.put(makeJob("bad", "not-a-cron", true)) // invalid parser will reject
	store.put(makeJob("ok2", "@every 1h", true))

	s := newTestScheduler(t, store, newFakeSessionStore(), newFakeCreator())
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = s.Stop(context.Background()) }()

	s.entriesMu.Lock()
	defer s.entriesMu.Unlock()
	if len(s.entries) != 2 {
		t.Errorf("entries = %d, want 2 (ok1 + ok2, bad skipped)", len(s.entries))
	}
	if _, ok := s.entries["bad"]; ok {
		t.Error("bad job should not be registered")
	}
}

func TestStart_ListEnabledError(t *testing.T) {
	store := newFakeStore()
	store.listEnabled = func(ctx context.Context) ([]*models.CronJob, error) {
		return nil, errors.New("db down")
	}
	s := newTestScheduler(t, store, newFakeSessionStore(), newFakeCreator())
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("Start: want error from failed ListEnabled, got nil")
	}
}

// --- Add / Remove / Update round-trips -----------------------------------

func TestAddRemoveUpdateRoundTrip(t *testing.T) {
	store := newFakeStore()
	s := newTestScheduler(t, store, newFakeSessionStore(), newFakeCreator())

	job := makeJob("x", "@every 1m", true)
	if err := s.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	s.entriesMu.Lock()
	firstID := s.entries["x"].entryID
	s.entriesMu.Unlock()

	// Update swaps the registration — new EntryID is expected.
	if err := s.UpdateJob(job); err != nil {
		t.Fatalf("UpdateJob: %v", err)
	}
	s.entriesMu.Lock()
	updatedID := s.entries["x"].entryID
	s.entriesMu.Unlock()
	if firstID == updatedID {
		t.Errorf("UpdateJob should assign a new EntryID (got %d unchanged)", firstID)
	}

	s.RemoveJob("x")
	s.entriesMu.Lock()
	_, ok := s.entries["x"]
	s.entriesMu.Unlock()
	if ok {
		t.Error("RemoveJob: entry still present")
	}

	// Remove a missing id: no-op, no panic.
	s.RemoveJob("missing")
}

func TestAddJob_InvalidSchedule(t *testing.T) {
	store := newFakeStore()
	s := newTestScheduler(t, store, newFakeSessionStore(), newFakeCreator())

	if err := s.AddJob(makeJob("bad", "not-a-schedule", true)); err == nil {
		t.Error("AddJob: want error for invalid schedule, got nil")
	}
	s.entriesMu.Lock()
	defer s.entriesMu.Unlock()
	if _, ok := s.entries["bad"]; ok {
		t.Error("AddJob should not register a job with an invalid schedule")
	}
}

func TestAddJob_InvalidTimezone(t *testing.T) {
	store := newFakeStore()
	s := newTestScheduler(t, store, newFakeSessionStore(), newFakeCreator())

	tz := "Not/A/Zone"
	job := makeJob("tz", "@every 1m", true)
	job.Timezone = &tz
	if err := s.AddJob(job); err == nil {
		t.Error("AddJob: want error for bogus timezone, got nil")
	}
}

// --- fireJob branches ----------------------------------------------------

func TestFire_Happy(t *testing.T) {
	store := newFakeStore()
	store.put(makeJob("j", "@every 1m", true))
	sessions := newFakeSessionStore()
	creator := newFakeCreator()
	s := newTestScheduler(t, store, sessions, creator)
	if err := s.AddJob(store.jobs["j"]); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	sess, skipped, err := s.fire(context.Background(), "j")
	if err != nil {
		t.Fatalf("fire: %v", err)
	}
	if skipped != "" {
		t.Errorf("skipped = %q, want empty", skipped)
	}
	if sess == nil {
		t.Fatal("fire returned nil session")
	}
	if len(creator.calls) != 1 {
		t.Fatalf("creator calls = %d, want 1", len(creator.calls))
	}
	opts := creator.calls[0]
	if opts.CronJobID != "j" {
		t.Errorf("opts.CronJobID = %q, want j", opts.CronJobID)
	}
	if !opts.DeferPR {
		t.Error("opts.DeferPR = false, want true")
	}
	if opts.HookToken == "" {
		t.Error("opts.HookToken is empty")
	}
	if opts.Plan != "do the thing" {
		t.Errorf("opts.Plan = %q, want 'do the thing'", opts.Plan)
	}
	if opts.Title != "job-j" {
		t.Errorf("opts.Title = %q, want %q (job name only — no 'cron: ' prefix)", opts.Title, "job-j")
	}
	if opts.BaseBranch != "main" {
		t.Errorf("opts.BaseBranch = %q, want 'main' (resolved from repo.DefaultBaseBranch)", opts.BaseBranch)
	}
	// Branch must be unique per fire (cron-<slug>-<unix>) so a leftover
	// branch from a SIGTERM'd / archived orphan can't trip ErrBranchExists.
	wantBranch := fmt.Sprintf("cron-job-j-%d", time.Unix(1700000000, 0).UTC().Unix())
	if opts.BranchName != wantBranch {
		t.Errorf("opts.BranchName = %q, want %q (cron-<slug>-<unix>)", opts.BranchName, wantBranch)
	}

	if len(store.markStartedCalls) != 1 {
		t.Fatalf("MarkFireStarted calls = %d, want 1", len(store.markStartedCalls))
	}
	mc := store.markStartedCalls[0]
	if mc.id != "j" || mc.sessionID != sess.ID {
		t.Errorf("MarkFireStarted call = %+v, want id=j sessionID=%s", mc, sess.ID)
	}
	if mc.nextRunAt == nil {
		t.Error("MarkFireStarted next_run_at was nil; scheduler should persist next tick")
	}
}

func TestFire_DisabledBetweenTickAndFire(t *testing.T) {
	store := newFakeStore()
	store.put(makeJob("j", "@every 1m", false))
	creator := newFakeCreator()
	s := newTestScheduler(t, store, newFakeSessionStore(), creator)

	sess, skipped, err := s.fire(context.Background(), "j")
	if err != nil {
		t.Fatalf("fire: unexpected error %v", err)
	}
	if skipped != "disabled" {
		t.Errorf("skipped = %q, want 'disabled'", skipped)
	}
	if sess != nil {
		t.Error("sess should be nil when fire is skipped")
	}
	if len(creator.calls) != 0 {
		t.Errorf("creator was called %d times; should not fire when disabled", len(creator.calls))
	}
}

func TestFire_DBFetchError(t *testing.T) {
	store := newFakeStore() // job "missing" never put
	creator := newFakeCreator()
	s := newTestScheduler(t, store, newFakeSessionStore(), creator)

	_, skipped, err := s.fire(context.Background(), "missing")
	if err != nil {
		t.Fatalf("fire: unexpected error %v", err)
	}
	if skipped != "db_fetch_error" {
		t.Errorf("skipped = %q, want 'db_fetch_error'", skipped)
	}
	if len(creator.calls) != 0 {
		t.Error("creator should not be called on DB fetch error")
	}
}

func TestFire_PreviousRunActive(t *testing.T) {
	store := newFakeStore()
	job := makeJob("j", "@every 1m", true)
	prev := "sess-prev"
	job.LastRunSessionID = &prev
	store.put(job)

	sessions := newFakeSessionStore()
	sessions.put(&models.Session{ID: prev, State: machine.ImplementingPlan}) // non-terminal

	creator := newFakeCreator()
	s := newTestScheduler(t, store, sessions, creator)

	_, skipped, err := s.fire(context.Background(), "j")
	if err != nil {
		t.Fatalf("fire: unexpected error %v", err)
	}
	if skipped != "overlap_prev_active" {
		t.Errorf("skipped = %q, want 'overlap_prev_active'", skipped)
	}
	if len(creator.calls) != 0 {
		t.Error("creator should not be called when previous run is still active")
	}
}

func TestFire_PreviousRunTerminal_ProceedsToFire(t *testing.T) {
	store := newFakeStore()
	job := makeJob("j", "@every 1m", true)
	prev := "sess-prev"
	job.LastRunSessionID = &prev
	store.put(job)

	sessions := newFakeSessionStore()
	sessions.put(&models.Session{ID: prev, State: machine.Merged}) // terminal

	creator := newFakeCreator()
	s := newTestScheduler(t, store, sessions, creator)

	_, skipped, err := s.fire(context.Background(), "j")
	if err != nil {
		t.Fatalf("fire: %v", err)
	}
	if skipped != "" {
		t.Errorf("skipped = %q, want empty (previous run is terminal)", skipped)
	}
	if len(creator.calls) != 1 {
		t.Errorf("creator calls = %d, want 1", len(creator.calls))
	}
}

func TestFire_PreviousSessionDeleted_ProceedsToFire(t *testing.T) {
	store := newFakeStore()
	job := makeJob("j", "@every 1m", true)
	prev := "sess-gone"
	job.LastRunSessionID = &prev
	store.put(job)

	// Empty SessionStore — Get returns sql.ErrNoRows.
	creator := newFakeCreator()
	s := newTestScheduler(t, store, newFakeSessionStore(), creator)

	_, skipped, err := s.fire(context.Background(), "j")
	if err != nil {
		t.Fatalf("fire: %v", err)
	}
	if skipped != "" {
		t.Errorf("skipped = %q, want empty (session was deleted)", skipped)
	}
}

// TestCronBranchName covers the slug + unix-suffix contract. Each test case
// is a regression guard for a specific class of branch-collision bug.
func TestCronBranchName(t *testing.T) {
	now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		jobName  string
		wantHead string // expected prefix; suffix is "-<unix>"
	}{
		{"plain", "Cron test", "cron-cron-test"},
		{"already-prefixed", "cron-test", "cron-cron-test"}, // doubled prefix is OK — uniqueness comes from the unix suffix
		{"empty", "", "cron-job"},
		{"unicode_and_punct", "Daily!! report — 2026", "cron-daily-report-2026"},
		{"long_slug", strings.Repeat("a", 200), "cron-" + strings.Repeat("a", 40)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := cronBranchName(c.jobName, now)
			wantSuffix := fmt.Sprintf("-%d", now.Unix())
			if !strings.HasSuffix(got, wantSuffix) {
				t.Errorf("cronBranchName(%q) = %q; want suffix %q", c.jobName, got, wantSuffix)
			}
			head := strings.TrimSuffix(got, wantSuffix)
			if head != c.wantHead {
				t.Errorf("cronBranchName(%q) head = %q, want %q", c.jobName, head, c.wantHead)
			}
		})
	}

	// Two fires one second apart must produce distinct branches. Cron's
	// smallest granularity is 1 minute so this gap is more than enough.
	first := cronBranchName("Cron test", now)
	second := cronBranchName("Cron test", now.Add(time.Second))
	if first == second {
		t.Errorf("consecutive fires produced identical branch %q — branch uniqueness is the whole point", first)
	}
}

// TestFire_BaseBranchFromRepoDefault is the regression guard for the
// real-world failure where the cron scheduler called CreateSession with
// BaseBranch="", causing `git worktree add ... origin/` to fail. The cron
// path bypasses the server's CreateSession defaulting, so fire() must look
// the value up itself.
func TestFire_BaseBranchFromRepoDefault(t *testing.T) {
	store := newFakeStore()
	store.put(makeJob("j", "@every 1m", true))

	repos := newFakeRepoStore()
	repos.put(&models.Repo{ID: "repo-1", DefaultBaseBranch: "develop"})

	creator := newFakeCreator()
	s := newTestSchedulerWithRepos(t, store, newFakeSessionStore(), repos, creator)
	if err := s.AddJob(store.jobs["j"]); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	if _, _, err := s.fire(context.Background(), "j"); err != nil {
		t.Fatalf("fire: %v", err)
	}
	if len(creator.calls) != 1 {
		t.Fatalf("creator calls = %d, want 1", len(creator.calls))
	}
	if got := creator.calls[0].BaseBranch; got != "develop" {
		t.Errorf("BaseBranch = %q, want 'develop' (from repo.DefaultBaseBranch)", got)
	}
}

// TestFire_RepoFetchError_MarksFireFailed covers the case where the repo
// row has been deleted between job creation and fire. The session must not
// be spawned with an empty BaseBranch; instead the outcome should be
// fire_failed so the operator can investigate.
func TestFire_RepoFetchError_MarksFireFailed(t *testing.T) {
	store := newFakeStore()
	store.put(makeJob("j", "@every 1m", true))

	// fakeRepoStore.Get falls back to a synthetic repo for unknown IDs, so
	// inject a real error via a custom store.
	repos := &erroringRepoStore{err: errors.New("repo gone")}

	creator := newFakeCreator()
	s := newTestSchedulerWithRepos(t, store, newFakeSessionStore(), &fakeRepoStore{}, creator)
	s.repos = repos // swap in the erroring store

	if _, _, err := s.fire(context.Background(), "j"); err == nil {
		t.Fatal("fire: want error when repo fetch fails, got nil")
	}
	if len(creator.calls) != 0 {
		t.Errorf("creator should not be called when repo fetch fails (got %d)", len(creator.calls))
	}
	if len(store.lastRunCalls) != 1 {
		t.Fatalf("UpdateLastRun calls = %d, want 1", len(store.lastRunCalls))
	}
	if store.lastRunCalls[0].outcome != models.CronJobOutcomeFireFailed {
		t.Errorf("outcome = %q, want %q",
			store.lastRunCalls[0].outcome, models.CronJobOutcomeFireFailed)
	}
}

// erroringRepoStore is a RepoStore stub whose Get always returns err.
type erroringRepoStore struct{ err error }

func (e *erroringRepoStore) Get(ctx context.Context, id string) (*models.Repo, error) {
	return nil, e.err
}
func (e *erroringRepoStore) Create(ctx context.Context, p db.CreateRepoParams) (*models.Repo, error) {
	panic("not used")
}
func (e *erroringRepoStore) GetByPath(ctx context.Context, path string) (*models.Repo, error) {
	panic("not used")
}
func (e *erroringRepoStore) List(ctx context.Context) ([]*models.Repo, error) { panic("not used") }
func (e *erroringRepoStore) Update(ctx context.Context, id string, p db.UpdateRepoParams) (*models.Repo, error) {
	panic("not used")
}
func (e *erroringRepoStore) Delete(ctx context.Context, id string) error { panic("not used") }

func TestFire_SessionCreateError_MarksFireFailed(t *testing.T) {
	store := newFakeStore()
	store.put(makeJob("j", "@every 1m", true))
	creator := newFakeCreator()
	creator.err = errors.New("worktree: disk full")

	s := newTestScheduler(t, store, newFakeSessionStore(), creator)
	_, _, err := s.fire(context.Background(), "j")
	if err == nil {
		t.Fatal("fire: want error when CreateSession fails, got nil")
	}
	if len(store.lastRunCalls) != 1 {
		t.Fatalf("UpdateLastRun calls = %d, want 1", len(store.lastRunCalls))
	}
	if store.lastRunCalls[0].outcome != models.CronJobOutcomeFireFailed {
		t.Errorf("outcome = %q, want %q",
			store.lastRunCalls[0].outcome, models.CronJobOutcomeFireFailed)
	}
	if len(store.markStartedCalls) != 0 {
		t.Error("MarkFireStarted should not be called when CreateSession fails")
	}
}

// --- Concurrency cap -----------------------------------------------------

func TestConcurrencyCap(t *testing.T) {
	store := newFakeStore()
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("j%d", i)
		store.put(makeJob(id, "@every 1m", true))
	}
	creator := newFakeCreator()
	creator.gate = make(chan struct{})

	const cap = testMaxConcurrent
	s := newTestScheduler(t, store, newFakeSessionStore(), creator)

	// Launch 5 fires concurrently.
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("j%d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = s.fire(context.Background(), id)
		}()
	}

	// Wait until exactly `cap` callers have entered CreateSession. The
	// other two must be blocked on the semaphore, not on the creator gate.
	deadline := time.Now().Add(2 * time.Second)
	for creator.inFlight.Load() < cap && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := creator.inFlight.Load(); got != cap {
		t.Fatalf("in-flight = %d, want %d while sem is saturated", got, cap)
	}
	// Guarantee the other two truly are blocked: hold for a beat and recheck.
	time.Sleep(50 * time.Millisecond)
	if got := creator.inFlight.Load(); got != cap {
		t.Fatalf("in-flight rose to %d past cap %d before gate opened", got, cap)
	}
	if got := creator.entered.Load(); got != cap {
		t.Fatalf("entered = %d, want %d (remaining 2 should be blocked on sem)", got, cap)
	}

	// Release all, let the queued fires drain.
	close(creator.gate)
	wg.Wait()

	if got := creator.maxInFlight.Load(); got > cap {
		t.Errorf("max in-flight = %d, exceeded cap %d", got, cap)
	}
	if got := creator.entered.Load(); got != 5 {
		t.Errorf("total entered = %d, want 5", got)
	}
}

// --- Stop waits for in-flight --------------------------------------------

func TestStop_WaitsForInFlight(t *testing.T) {
	store := newFakeStore()
	store.put(makeJob("j", "@every 1m", true))
	creator := newFakeCreator()
	creator.gate = make(chan struct{})

	s := newTestScheduler(t, store, newFakeSessionStore(), creator)

	// Start a direct fire in a goroutine — it will block in CreateSession.
	fireDone := make(chan struct{})
	go func() {
		_, _, _ = s.fire(context.Background(), "j")
		close(fireDone)
	}()

	// Wait until the fire is actually inside CreateSession.
	select {
	case <-creator.enterCh:
	case <-time.After(time.Second):
		t.Fatal("CreateSession never called")
	}

	// Stop with a generous deadline must not return until the fire returns.
	stopDone := make(chan error, 1)
	go func() {
		stopDone <- s.Stop(context.Background())
	}()

	select {
	case err := <-stopDone:
		t.Fatalf("Stop returned before in-flight fire completed: err=%v", err)
	case <-time.After(50 * time.Millisecond):
	}

	// Release the fire; Stop should complete shortly after.
	close(creator.gate)
	<-fireDone
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Stop did not return after fire released")
	}
}

func TestStop_ContextDeadlineWhileInFlight(t *testing.T) {
	store := newFakeStore()
	store.put(makeJob("j", "@every 1m", true))
	creator := newFakeCreator()
	creator.gate = make(chan struct{}) // never closed

	s := newTestScheduler(t, store, newFakeSessionStore(), creator)
	go func() {
		_, _, _ = s.fire(context.Background(), "j")
	}()
	<-creator.enterCh // ensure the fire is actually in flight

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := s.Stop(ctx); err == nil {
		t.Error("Stop: want context-deadline error, got nil")
	}

	// Cleanup: release the in-flight call so the test goroutine exits.
	close(creator.gate)
}

// --- RunNow --------------------------------------------------------------

func TestRunNow_Happy(t *testing.T) {
	store := newFakeStore()
	store.put(makeJob("j", "@every 1m", true))
	creator := newFakeCreator()
	s := newTestScheduler(t, store, newFakeSessionStore(), creator)

	sess, skipped, err := s.RunNow(context.Background(), "j")
	if err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	if skipped != "" {
		t.Errorf("skipped = %q, want empty", skipped)
	}
	if sess == nil {
		t.Fatal("RunNow returned nil session on happy path")
	}
}

// TestRunNow_DetachesCallerContext is the regression guard for the
// real-world failure where RunNow propagated the caller's gRPC request ctx
// into claude.Start. As soon as the gRPC handler returned, the request ctx
// cancelled and Claude was SIGTERM'd before the prompt finished. RunNow
// must shield the spawn pipeline from caller-cancellation; only an
// already-cancelled ctx is honored (as a sanity check).
func TestRunNow_DetachesCallerContext(t *testing.T) {
	store := newFakeStore()
	store.put(makeJob("j", "@every 1m", true))
	creator := newFakeCreator()
	creator.gate = make(chan struct{})

	s := newTestScheduler(t, store, newFakeSessionStore(), creator)

	callerCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type result struct {
		sess *models.Session
		err  error
	}
	done := make(chan result, 1)
	go func() {
		sess, _, err := s.RunNow(callerCtx, "j")
		done <- result{sess: sess, err: err}
	}()

	// Wait until CreateSession is actually waiting on the gate.
	select {
	case <-creator.enterCh:
	case <-time.After(time.Second):
		t.Fatal("CreateSession was never reached")
	}

	// Cancel the caller's ctx. If RunNow propagated it, the gate select in
	// fakeCreator.CreateSession would observe ctx.Done() and return early
	// with an error. With the detach in place, the call stays parked.
	cancel()

	select {
	case r := <-done:
		t.Fatalf("RunNow returned after caller cancel — ctx leaked into the spawn pipeline: sess=%v err=%v", r.sess, r.err)
	case <-time.After(80 * time.Millisecond):
	}

	// Release the gate; the spawn must complete successfully despite the
	// earlier cancel.
	close(creator.gate)
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("RunNow: unexpected error after gate release: %v", r.err)
		}
		if r.sess == nil {
			t.Fatal("RunNow returned nil session on happy path after gate release")
		}
	case <-time.After(time.Second):
		t.Fatal("RunNow did not return after gate release")
	}
}

// TestRunNow_AlreadyCancelledCtx checks the sanity-check at the top of
// RunNow: a caller that's already given up shouldn't kick off new work.
func TestRunNow_AlreadyCancelledCtx(t *testing.T) {
	store := newFakeStore()
	store.put(makeJob("j", "@every 1m", true))
	creator := newFakeCreator()
	s := newTestScheduler(t, store, newFakeSessionStore(), creator)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the call

	if _, _, err := s.RunNow(ctx, "j"); err == nil {
		t.Fatal("RunNow with pre-cancelled ctx: want error, got nil")
	}
	if len(creator.calls) != 0 {
		t.Errorf("creator was called %d times for a pre-cancelled RunNow; should be 0", len(creator.calls))
	}
}

func TestRunNow_OverlapSkip(t *testing.T) {
	store := newFakeStore()
	job := makeJob("j", "@every 1m", true)
	prev := "sess-prev"
	job.LastRunSessionID = &prev
	store.put(job)

	sessions := newFakeSessionStore()
	sessions.put(&models.Session{ID: prev, State: machine.AwaitingChecks})

	creator := newFakeCreator()
	s := newTestScheduler(t, store, sessions, creator)

	_, skipped, err := s.RunNow(context.Background(), "j")
	if err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	if skipped != "overlap_prev_active" {
		t.Errorf("skipped = %q, want 'overlap_prev_active'", skipped)
	}
}
