package cron

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/kit/clocktest"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/adapter/github"
	"github.com/recurser/bossd/internal/domain"
)

const quotaTestOrigin = "https://github.com/acme/widgets.git"

// fakeQuota answers QuotaStatus from a fixed reading and counts the reads.
type fakeQuota struct {
	mu     sync.Mutex
	quotas []vcs.Quota
	err    error
	calls  int
}

func (f *fakeQuota) QuotaStatus(context.Context) ([]vcs.Quota, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.quotas, f.err
}

func (f *fakeQuota) reads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// quotasAt is a reading with core healthy and graphql at remaining/5000.
func quotasAt(graphQLRemaining int) []vcs.Quota {
	return []vcs.Quota{
		{Name: "core", Limit: 5000, Remaining: 4900},
		{Name: "graphql", Limit: 5000, Remaining: graphQLRemaining},
	}
}

func TestQuotaBelowFloorAtBothThresholds(t *testing.T) {
	for _, tt := range []struct {
		name   string
		quotas []vcs.Quota
		floor  float64
		want   string // the quota below the floor, "" when the job may fire
	}{
		{"standard: graphql exactly at 35%", quotasAt(1750), 0.35, ""},
		{"standard: graphql just under 35%", quotasAt(1749), 0.35, "graphql"},
		{"standard: core just under 35%", []vcs.Quota{{Name: "core", Limit: 5000, Remaining: 1749}, {Name: "graphql", Limit: 5000, Remaining: 5000}}, 0.35, "core"},
		{"standard: core exactly at 35%", []vcs.Quota{{Name: "core", Limit: 5000, Remaining: 1750}, {Name: "graphql", Limit: 5000, Remaining: 5000}}, 0.35, ""},
		{"reserve: graphql exactly at 10%", quotasAt(500), 0.10, ""},
		{"reserve: graphql just under 10%", quotasAt(499), 0.10, "graphql"},
		{"reserve: core just under 10%", []vcs.Quota{{Name: "core", Limit: 5000, Remaining: 499}, {Name: "graphql", Limit: 5000, Remaining: 5000}}, 0.10, "core"},
		{"reserve: 20% left", quotasAt(1000), 0.10, ""},
		{"no usable limit", []vcs.Quota{{Name: "graphql", Limit: 0, Remaining: 0}}, 0.35, ""},
		{"empty reading", nil, 0.35, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			q, low := quotaBelowFloor(tt.quotas, tt.floor)
			if got := map[bool]string{true: q.Name}[low]; got != tt.want {
				t.Fatalf("quotaBelowFloor = (%q, %v), want %q", q.Name, low, tt.want)
			}
		})
	}
}

