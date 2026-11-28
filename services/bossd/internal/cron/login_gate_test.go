package cron

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/kit/clocktest"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/domain"
)

type fakeCronLoginChecker struct {
	mu      sync.Mutex
	calls   int
	lastArg struct {
		agent string
		model string
	}
	fn func(ctx context.Context, agentName, model string) (AgentLogin, error)
}

func (f *fakeCronLoginChecker) CheckAgentLogin(ctx context.Context, agentName, model string) (AgentLogin, error) {
	f.mu.Lock()
	f.calls++
	f.lastArg.agent = agentName
	f.lastArg.model = model
	fn := f.fn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, agentName, model)
	}
	return AgentLogin{Agent: agentName}, nil
}

func (f *fakeCronLoginChecker) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeCronLoginChecker) setFn(fn func(ctx context.Context, agentName, model string) (AgentLogin, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fn = fn
}

func newLoginGateHarness(
	t *testing.T,
	jobs []*domain.CronJob,
	checker AgentLoginChecker,
	clock *clocktest.Clock,
	cfgMut func(*Config),
) (*Scheduler, *fakeStore, *fakeSessionStore, *fakeCreator, *bytes.Buffer) {
	t.Helper()
	repoPath := t.TempDir()
	store := newFakeStore()
	repos := newFakeRepoStore()
	sessions := newFakeSessionStore()
	creator := newFakeCreator()
	for _, job := range jobs {
		store.put(job)
		repos.put(&models.Repo{ID: job.RepoID, DefaultBaseBranch: "main", LocalPath: repoPath})
	}
	var logBuf bytes.Buffer
	cfg := Config{
		Store:         store,
		Sessions:      sessions,
		Repos:         repos,
		Creator:       creator,
		AgentLogin:    checker,
		MaxConcurrent: testMaxConcurrent,
		Logger:        zerolog.New(&logBuf),
		NowFunc:       clock.Now,
	}
	if cfgMut != nil {
		cfgMut(&cfg)
	}
	s := New(cfg)
	for _, job := range jobs {
		if err := s.AddJob(job); err != nil {
			t.Fatalf("AddJob(%s): %v", job.ID, err)
		}
	}
	return s, store, sessions, creator, &logBuf
}

func TestFireBlocksOnLoggedOutAgent(t *testing.T) {
	const remedy = "codex auth required: run `codex login`"
	clock := clocktest.New(time.Unix(1_700_000_000, 0).UTC())
	prevID := "sess-prev"
	job := makeJob("j1", "@every 1m", true)
	job.AgentName = "codex"
	job.ConcurrencyPolicy = models.CronJobConcurrencyPolicyCancelInProgress
	job.LastRunSessionID = &prevID

	checker := &fakeCronLoginChecker{
		fn: func(_ context.Context, agentName, _ string) (AgentLogin, error) {
			return AgentLogin{Agent: agentName, LoggedOut: true, Remedy: remedy}, nil
		},
	}
	var cancelCalls int
	s, store, sessions, creator, logBuf := newLoginGateHarness(t, []*domain.CronJob{job}, checker, clock, func(cfg *Config) {
		cfg.Activity = fakeActivity{active: true}
		cfg.Canceller = RunCancellerFunc(func(_ context.Context, _ string) error {
			cancelCalls++
			return nil
		})
	})
	sessions.put(&models.Session{ID: prevID, State: machine.ImplementingPlan})

	sess, skipped, err := s.fire(context.Background(), job.ID)
	if sess != nil || skipped != "" || err == nil {
		t.Fatalf("fire = (%v, %q, %v), want nil session, empty skip, non-nil error", sess, skipped, err)
	}
	if !strings.Contains(err.Error(), "codex") || !strings.Contains(err.Error(), remedy) {
		t.Fatalf("fire error = %q, want agent name and remedy %q", err.Error(), remedy)
	}
	if n := len(creator.calls); n != 0 {
		t.Fatalf("CreateSession called %d times, want 0", n)
	}
	if cancelCalls != 0 {
		t.Fatalf("CancelRun called %d times, want 0 (logged-out agent must not cancel previous run)", cancelCalls)
	}
	if len(store.lastRunCalls) != 1 {
		t.Fatalf("UpdateLastRun calls = %d, want 1", len(store.lastRunCalls))
	}
	rec := store.lastRunCalls[0]
	if rec.outcome != domain.CronJobOutcomeFireFailed || rec.sessionID != nil || rec.nextRunAt == nil {
		t.Fatalf("recorded %+v, want outcome=fire_failed, nil sessionID, non-nil nextRunAt", rec)
	}
	logged := logBuf.String()
	if !strings.Contains(logged, `"level":"warn"`) || !strings.Contains(logged, "codex") || !strings.Contains(logged, remedy) {
		t.Fatalf("WARN log = %q, want level=warn naming agent and remedy", logged)
	}
}

func TestFireProceedsWhenLoginUnknown(t *testing.T) {
	clock := clocktest.New(time.Unix(1_700_000_000, 0).UTC())
	job := makeJob("j1", "@every 1m", true)
	job.AgentName = "claude"

	checker := &fakeCronLoginChecker{
		fn: func(_ context.Context, agentName, _ string) (AgentLogin, error) {
			return AgentLogin{Agent: agentName, LoggedOut: false}, nil
		},
	}
	s, _, _, creator, _ := newLoginGateHarness(t, []*domain.CronJob{job}, checker, clock, nil)

	sess, skipped, err := s.fire(context.Background(), job.ID)
	if err != nil || skipped != "" || sess == nil {
		t.Fatalf("fire = (%v, %q, %v), want session", sess, skipped, err)
	}
	if n := len(creator.calls); n != 1 {
		t.Fatalf("CreateSession calls = %d, want 1", n)
	}
}

func TestFireProceedsOnLoginCheckError(t *testing.T) {
	clock := clocktest.New(time.Unix(1_700_000_000, 0).UTC())
	job := makeJob("j1", "@every 1m", true)
	job.AgentName = "codex"
	job.ConcurrencyPolicy = models.CronJobConcurrencyPolicyAllowConcurrent

	checker := &fakeCronLoginChecker{
		fn: func(_ context.Context, _, _ string) (AgentLogin, error) {
			return AgentLogin{}, errors.New("plugin unavailable")
		},
	}
	s, _, _, creator, _ := newLoginGateHarness(t, []*domain.CronJob{job}, checker, clock, nil)

	for i := 1; i <= 2; i++ {
		sess, skipped, err := s.fire(context.Background(), job.ID)
		if err != nil || skipped != "" || sess == nil {
			t.Fatalf("fire #%d = (%v, %q, %v), want session", i, sess, skipped, err)
		}
	}
	if n := len(creator.calls); n != 2 {
		t.Fatalf("CreateSession calls = %d, want 2", n)
	}
	if got := checker.callCount(); got != 2 {
		t.Fatalf("checker calls = %d, want 2 (errors must not be cached)", got)
	}
}

func TestFireWithoutLoginChecker(t *testing.T) {
	clock := clocktest.New(time.Unix(1_700_000_000, 0).UTC())
	job := makeJob("j1", "@every 1m", true)
	job.AgentName = "codex"

	s, _, _, creator, _ := newLoginGateHarness(t, []*domain.CronJob{job}, nil, clock, nil)

	sess, skipped, err := s.fire(context.Background(), job.ID)
	if err != nil || skipped != "" || sess == nil {
		t.Fatalf("fire = (%v, %q, %v), want session", sess, skipped, err)
	}
	if n := len(creator.calls); n != 1 {
		t.Fatalf("CreateSession calls = %d, want 1", n)
	}
}

func TestLoginGateCachesPerAgent(t *testing.T) {
	clock := clocktest.New(time.Unix(1_700_000_000, 0).UTC())
	jobCodex := makeJob("codex-job", "@every 1m", true)
	jobCodex.AgentName = "codex"
	jobCodex.ConcurrencyPolicy = models.CronJobConcurrencyPolicyAllowConcurrent

	jobClaude := makeJob("claude-job", "@every 1m", true)
	jobClaude.AgentName = "claude"
	jobClaude.ConcurrencyPolicy = models.CronJobConcurrencyPolicyAllowConcurrent

	jobDefault := makeJob("default-job", "@every 1m", true)
	jobDefault.AgentName = ""
	jobDefault.ConcurrencyPolicy = models.CronJobConcurrencyPolicyAllowConcurrent

	checker := &fakeCronLoginChecker{
		fn: func(_ context.Context, agentName, _ string) (AgentLogin, error) {
			resolved := agentName
			if resolved == "" {
				resolved = "codex"
			}
			return AgentLogin{Agent: resolved, LoggedOut: false}, nil
		},
	}
	s, _, _, _, _ := newLoginGateHarness(t, []*domain.CronJob{jobCodex, jobClaude, jobDefault}, checker, clock, nil)

	if sess, _, err := s.fire(context.Background(), jobCodex.ID); err != nil || sess == nil {
		t.Fatalf("codex fire 1: (%v, %v)", sess, err)
	}
	if got := checker.callCount(); got != 1 {
		t.Fatalf("calls after codex fire 1 = %d, want 1", got)
	}

	clock.Advance(4 * time.Minute)
	if sess, _, err := s.fire(context.Background(), jobCodex.ID); err != nil || sess == nil {
		t.Fatalf("codex fire 2: (%v, %v)", sess, err)
	}
	if got := checker.callCount(); got != 1 {
		t.Fatalf("calls after codex fire 2 (4m) = %d, want 1", got)
	}

	if sess, _, err := s.fire(context.Background(), jobClaude.ID); err != nil || sess == nil {
		t.Fatalf("claude fire 1: (%v, %v)", sess, err)
	}
	if got := checker.callCount(); got != 2 {
		t.Fatalf("calls after claude fire 1 = %d, want 2", got)
	}

	if sess, _, err := s.fire(context.Background(), jobDefault.ID); err != nil || sess == nil {
		t.Fatalf("default fire 1: (%v, %v)", sess, err)
	}
	if got := checker.callCount(); got != 3 {
		t.Fatalf("calls after default fire 1 = %d, want 3", got)
	}
	if sess, _, err := s.fire(context.Background(), jobDefault.ID); err != nil || sess == nil {
		t.Fatalf("default fire 2: (%v, %v)", sess, err)
	}
	if got := checker.callCount(); got != 3 {
		t.Fatalf("calls after default fire 2 = %d, want 3", got)
	}

	clock.Advance(loginCacheTTL + time.Second)
	if sess, _, err := s.fire(context.Background(), jobCodex.ID); err != nil || sess == nil {
		t.Fatalf("codex fire after TTL: (%v, %v)", sess, err)
	}
	if got := checker.callCount(); got != 4 {
		t.Fatalf("calls after TTL expiry = %d, want 4", got)
	}
}

func TestGatedFireSkipsLoginCheck(t *testing.T) {
	clock := clocktest.New(time.Unix(1_700_000_000, 0).UTC())
	job := makeJob("gated-job", "@every 1m", true)
	job.AgentName = "codex"
	job.GateCommand = "exit 1"

	checker := &fakeCronLoginChecker{
		fn: func(_ context.Context, agentName, _ string) (AgentLogin, error) {
			return AgentLogin{Agent: agentName, LoggedOut: true, Remedy: "run `codex login`"}, nil
		},
	}
	s, store, _, creator, _ := newLoginGateHarness(t, []*domain.CronJob{job}, checker, clock, nil)

	sess, skipped, err := s.fire(context.Background(), job.ID)
	if err != nil || sess != nil || skipped != SkipReasonGated {
		t.Fatalf("fire = (%v, %q, %v), want skip %q", sess, skipped, err, SkipReasonGated)
	}
	if got := checker.callCount(); got != 0 {
		t.Fatalf("login checker calls = %d, want 0 when gate command blocks", got)
	}
	if n := len(creator.calls); n != 0 {
		t.Fatalf("CreateSession calls = %d, want 0", n)
	}
	if len(store.lastRunCalls) != 1 || store.lastRunCalls[0].outcome != domain.CronJobOutcomeGated {
		t.Fatalf("lastRunCalls = %+v, want single gated outcome", store.lastRunCalls)
	}
}

func TestRunNowRechecksLogin(t *testing.T) {
	const remedy = "run `claude auth login`"
	clock := clocktest.New(time.Unix(1_700_000_000, 0).UTC())
	job := makeJob("j1", "@every 1m", true)
	job.AgentName = "claude"
	job.ConcurrencyPolicy = models.CronJobConcurrencyPolicyAllowConcurrent

	checker := &fakeCronLoginChecker{
		fn: func(_ context.Context, agentName, _ string) (AgentLogin, error) {
			return AgentLogin{Agent: agentName, LoggedOut: true, Remedy: remedy}, nil
		},
	}
	s, _, _, creator, _ := newLoginGateHarness(t, []*domain.CronJob{job}, checker, clock, nil)

	if _, _, err := s.fire(context.Background(), job.ID); err == nil {
		t.Fatalf("scheduled fire 1 unexpectedly succeeded while logged out")
	}
	if _, _, err := s.fire(context.Background(), job.ID); err == nil {
		t.Fatalf("scheduled fire 2 unexpectedly succeeded while logged out")
	}
	if got := checker.callCount(); got != 1 {
		t.Fatalf("calls after two scheduled fires = %d, want 1", got)
	}

	checker.setFn(func(_ context.Context, agentName, _ string) (AgentLogin, error) {
		return AgentLogin{Agent: agentName, LoggedOut: false}, nil
	})
	sess, skipped, err := s.RunNow(context.Background(), job.ID)
	if err != nil || skipped != "" || sess == nil {
		t.Fatalf("RunNow = (%v, %q, %v), want session after operator login", sess, skipped, err)
	}
	if got := checker.callCount(); got != 2 {
		t.Fatalf("calls after RunNow = %d, want 2", got)
	}

	sess, skipped, err = s.fire(context.Background(), job.ID)
	if err != nil || skipped != "" || sess == nil {
		t.Fatalf("scheduled fire after RunNow = (%v, %q, %v), want session from refreshed cache", sess, skipped, err)
	}
	if got := checker.callCount(); got != 2 {
		t.Fatalf("calls after post-RunNow scheduled fire = %d, want 2 (refreshed cache hit)", got)
	}
	if n := len(creator.calls); n != 2 {
		t.Fatalf("CreateSession calls = %d, want 2", n)
	}
}

func TestLoginGate_ScheduledJobs_AC271001(t *testing.T) {
	const remedy = "run `claude auth login`"
	clock := clocktest.New(time.Unix(1_700_000_000, 0).UTC())
	job := makeJob("ac-job", "@every 1m", true)
	job.AgentName = "claude"
	job.ConcurrencyPolicy = models.CronJobConcurrencyPolicyAllowConcurrent

	checker := &fakeCronLoginChecker{
		fn: func(_ context.Context, agentName, _ string) (AgentLogin, error) {
			return AgentLogin{Agent: agentName, LoggedOut: true, Remedy: remedy}, nil
		},
	}
	s, store, _, creator, _ := newLoginGateHarness(t, []*domain.CronJob{job}, checker, clock, nil)

	sess, _, err := s.fire(context.Background(), job.ID)
	if sess != nil || err == nil || !strings.Contains(err.Error(), remedy) {
		t.Fatalf("logged-out fire = (%v, %v), want nil session and error naming %q", sess, err, remedy)
	}
	if len(creator.calls) != 0 {
		t.Fatalf("CreateSession calls = %d, want 0", len(creator.calls))
	}
	if len(store.lastRunCalls) != 1 || store.lastRunCalls[0].outcome != domain.CronJobOutcomeFireFailed {
		t.Fatalf("lastRunCalls = %+v, want fire_failed", store.lastRunCalls)
	}

	clock.Advance(loginCacheTTL + time.Second)
	checker.setFn(func(_ context.Context, agentName, _ string) (AgentLogin, error) {
		return AgentLogin{Agent: agentName, LoggedOut: false}, nil
	})
	sess, skipped, err := s.fire(context.Background(), job.ID)
	if err != nil || skipped != "" || sess == nil {
		t.Fatalf("unknown-login fire = (%v, %q, %v), want session", sess, skipped, err)
	}
	if len(creator.calls) != 1 {
		t.Fatalf("CreateSession calls = %d, want 1", len(creator.calls))
	}
}
