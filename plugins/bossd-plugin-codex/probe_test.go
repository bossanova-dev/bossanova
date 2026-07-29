package main

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestProbeRateLimitMapsRolloutFixtures(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())

	tests := []struct {
		name       string
		fixture    string
		status     bossanovav1.RateLimitPlanStatus
		limited    bool
		util5h     float64
		util7d     float64
		reset5hNil bool
		reset7dNil bool
		planTier   string
	}{
		{
			name:     "healthy",
			fixture:  "testdata/transcripts/ratelimit_healthy.jsonl",
			status:   bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_ACTIVE,
			util5h:   0.12,
			util7d:   0.34,
			planTier: "plus",
		},
		{
			name:     "near",
			fixture:  "testdata/transcripts/ratelimit_near.jsonl",
			status:   bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_ACTIVE,
			util5h:   0.95,
			util7d:   0.88,
			planTier: "plus",
		},
		{
			name:     "exhausted",
			fixture:  "testdata/transcripts/ratelimit_exhausted.jsonl",
			status:   bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_RATE_LIMITED,
			limited:  true,
			util5h:   1.0,
			util7d:   0.72,
			planTier: "plus",
		},
		{
			name:       "weekly only",
			fixture:    "testdata/transcripts/ratelimit_weekly_only.jsonl",
			status:     bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_ACTIVE,
			util7d:     0.26,
			reset5hNil: true,
			planTier:   "pro",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			codexHome := t.TempDir()
			copyFixture(t, tt.fixture, shardedRolloutPath(filepath.Join(codexHome, "sessions"), "probe-"+tt.name))

			s := &Server{logger: zerolog.Nop()}
			resp, err := s.ProbeRateLimit(context.Background(), &bossanovav1.ProbeRateLimitRequest{
				CredentialEnv: map[string]string{"CODEX_HOME": codexHome},
			})
			if err != nil {
				t.Fatalf("ProbeRateLimit: %v", err)
			}
			status := resp.GetStatus()
			if got := status.GetStatus(); got != tt.status {
				t.Fatalf("Status = %v, want %v", got, tt.status)
			}
			if got := status.GetLimited(); got != tt.limited {
				t.Fatalf("Limited = %v, want %v", got, tt.limited)
			}
			if math.Abs(status.GetUtil_5H()-tt.util5h) > 0.0001 {
				t.Fatalf("Util_5H = %v, want %v", status.GetUtil_5H(), tt.util5h)
			}
			if math.Abs(status.GetUtil_7D()-tt.util7d) > 0.0001 {
				t.Fatalf("Util_7D = %v, want %v", status.GetUtil_7D(), tt.util7d)
			}
			if got := status.GetReset_5H() == nil; got != tt.reset5hNil {
				t.Fatalf("Reset_5H nil = %v, want %v", got, tt.reset5hNil)
			}
			if got := status.GetReset_7D() == nil; got != tt.reset7dNil {
				t.Fatalf("Reset_7D nil = %v, want %v", got, tt.reset7dNil)
			}
			if got := status.GetPlanTier(); got != tt.planTier {
				t.Fatalf("PlanTier = %q, want %q", got, tt.planTier)
			}
		})
	}
}

func TestProbeRateLimitUnsupportedWhenNoSnapshotOrHome(t *testing.T) {
	tests := []struct {
		name          string
		credentialEnv map[string]string
		fixture       string
	}{
		{name: "missing CODEX_HOME", credentialEnv: nil},
		{name: "missing rollout", credentialEnv: map[string]string{"CODEX_HOME": ""}},
		{name: "no snapshot", credentialEnv: map[string]string{"CODEX_HOME": ""}, fixture: "testdata/transcripts/ratelimit_no_snapshot.jsonl"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := tt.credentialEnv
			if env != nil {
				codexHome := t.TempDir()
				env = map[string]string{"CODEX_HOME": codexHome}
				if tt.fixture != "" {
					copyFixture(t, tt.fixture, shardedRolloutPath(filepath.Join(codexHome, "sessions"), "no-snapshot"))
				}
			}

			s := &Server{logger: zerolog.Nop()}
			resp, err := s.ProbeRateLimit(context.Background(), &bossanovav1.ProbeRateLimitRequest{CredentialEnv: env})
			if err != nil {
				t.Fatalf("ProbeRateLimit: %v", err)
			}
			if got := resp.GetStatus().GetStatus(); got != bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_UNSUPPORTED {
				t.Fatalf("Status = %v, want UNSUPPORTED", got)
			}
			if resp.GetStatus().GetLimited() {
				t.Fatal("Limited = true, want false for unsupported")
			}
		})
	}
}

