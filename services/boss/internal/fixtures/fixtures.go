// Package fixtures provides one deterministic "demo world" of dummy domain
// data (repos, sessions, chats, cron jobs) for populating proof screenshots
// and integration tests. It depends only on the generated pb types so any
// test package can import it without an import cycle.
package fixtures

import (
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// fixedNow pins the clock so captured screens are byte-identical across runs.
var fixedNow = time.Date(2026, 1, 15, 9, 0, 0, 0, time.UTC)

func ts(offset time.Duration) *timestamppb.Timestamp {
	return timestamppb.New(fixedNow.Add(offset))
}

func i32(n int32) *int32 { return &n }

// World is the deterministic demo dataset used to populate proof screenshots.
type World struct {
	Repos    []*pb.Repo
	Sessions []*pb.Session
	Chats    []*pb.ClaudeChat
	CronJobs []*pb.CronJob
}

// Repos returns the registered repositories. The first entry (repo-1 / my-app)
// matches the legacy tuitest data so existing tests keep passing when they
// delegate here. Five entries fill the repo list for a realistic capture.
func Repos() []*pb.Repo {
	return []*pb.Repo{
		{Id: "repo-1", DisplayName: "my-app", LocalPath: "/tmp/my-app", DefaultBaseBranch: "main", MergeStrategy: "merge"},
		{Id: "repo-2", DisplayName: "my-api", LocalPath: "/tmp/my-api", DefaultBaseBranch: "main", MergeStrategy: "squash"},
		{Id: "repo-3", DisplayName: "my-web", LocalPath: "/tmp/my-web", DefaultBaseBranch: "main", MergeStrategy: "squash"},
		{Id: "repo-4", DisplayName: "mobile-app", LocalPath: "/tmp/mobile-app", DefaultBaseBranch: "main", MergeStrategy: "merge"},
		{
			// design-system sorts first alphabetically, so it is the default-selected
			// repo in both the repo-settings and new-session pickers. Made-up Sentry
			// credentials here light up the repo-settings Sentry section and the
			// new-session "Fix a Sentry issue" option in proof captures. The API key
			// renders masked (last 4 only), so the demo token is safe to publish.
			Id: "repo-5", DisplayName: "design-system", LocalPath: "/tmp/design-system", DefaultBaseBranch: "main", MergeStrategy: "merge",
			SentryApiKey: "sntryu_0f4d2c9a1b6e8740demo3a5c", SentryOrg: "acme-engineering",
		},
	}
}

// ActiveSessions returns non-archived sessions across a spread of states so the
// home screen's STATUS column is realistic. The first entry (sess-aaa-111 /
// "Add dark mode") must stay first: the home view preserves slice order for
// no-attention sessions, and proof navigation selects index 0 to reach the
// chat picker, so existing anchors stay valid.
func ActiveSessions() []*pb.Session {
	return []*pb.Session{
		{
			Id: "sess-aaa-111", RepoId: "repo-1", RepoDisplayName: "my-app",
			Title: "Add dark mode", BranchName: "boss/add-dark-mode",
			State:    pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN,
			PrNumber: i32(597), CreatedAt: ts(-2 * time.Hour),
		},
		{
			Id: "sess-bbb-222", RepoId: "repo-1", RepoDisplayName: "my-app",
			Title: "Fix login bug", BranchName: "boss/fix-login-bug",
			State:     pb.SessionState_SESSION_STATE_AWAITING_CHECKS,
			CreatedAt: ts(-5 * time.Hour),
		},
		{
			Id: "sess-ddd-444", RepoId: "repo-2", RepoDisplayName: "my-api",
			Title: "Add rate limiting to public API", BranchName: "boss/rate-limiting",
			State:    pb.SessionState_SESSION_STATE_READY_FOR_REVIEW,
			PrNumber: i32(412), CreatedAt: ts(-7 * time.Hour),
		},
		{
			Id: "sess-eee-555", RepoId: "repo-3", RepoDisplayName: "my-web",
			Title: "Refresh landing page hero", BranchName: "boss/landing-hero",
			State:    pb.SessionState_SESSION_STATE_GREEN_DRAFT,
			PrNumber: i32(88), CreatedAt: ts(-26 * time.Hour),
		},
		{
			Id: "sess-fff-666", RepoId: "repo-4", RepoDisplayName: "mobile-app",
			Title: "Upgrade to React Navigation 7", BranchName: "boss/rn7-upgrade",
			State:    pb.SessionState_SESSION_STATE_FIXING_CHECKS,
			PrNumber: i32(233), CreatedAt: ts(-30 * time.Hour),
		},
	}
}

// ArchivedSessions returns sessions with ArchivedAt set so the Trash screen is
// non-empty. The mock daemon only surfaces these when IncludeArchived is set.
// Five entries give the Trash list a realistic fill; sess-ccc-333 stays first
// to match legacy data.
func ArchivedSessions() []*pb.Session {
	return []*pb.Session{
		{
			Id: "sess-ccc-333", RepoId: "repo-2", RepoDisplayName: "my-api",
			Title: "Old caching spike", BranchName: "boss/caching-spike",
			State:      pb.SessionState_SESSION_STATE_AWAITING_CHECKS,
			ArchivedAt: ts(-48 * time.Hour), CreatedAt: ts(-72 * time.Hour),
		},
		{
			Id: "sess-ggg-777", RepoId: "repo-1", RepoDisplayName: "my-app",
			Title: "Offline mode prototype", BranchName: "boss/offline-mode",
			State:      pb.SessionState_SESSION_STATE_CLOSED,
			ArchivedAt: ts(-96 * time.Hour), CreatedAt: ts(-120 * time.Hour),
		},
		{
			Id: "sess-hhh-888", RepoId: "repo-3", RepoDisplayName: "my-web",
			Title: "Holiday banner experiment", BranchName: "boss/holiday-banner",
			State:      pb.SessionState_SESSION_STATE_MERGED,
			ArchivedAt: ts(-144 * time.Hour), CreatedAt: ts(-168 * time.Hour),
		},
		{
			Id: "sess-iii-999", RepoId: "repo-2", RepoDisplayName: "my-api",
			Title: "Deprecate v1 endpoints", BranchName: "boss/deprecate-v1",
			State:      pb.SessionState_SESSION_STATE_CLOSED,
			ArchivedAt: ts(-200 * time.Hour), CreatedAt: ts(-240 * time.Hour),
		},
		{
			Id: "sess-jjj-000", RepoId: "repo-4", RepoDisplayName: "mobile-app",
			Title: "Legacy onboarding flow", BranchName: "boss/legacy-onboarding",
			State:      pb.SessionState_SESSION_STATE_MERGED,
			ArchivedAt: ts(-300 * time.Hour), CreatedAt: ts(-340 * time.Hour),
		},
	}
}

// Chats returns conversations for the first active session, newest first. All
// five reference sess-aaa-111 so the chat picker for that session is fully
// populated; chat-1 ("Initial implementation") stays first as the proof anchor.
func Chats() []*pb.ClaudeChat {
	return []*pb.ClaudeChat{
		{Id: "chat-1", AgentSessionId: "claude-111", SessionId: "sess-aaa-111", Title: "Initial implementation", CreatedAt: ts(-1 * time.Hour)},
		{Id: "chat-2", AgentSessionId: "claude-222", SessionId: "sess-aaa-111", Title: "Follow-up review", CreatedAt: ts(-2 * time.Hour)},
		{Id: "chat-3", AgentSessionId: "claude-333", SessionId: "sess-aaa-111", Title: "Address review comments", CreatedAt: ts(-3 * time.Hour)},
		{Id: "chat-4", AgentSessionId: "claude-444", SessionId: "sess-aaa-111", Title: "Fix failing checks", CreatedAt: ts(-4 * time.Hour)},
		{Id: "chat-5", AgentSessionId: "claude-555", SessionId: "sess-aaa-111", Title: "Final polish pass", CreatedAt: ts(-5 * time.Hour)},
	}
}

// CronJobs returns scheduled jobs so the Scheduled Jobs screen is non-empty.
// Five entries (mix of schedules, agents, and one disabled) give the list a
// realistic fill; cron-1 stays first to match legacy data. Two carry the
// gate-command statuses (gating in progress, gated blocked) so the list
// proof shows the new STATUS values, and "Morning PR triage" sets a gate
// command so the cron form proof has a populated Gate command field.
func CronJobs() []*pb.CronJob {
	return []*pb.CronJob{
		{Id: "cron-1", RepoId: "repo-1", Name: "Daily dependency update", Prompt: "Update dependencies and open a PR", Schedule: "@daily", Timezone: "UTC", Enabled: true, AgentName: "claude", RunSetupCommand: true},
		{Id: "cron-2", RepoId: "repo-2", Name: "Nightly mutation tests", Prompt: "Run mutation tests and add coverage for survivors", Schedule: "0 3 * * *", Timezone: "UTC", Enabled: true, AgentName: "claude", RunSetupCommand: true, LastRunStatus: pb.CronJobStatus_CRON_JOB_STATUS_GATING},
		{Id: "cron-3", RepoId: "repo-1", Name: "Weekly tech-debt sweep", Prompt: "Find and fix one unit of technical debt", Schedule: "@weekly", Timezone: "UTC", Enabled: true, AgentName: "claude", RunSetupCommand: true},
		{Id: "cron-4", RepoId: "repo-3", Name: "Hourly broken-link check", Prompt: "Scan the marketing site for broken links", Schedule: "@hourly", Timezone: "UTC", Enabled: false, AgentName: "claude", RunSetupCommand: true},
		{Id: "cron-5", RepoId: "repo-2", Name: "Morning PR triage", Prompt: "Triage open PRs and summarize review state", Schedule: "0 9 * * 1-5", Timezone: "UTC", Enabled: true, AgentName: "codex", GateCommand: "gh pr list --label needs-triage --state open | grep .", RunSetupCommand: false, LastRunStatus: pb.CronJobStatus_CRON_JOB_STATUS_GATED, LastRunOutcome: "gated"},
	}
}

// DemoWorld assembles the full deterministic dataset. Sessions are ordered
// active-first (index 0 is the default-selected home-screen entry, sess-aaa-111)
// with archived sessions appended; downstream proof navigation depends on this.
func DemoWorld() World {
	return World{
		Repos:    Repos(),
		Sessions: append(ActiveSessions(), ArchivedSessions()...),
		Chats:    Chats(),
		CronJobs: CronJobs(),
	}
}