// quotaScheduler builds a scheduler for one GitHub repo whose gate, when set,
// records BOSS_GITHUB_QUOTA_FLOOR into floor.txt in the repo.
func quotaScheduler(t *testing.T, job *domain.CronJob, origin string, quota *fakeQuota) (*Scheduler, *fakeStore, *fakeCreator, string) {
	t.Helper()
	repoPath := t.TempDir()
	if job.GateCommand != "" {
		script := "#!/bin/sh\nprintf '%s' \"$BOSS_GITHUB_QUOTA_FLOOR\" > floor.txt\n"
		if err := os.WriteFile(filepath.Join(repoPath, "floor.sh"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	store := newFakeStore()
	store.put(job)
	repos := newFakeRepoStore()
	repos.put(&models.Repo{ID: job.RepoID, DefaultBaseBranch: "main", LocalPath: repoPath, OriginURL: origin})
	creator := newFakeCreator()
	s := newTestSchedulerWithRepos(t, store, newFakeSessionStore(), repos, creator)
	if quota != nil {
		s.quota = quota
	}
	if err := s.AddJob(job); err != nil {
		t.Fatal(err)
	}
	return s, store, creator, repoPath
}

func sweepJob() *domain.CronJob {
	job := makeJob("sweep", "@every 1m", true)
	job.Prompt = "Tidy the docs"
	return job
}

// verifyJob is a verify-stage cron identified by its prompt, whose gate
// is the floor-recording script quotaScheduler writes.
func verifyJob() *domain.CronJob {
	job := makeJob("verify", "@every 1m", true)
	job.Prompt = "/boss-verify"
	job.GateCommand = "./floor.sh"
	return job
}

func TestFireLeavesQuotaForVerifyBelowStandardFloor_ScheduledJobs_AC220301(t *testing.T) {
	quota := &fakeQuota{quotas: quotasAt(1000)} // 20% of graphql left
	s, store, creator, _ := quotaScheduler(t, sweepJob(), quotaTestOrigin, quota)

	sess, skipped, err := s.fire(context.Background(), "sweep")
	if err != nil || sess != nil || skipped != SkipReasonQuotaReserved {
		t.Fatalf("fire = (%v, %q, %v), want skip %q", sess, skipped, err, SkipReasonQuotaReserved)
	}
	if n := len(creator.calls); n != 0 {
		t.Fatalf("CreateSession called %d times for a quota-reserved fire", n)
	}
	if len(store.lastRunCalls) != 1 {
		t.Fatalf("UpdateLastRun calls = %d, want 1", len(store.lastRunCalls))
	}
	rec := store.lastRunCalls[0]
	if rec.outcome != domain.CronJobOutcomeGated || rec.sessionID != nil || rec.nextRunAt == nil {
		t.Fatalf("recorded %+v, want outcome gated, no session, next_run_at advanced", rec)
	}
}

func TestFireRunsVerifyBetweenReserveAndStandardFloor_ScheduledJobs_AC220301(t *testing.T) {
	quota := &fakeQuota{quotas: quotasAt(1000)} // 20% of graphql left
	s, _, creator, repoPath := quotaScheduler(t, verifyJob(), quotaTestOrigin, quota)

	sess, skipped, err := s.fire(context.Background(), "verify")
	if err != nil || skipped != "" || sess == nil {
		t.Fatalf("fire = (%v, %q, %v), want a verify session at 20%% quota", sess, skipped, err)
	}
	if n := len(creator.calls); n != 1 {
		t.Fatalf("CreateSession called %d times, want 1", n)
	}
	got, err := os.ReadFile(filepath.Join(repoPath, "floor.txt"))
	if err != nil {
		t.Fatalf("gate did not run: %v", err)
	}
	if string(got) != "0.1" {
		t.Fatalf("gate BOSS_GITHUB_QUOTA_FLOOR = %q, want %q", got, "0.1")
	}
}

func TestFireReservesVerifyBelowReserveFloor(t *testing.T) {
	quota := &fakeQuota{quotas: quotasAt(250)} // 5% of graphql left
	s, _, creator, repoPath := quotaScheduler(t, verifyJob(), quotaTestOrigin, quota)

	if _, skipped, err := s.fire(context.Background(), "verify"); err != nil || skipped != SkipReasonQuotaReserved {
		t.Fatalf("fire = (%q, %v), want skip %q", skipped, err, SkipReasonQuotaReserved)
	}
	if n := len(creator.calls); n != 0 {
		t.Fatalf("CreateSession called %d times below the reserve floor", n)
	}
	if _, err := os.Stat(filepath.Join(repoPath, "floor.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("gate ran below the reserve floor (stat err %v)", err)
	}
}

func TestStandardGateEnvNamesStandardFloor(t *testing.T) {
	job := sweepJob()
	job.GateCommand = "./floor.sh"
	s, _, _, repoPath := quotaScheduler(t, job, quotaTestOrigin, &fakeQuota{quotas: quotasAt(5000)})

	if sess, skipped, err := s.fire(context.Background(), "sweep"); err != nil || sess == nil {
		t.Fatalf("fire = (%v, %q, %v), want a session with a full quota", sess, skipped, err)
	}
	got, err := os.ReadFile(filepath.Join(repoPath, "floor.txt"))
	if err != nil {
		t.Fatalf("gate did not run: %v", err)
	}
	if string(got) != "0.35" {
		t.Fatalf("gate BOSS_GITHUB_QUOTA_FLOOR = %q, want %q", got, "0.35")
	}
}

func TestQuotaFloorNeverBlocksWhatItCannotJudge_ScheduledJobs_AC220301(t *testing.T) {
	for _, tt := range []struct {
		name      string
		origin    string
		quota     *fakeQuota
		wantReads int
	}{
		{"unreadable quota proceeds", quotaTestOrigin, &fakeQuota{err: errors.New("gh: network unreachable")}, 1},
		{"non-GitHub repo is never checked", "https://gitlab.com/acme/widgets.git", &fakeQuota{quotas: quotasAt(0)}, 0},
		{"repo without an origin is never checked", "", &fakeQuota{quotas: quotasAt(0)}, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, _, creator, _ := quotaScheduler(t, sweepJob(), tt.origin, tt.quota)
			sess, skipped, err := s.fire(context.Background(), "sweep")
			if err != nil || skipped != "" || sess == nil {
				t.Fatalf("fire = (%v, %q, %v), want a session", sess, skipped, err)
			}
			if n := len(creator.calls); n != 1 {
				t.Fatalf("CreateSession called %d times, want 1", n)
			}
			if got := tt.quota.reads(); got != tt.wantReads {
				t.Fatalf("quota read %d times, want %d", got, tt.wantReads)
			}
		})
	}
}

func TestRunNowIgnoresQuotaFloor(t *testing.T) {
	quota := &fakeQuota{quotas: quotasAt(0)}
	s, _, creator, _ := quotaScheduler(t, sweepJob(), quotaTestOrigin, quota)

	sess, skipped, err := s.RunNow(context.Background(), "sweep")
	if err != nil || skipped != "" || sess == nil {
		t.Fatalf("RunNow = (%v, %q, %v), want a session: a manual run spends quota on purpose", sess, skipped, err)
	}
	if n := len(creator.calls); n != 1 || quota.reads() != 0 {
		t.Fatalf("CreateSession calls = %d, quota reads = %d; want 1 and 0", n, quota.reads())
	}
}

// refusedProvider is a real GitHub provider that has just taken one GraphQL
// refusal worded like GitHub's per-minute limit, while rate_limit reports
// graphQLRemaining of 5000. It reads the quotas before the refusal, so a
// refusal that zeroed the cached reading would reach the floor.
func refusedProvider(t *testing.T, graphQLRemaining int) *github.Provider {
	t.Helper()
	clock := clocktest.New(time.Unix(1_800_000_000, 0))
	runGH := func(_ context.Context, args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "api" && args[1] == "rate_limit" {
			reset := clock.Now().Add(40 * time.Minute).Unix()
			return fmt.Sprintf(`{"resources":{"core":{"limit":5000,"remaining":4900,"reset":%d},"graphql":{"limit":5000,"remaining":%d,"reset":%d}}}`,
				reset, graphQLRemaining, reset), nil
		}
		return "", errors.New("gh pr view 7 --repo o/r: exit status 1: GraphQL: API rate limit already exceeded for user ID 96322.")
	}
	p := github.New(zerolog.Nop(), github.WithRunGH(runGH), github.WithNowFunc(clock.Now),
		github.WithJitterFunc(func(time.Duration) time.Duration { return 0 }))
	if _, err := p.QuotaStatus(context.Background()); err != nil {
		t.Fatalf("QuotaStatus: %v", err)
	}
	clock.Advance(5 * time.Second)
	if _, err := p.GetPRStatus(context.Background(), "o/r", 7); !errors.Is(err, vcs.ErrRateLimited) {
		t.Fatalf("refused call err = %v, want vcs.ErrRateLimited", err)
	}
	return p
}

func TestVerifyCronFiresDuringBurstPause_ScheduledJobs_AC230402(t *testing.T) {
	s, _, creator, _ := quotaScheduler(t, verifyJob(), quotaTestOrigin, nil)
	s.quota = refusedProvider(t, 4699)
	sess, skipped, err := s.fire(context.Background(), "verify")
	if err != nil || skipped == SkipReasonQuotaReserved || sess == nil {
		t.Fatalf("fire during a burst pause = (%v, %q, %v), want a verify session", sess, skipped, err)
	}
	if n := len(creator.calls); n != 1 {
		t.Fatalf("CreateSession called %d times, want 1", n)
	}

	// The same path with GitHub reporting the hourly quota spent still skips,
	// so the pass above is not vacuous.
	s, _, creator, _ = quotaScheduler(t, verifyJob(), quotaTestOrigin, nil)
	s.quota = refusedProvider(t, 0)
	if _, skipped, err := s.fire(context.Background(), "verify"); err != nil || skipped != SkipReasonQuotaReserved {
		t.Fatalf("fire after a real exhaustion = (%q, %v), want skip %q", skipped, err, SkipReasonQuotaReserved)
	}
	if n := len(creator.calls); n != 0 {
		t.Fatalf("CreateSession called %d times after a real exhaustion, want 0", n)
	}
}