func TestProbeRateLimitUnsupportedForSharedSessionsRoot(t *testing.T) {
	base := t.TempDir()
	copyFixture(t, "testdata/transcripts/ratelimit_exhausted.jsonl", shardedRolloutPath(filepath.Join(base, "sessions"), "shared"))

	codexHome := t.TempDir()
	if err := os.Symlink(filepath.Join(base, "sessions"), filepath.Join(codexHome, "sessions")); err != nil {
		t.Fatalf("symlink shared sessions root: %v", err)
	}

	s := &Server{logger: zerolog.Nop()}
	resp, err := s.ProbeRateLimit(context.Background(), &bossanovav1.ProbeRateLimitRequest{
		CredentialEnv: map[string]string{"CODEX_HOME": codexHome},
	})
	if err != nil {
		t.Fatalf("ProbeRateLimit: %v", err)
	}
	if got := resp.GetStatus().GetStatus(); got != bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_UNSUPPORTED {
		t.Fatalf("Status = %v, want UNSUPPORTED for shared sessions root", got)
	}
	if resp.GetStatus().GetLimited() {
		t.Fatal("Limited = true, want false for unsupported shared sessions root")
	}
}

func TestPercentToFractionClampsOverCapUsage(t *testing.T) {
	tests := []struct {
		name    string
		percent float64
		want    float64
	}{
		{name: "negative", percent: -10, want: 0},
		{name: "zero", percent: 0, want: 0},
		{name: "partial", percent: 12.5, want: 0.125},
		{name: "full", percent: 100, want: 1},
		{name: "over cap", percent: 120, want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := percentToFraction(tt.percent); math.Abs(got-tt.want) > 0.0001 {
				t.Fatalf("percentToFraction(%v) = %v, want %v", tt.percent, got, tt.want)
			}
		})
	}
}

func TestMapRateLimitSnapshotClearsExpiredLimitWindows(t *testing.T) {
	now := time.Unix(2_000, 0)
	tests := []struct {
		name    string
		resetAt int64
		reached string
		limited bool
		status  bossanovav1.RateLimitPlanStatus
	}{
		{
			name:    "expired exhausted snapshot is active",
			resetAt: 1_000,
			reached: "primary",
			status:  bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_ACTIVE,
		},
		{
			name:    "future exhausted snapshot is limited",
			resetAt: 3_000,
			reached: "primary",
			limited: true,
			status:  bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_RATE_LIMITED,
		},
		{
			name:    "expired primary reached type ignores future secondary reset",
			resetAt: 1_000,
			reached: "primary",
			status:  bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_ACTIVE,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := mapRateLimitSnapshotAt(codexRateLimitSnapshot{
				Primary: &codexRateLimitWindow{
					UsedPercent: 120,
					ResetsAt:    tt.resetAt,
				},
				Secondary: &codexRateLimitWindow{
					UsedPercent: 99,
					ResetsAt:    3_000,
				},
				RateLimitReachedType: tt.reached,
			}, now)

			if got := status.GetLimited(); got != tt.limited {
				t.Fatalf("Limited = %v, want %v", got, tt.limited)
			}
			if got := status.GetStatus(); got != tt.status {
				t.Fatalf("Status = %v, want %v", got, tt.status)
			}
			if got := status.GetUtil_5H(); got != 1 {
				t.Fatalf("Util_5H = %v, want clamped 1", got)
			}
		})
	}
}

func TestClassifyRateLimitWindows(t *testing.T) {
	short := &codexRateLimitWindow{WindowMinutes: 300}
	weekly := &codexRateLimitWindow{WindowMinutes: 10_080}
	secondWeekly := &codexRateLimitWindow{WindowMinutes: 20_160}
	unknownPrimary := &codexRateLimitWindow{}
	unknownSecondary := &codexRateLimitWindow{}

	tests := []struct {
		name              string
		snapshot          codexRateLimitSnapshot
		wantShort, want7D *codexRateLimitWindow
	}{
		{
			name: "weekly only primary",
			snapshot: codexRateLimitSnapshot{
				Primary: weekly,
			},
			want7D: weekly,
		},
		{
			name: "legacy short and weekly",
			snapshot: codexRateLimitSnapshot{
				Primary:   short,
				Secondary: weekly,
			},
			wantShort: short,
			want7D:    weekly,
		},
		{
			name: "missing window minutes uses positional fallback",
			snapshot: codexRateLimitSnapshot{
				Primary:   unknownPrimary,
				Secondary: unknownSecondary,
			},
			wantShort: unknownPrimary,
			want7D:    unknownSecondary,
		},
		{
			name: "first weekly window wins",
			snapshot: codexRateLimitSnapshot{
				Primary:   weekly,
				Secondary: secondWeekly,
			},
			want7D: weekly,
		},
		{
			name: "nil secondary",
			snapshot: codexRateLimitSnapshot{
				Primary: short,
			},
			wantShort: short,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotShort, got7D := classifyRateLimitWindows(tt.snapshot)
			if gotShort != tt.wantShort {
				t.Fatalf("short = %p, want %p", gotShort, tt.wantShort)
			}
			if got7D != tt.want7D {
				t.Fatalf("weekly = %p, want %p", got7D, tt.want7D)
			}
		})
	}
}

func TestMapRateLimitSnapshotWeeklyOnlyLimitAndReachedTypes(t *testing.T) {
	now := time.Unix(2_000, 0)
	tests := []struct {
		name     string
		snapshot codexRateLimitSnapshot
		limited  bool
		status   bossanovav1.RateLimitPlanStatus
	}{
		{
			name: "weekly only exhausted window is limited",
			snapshot: codexRateLimitSnapshot{
				Primary: &codexRateLimitWindow{UsedPercent: 100, WindowMinutes: 10_080, ResetsAt: 3_000},
			},
			limited: true,
			status:  bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_RATE_LIMITED,
		},
		{
			name: "weekly reached type uses classified weekly window",
			snapshot: codexRateLimitSnapshot{
				Primary:              &codexRateLimitWindow{WindowMinutes: 10_080, ResetsAt: 1_000},
				Secondary:            &codexRateLimitWindow{WindowMinutes: 300, ResetsAt: 3_000},
				RateLimitReachedType: "7d",
			},
			status: bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_ACTIVE,
		},
		{
			name: "short reached type uses classified short window",
			snapshot: codexRateLimitSnapshot{
				Primary:              &codexRateLimitWindow{WindowMinutes: 10_080, ResetsAt: 3_000},
				Secondary:            &codexRateLimitWindow{WindowMinutes: 300, ResetsAt: 1_000},
				RateLimitReachedType: "5h",
			},
			status: bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_ACTIVE,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := mapRateLimitSnapshotAt(tt.snapshot, now)
			if got := status.GetLimited(); got != tt.limited {
				t.Fatalf("Limited = %v, want %v", got, tt.limited)
			}
			if got := status.GetStatus(); got != tt.status {
				t.Fatalf("Status = %v, want %v", got, tt.status)
			}
			if tt.name == "weekly only exhausted window is limited" && status.GetUtil_7D() != 1 {
				t.Fatalf("Util_7D = %v, want clamped 1", status.GetUtil_7D())
			}
		})
	}
}

func TestLatestRateLimitSnapshotFollowsSymlinkedSessionsRoot(t *testing.T) {
	realRoot := t.TempDir()
	copyFixture(t, "testdata/transcripts/ratelimit_healthy.jsonl", shardedRolloutPath(realRoot, "symlinked"))

	codexHome := t.TempDir()
	sessionsRoot := filepath.Join(codexHome, "sessions")
	if err := os.Symlink(realRoot, sessionsRoot); err != nil {
		t.Fatalf("symlink sessions root: %v", err)
	}

	snapshot, ok := latestRateLimitSnapshot(sessionsRoot)
	if !ok {
		t.Fatal("latestRateLimitSnapshot ok = false, want true through symlink root")
	}
	if snapshot.Primary == nil {
		t.Fatal("Primary = nil, want fixture window")
	}
	if got := snapshot.Primary.UsedPercent; got != 12 {
		t.Fatalf("Primary.UsedPercent = %v, want fixture value 12", got)
	}
}

func TestLatestRateLimitSnapshotNewestWins(t *testing.T) {
	root := t.TempDir()
	path := shardedRolloutPath(root, "newest-wins")
	copyFixture(t, "testdata/transcripts/ratelimit_newest_wins.jsonl", path)

	snapshot, ok := latestRateLimitSnapshot(root)
	if !ok {
		t.Fatal("latestRateLimitSnapshot ok = false, want true")
	}
	if snapshot.Primary == nil {
		t.Fatal("Primary = nil, want fixture window")
	}
	if got := snapshot.Primary.UsedPercent; got != 100 {
		t.Fatalf("Primary.UsedPercent = %v, want final snapshot value 100", got)
	}
	if got := snapshot.Timestamp; !got.Equal(time.Date(2026, 5, 8, 7, 47, 0, 0, time.UTC)) {
		t.Fatalf("Timestamp = %s, want final snapshot timestamp", got.Format(time.RFC3339))
	}
}
