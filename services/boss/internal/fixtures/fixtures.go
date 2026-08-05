// Package fixtures provides one deterministic "demo world" of dummy domain
// data (repos, sessions, chats, cron jobs) for populating proof screenshots
// and integration tests. It depends only on the generated pb types so any
// test package can import it without an import cycle.
package fixtures

import (
	"time"

	"github.com/recurser/bossalib/displaystatus"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// fixedNow pins the clock so captured screens are byte-identical across runs.
var fixedNow = time.Date(2026, 1, 15, 9, 0, 0, 0, time.UTC)

func ts(offset time.Duration) *timestamppb.Timestamp {
	return timestamppb.New(fixedNow.Add(offset))
}

func i32(n int32) *int32 { return &n }

func sp(s string) *string { return &s }

// World is the deterministic demo dataset used to populate proof screenshots.
type World struct {
	Repos        []*pb.Repo
	Sessions     []*pb.Session
	Chats        []*pb.ClaudeChat
	ChatStatuses []*pb.ChatStatusEntry
	// SessionStatuses carries the daemon's aggregate per-session heartbeat. Only
	// presets that need it seed it — notably waiting-callback, where the home
	// list's waiting-reason sub-row is read from SessionStatusEntry.waiting_reason
	// rather than from the Session itself (BOS-668).
	SessionStatuses []*pb.SessionStatusEntry
	CronJobs        []*pb.CronJob
	Accounts        []*pb.Account
}

// Repos returns the registered repositories. The first entry (repo-1 / my-app)
// matches the legacy tuitest data so existing tests keep passing when they
// delegate here. Five entries fill the repo list for a realistic capture.
func Repos() []*pb.Repo {
	// ShouldArchiveSessionsAfterMerge and CanAutoDeleteBranches are true on every demo
	// repo to mirror the real default-on (the double-default the DB layer enforces
	// on Create), so the repo-settings Automations section renders both checkboxes
	// checked in proof.
	return []*pb.Repo{
		{Id: "repo-1", DisplayName: "my-app", LocalPath: "/tmp/my-app", DefaultBaseBranch: "main", MergeStrategy: "merge", ShouldArchiveSessionsAfterMerge: true, CanAutoDeleteBranches: true},
		{Id: "repo-2", DisplayName: "my-api", LocalPath: "/tmp/my-api", DefaultBaseBranch: "main", MergeStrategy: "squash", ShouldArchiveSessionsAfterMerge: true, CanAutoDeleteBranches: true},
		{Id: "repo-3", DisplayName: "my-web", LocalPath: "/tmp/my-web", DefaultBaseBranch: "main", MergeStrategy: "squash", ShouldArchiveSessionsAfterMerge: true, CanAutoDeleteBranches: true},
		{Id: "repo-4", DisplayName: "mobile-app", LocalPath: "/tmp/mobile-app", DefaultBaseBranch: "main", MergeStrategy: "merge", ShouldArchiveSessionsAfterMerge: true, CanAutoDeleteBranches: true},
		{
			// design-system sorts first alphabetically, so it is the default-selected
			// repo in both the repo-settings and new-session pickers. Made-up Sentry
			// credentials here light up the repo-settings Sentry section and the
			// new-session "Fix a Sentry issue" option in proof captures. The API key
			// renders masked (last 4 only), so the demo token is safe to publish.
			Id: "repo-5", DisplayName: "design-system", LocalPath: "/tmp/design-system", DefaultBaseBranch: "main", MergeStrategy: "merge",
			SentryApiKey: "sntryu_0f4d2c9a1b6e8740demo3a5c", SentryOrg: "acme-engineering", ShouldArchiveSessionsAfterMerge: true, CanAutoDeleteBranches: true,
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
			WorktreePath: "/Users/demo/worktrees/my-app/add-dark-mode",
			AccountId:    sp("work-claude"),
		},
		{
			Id: "sess-bbb-222", RepoId: "repo-1", RepoDisplayName: "my-app",
			Title: "Fix login bug", BranchName: "boss/fix-login-bug",
			State:        pb.SessionState_SESSION_STATE_AWAITING_CHECKS,
			CreatedAt:    ts(-5 * time.Hour),
			WorktreePath: "/Users/demo/worktrees/my-app/fix-login-bug",
		},
		{
			Id: "sess-ddd-444", RepoId: "repo-2", RepoDisplayName: "my-api",
			Title: "Add rate limiting to public API", BranchName: "boss/rate-limiting",
			State:    pb.SessionState_SESSION_STATE_READY_FOR_REVIEW,
			PrNumber: i32(412), CreatedAt: ts(-7 * time.Hour),
			WorktreePath: "/Users/demo/worktrees/my-api/rate-limiting",
		},
		{
			Id: "sess-eee-555", RepoId: "repo-3", RepoDisplayName: "my-web",
			Title: "Refresh landing page hero", BranchName: "boss/landing-hero",
			State:    pb.SessionState_SESSION_STATE_GREEN_DRAFT,
			PrNumber: i32(88), CreatedAt: ts(-26 * time.Hour),
			WorktreePath: "/Users/demo/worktrees/my-web/landing-hero",
		},
		{
			Id: "sess-fff-666", RepoId: "repo-4", RepoDisplayName: "mobile-app",
			Title: "Upgrade to React Navigation 7", BranchName: "boss/rn7-upgrade",
			State:    pb.SessionState_SESSION_STATE_FIXING_CHECKS,
			PrNumber: i32(233), CreatedAt: ts(-30 * time.Hour),
			WorktreePath: "/Users/demo/worktrees/mobile-app/rn7-upgrade",
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
// command so the cron form proof has a populated Gate command field — and is
// also the one job with IsZeroOutput set, so the cron form's edit-mode proof
// can show that confirm pre-populated affirmative (BOS-565). cron-8
// carries the benign worktree_gone outcome with an IDLE status (BOS-384): a
// finalize against an already-removed worktree renders as a plain "idle" row,
// never a red FAILED framing.
func CronJobs() []*pb.CronJob {
	return []*pb.CronJob{
		{Id: "cron-1", RepoId: "repo-1", Name: "Daily dependency update", Prompt: "Update dependencies and open a PR", Schedule: "@daily", Timezone: "UTC", IsEnabled: true, AgentName: "claude", ShouldRunSetupCommand: true},
		{Id: "cron-2", RepoId: "repo-2", Name: "Nightly mutation tests", Prompt: "Run mutation tests and add coverage for survivors", Schedule: "0 3 * * *", Timezone: "UTC", IsEnabled: true, AgentName: "claude", ShouldRunSetupCommand: true, LastRunStatus: pb.CronJobStatus_CRON_JOB_STATUS_GATING},
		{Id: "cron-3", RepoId: "repo-1", Name: "Weekly tech-debt sweep", Prompt: "Find and fix one unit of technical debt", Schedule: "@weekly", Timezone: "UTC", IsEnabled: true, AgentName: "claude", ShouldRunSetupCommand: true},
		{Id: "cron-4", RepoId: "repo-3", Name: "Hourly broken-link check", Prompt: "Scan the marketing site for broken links", Schedule: "@hourly", Timezone: "UTC", IsEnabled: false, AgentName: "claude", ShouldRunSetupCommand: true},
		// cron-5 is the "light job" archetype and the only fixture with
		// IsZeroOutput set (BOS-565), so the cron form's edit-mode proof scene has
		// a job whose Zero output confirm renders affirmative. Exactly one job
		// carries it so both states stay represented in the demo world.
		{Id: "cron-5", RepoId: "repo-2", Name: "Morning PR triage", Prompt: "Triage open PRs and summarize review state", Schedule: "0 9 * * 1-5", Timezone: "UTC", IsEnabled: true, AgentName: "codex", GateCommand: "gh pr list --label needs-triage --state open | grep .", ShouldRunSetupCommand: false, IsZeroOutput: true, LastRunStatus: pb.CronJobStatus_CRON_JOB_STATUS_GATED, LastRunOutcome: "gated"},
		// Disabled jobs carrying a colored last-run status (gated / failed): the
		// muted-row rendering states BOS-313 fixed are otherwise unreachable in
		// the demo world, so TUI proofs could never show them (BOS-251).
		{Id: "cron-6", RepoId: "repo-4", Name: "Paused release gate", Prompt: "Cut a release when the gate opens", Schedule: "@daily", Timezone: "UTC", IsEnabled: false, AgentName: "claude", GateCommand: "test -f RELEASE_READY", ShouldRunSetupCommand: true, LastRunStatus: pb.CronJobStatus_CRON_JOB_STATUS_GATED, LastRunOutcome: "gated"},
		{Id: "cron-7", RepoId: "repo-5", Name: "Paused visual regression", Prompt: "Run the visual regression suite", Schedule: "@weekly", Timezone: "UTC", IsEnabled: false, AgentName: "claude", ShouldRunSetupCommand: true, LastRunStatus: pb.CronJobStatus_CRON_JOB_STATUS_FAILED, LastRunOutcome: "failed"},
		// cron-8 (BOS-384): a benign worktree_gone outcome — finalize ran against
		// an already-removed worktree (archived/deleted session). It renders with
		// a plain IDLE status, never a red FAILED framing.
		{Id: "cron-8", RepoId: "repo-1", Name: "Stale branch cleanup", Prompt: "Prune merged branches and open a cleanup PR", Schedule: "@daily", Timezone: "UTC", IsEnabled: true, AgentName: "claude", ShouldRunSetupCommand: true, LastRunAt: ts(-2 * time.Hour), LastRunStatus: pb.CronJobStatus_CRON_JOB_STATUS_IDLE, LastRunOutcome: "worktree_gone"},
	}
}

// Accounts returns the managed rotation accounts so the Settings → Accounts
// list (BOS-265) is non-empty. Two entries exercise both providers and both
// health/status states: an active/ok "claude" account and a disabled/failed
// "codex" account that is cooling down and carries a last-test error. Fields
// are display-safe metadata only — the Account proto has no credential field —
// and all timestamps derive from the pinned clock so captures stay byte-stable.
func Accounts() []*pb.Account {
	return []*pb.Account{
		{
			Id:           "acct-claude-1",
			Provider:     "claude",
			Label:        "work-claude",
			Status:       "active",
			Priority:     0,
			Health:       "ok",
			Tier:         "max",
			LastUsedAt:   ts(-30 * time.Minute),
			LastTestOkAt: ts(-2 * time.Hour),
			CreatedAt:    ts(-720 * time.Hour),
			UpdatedAt:    ts(-30 * time.Minute),
			// A populated usage snapshot so the Accounts list AGE column and the
			// detail-screen usage rows (BOS-270) render real values for this
			// probed account, while the sibling codex account below stays
			// never-probed (nil Usage → em dash everywhere). PlanTier is the
			// provider-reported plan, deliberately distinct from the registry
			// Tier ("max") above. Reset instants use a far-FUTURE offset for the
			// same reason CooldownUntil does (see the codex account): the TUI's
			// usage cells countdown against the real wall clock, but every ts()
			// derives from fixedNow (pinned in the past), so a near offset would
			// already be elapsed at capture and drop the "resets in …" countdown.
			// FetchedAt is a small offset in the past so the age renders as a
			// concrete "fetched … ago" freshness line.
			Usage: &pb.UsageSnapshot{
				Util_5H:   0.42,
				Util_7D:   0.68,
				Reset_5H:  ts(500 * 24 * time.Hour),
				Reset_7D:  ts(505 * 24 * time.Hour),
				Status:    "active",
				PlanTier:  "max_20x",
				FetchedAt: ts(-4 * time.Minute),
			},
		},
		{
			Id:       "acct-codex-1",
			Provider: "codex",
			Label:    "personal-codex",
			Status:   "disabled",
			Priority: 1,
			Health:   "failed",
			Tier:     "plus",
			// CooldownUntil must render as a FUTURE "cooling · resets …" state in
			// proof captures. The TUI's cooldown cells use the real wall clock
			// (time.Now), but every fixture ts() derives from fixedNow (pinned in
			// the past for byte-stable screens), so a small offset like +45m would
			// already be elapsed at capture time and render "active". A large
			// offset keeps the cooling state well into the future for any realistic
			// capture run (BOS-269).
			CooldownUntil: ts(500 * 24 * time.Hour),
			// SYNTHETIC secret-shaped error so masking (maskTestError →
			// agenterr.Redact) is demonstrable in captures: the raw token must be
			// redacted to [REDACTED] on screen. This is a FAKE token, never a real
			// credential.
			LastTestError: "401 invalid_grant: token=sk-FAKE0123456789abcdef rejected",
			CreatedAt:     ts(-480 * time.Hour),
			UpdatedAt:     ts(-3 * time.Hour),
		},
	}
}

// ArchiveSignalSessions returns two merged (non-archived) sessions in an
// archive-after-merge repo that differ only in the daemon-driven archive_pending
// signal (BOS-425). Both have DisplayStatus=MERGED, RepoShouldArchiveSessionsAfterMerge
// on, and ArchivedAt nil (already resurrected once — no archive is actually in
// flight from steady state):
//   - index 0 (sess-425-resurrected): archive_pending=false — the regression
//     case; the detail view must NOT show "Archiving session...", and the home
//     list STATUS column shows "✓ merged".
//   - index 1 (sess-425-archiving): archive_pending=true — the daemon has an
//     archive in flight; the detail view MUST show "Archiving session...", and
//     (post-BOS-422) the home list STATUS column shows "archiving" with a
//     spinner, computed by displaystatus.Compute from archive_pending.
//
// Order is fixed: the home view preserves slice order for no-attention (merged)
// sessions, so the ArchiveSignal scenarios navigate to index 0 / index 1
// deterministically.
func ArchiveSignalSessions() []*pb.Session {
	return []*pb.Session{
		{
			// A resurrected session is active again (live lifecycle State) but its PR
			// is MERGED — exactly the steady state that made the old heuristic show a
			// permanent false spinner. State stays active so the session renders on the
			// home list (terminal-state sessions are hidden from the default home view).
			Id: "sess-425-resurrected", RepoId: "repo-1", RepoDisplayName: "my-app",
			Title: "Resurrected merged session", BranchName: "boss/resurrected-merged",
			State:         pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN,
			DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_MERGED,
			// DisplayLabel/Intent are normally hydrated server-side from DisplayStatus;
			// the mock daemon returns the session verbatim, so set them here so the
			// home STATUS column renders "✓ merged" (matches displaystatus.Compute).
			DisplayLabel:                        "✓ merged",
			DisplayIntent:                       pb.DisplayIntent_DISPLAY_INTENT_MUTED,
			RepoShouldArchiveSessionsAfterMerge: true,
			ArchivePending:                      false, // no archive in flight — must NOT show "Archiving session..."
			PrNumber:                            i32(424), CreatedAt: ts(-2 * time.Hour),
			WorktreePath: "/Users/demo/worktrees/my-app/resurrected-merged",
		},
		{
			Id: "sess-425-archiving", RepoId: "repo-1", RepoDisplayName: "my-app",
			Title: "Archiving merged session", BranchName: "boss/archiving-merged",
			State:         pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN,
			DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_MERGED,
			// Post-BOS-422 the daemon feeds archive_pending into displaystatus.Compute,
			// so an in-flight archive computes DisplayLabel="archiving" (WARNING +
			// spinner) instead of the stale "✓ merged" — this fixture mirrors that
			// server-side result so the home STATUS column renders "archiving" for the
			// list, matching the detail-view "Archiving session..." spinner (BOS-425).
			DisplayLabel:                        "archiving",
			DisplayIntent:                       pb.DisplayIntent_DISPLAY_INTENT_WARNING,
			DisplaySpinner:                      true,
			RepoShouldArchiveSessionsAfterMerge: true,
			ArchivePending:                      true, // daemon archive in flight — MUST show "archiving" (list) / "Archiving session..." (detail)
			PrNumber:                            i32(425), CreatedAt: ts(-3 * time.Hour),
			WorktreePath: "/Users/demo/worktrees/my-app/archiving-merged",
		},
	}
}

// ArchiveSignalChats returns one chat per ArchiveSignal session so both
// chatpickers render populated (not "Loading chats...").
func ArchiveSignalChats() []*pb.ClaudeChat {
	return []*pb.ClaudeChat{
		{Id: "chat-425-r", AgentSessionId: "claude-425-r", SessionId: "sess-425-resurrected", Title: "Implement the change", CreatedAt: ts(-2 * time.Hour)},
		{Id: "chat-425-a", AgentSessionId: "claude-425-a", SessionId: "sess-425-archiving", Title: "Implement the change", CreatedAt: ts(-3 * time.Hour)},
	}
}

// RotationHistoryWorld builds the BOS-432 chat-picker proof dataset: the demo
// repos plus a single active session carrying rotation history (a newest-first
// UNSPECIFIED BOS-409 stale-port event with the whole message in Detail, then a
// ROTATED event) and one chat. Navigating into the session shows the rotation
// block at the very bottom of the chat-picker view — below the [esc] back action
// bar. All event timestamps derive from the pinned fixedNow (2026-01-15), which
// is always an earlier calendar day than the real render clock, so every row
// renders with the ISO date prefix ("2026-…") deterministically.
func RotationHistoryWorld() World {
	return World{
		Repos:    Repos(),
		Sessions: RotationHistorySessions(),
		Chats:    RotationHistoryChats(),
	}
}

// RotationHistorySessions returns the single BOS-432 session with rotation
// history. The newest event is a generic (UNSPECIFIED) stale-port notice whose
// full message lives in Detail, proving the row renders "<date> <time> <detail>"
// with no "rotation event — " prefix.
func RotationHistorySessions() []*pb.Session {
	return []*pb.Session{
		{
			Id: "sess-432-rotation", RepoId: "repo-1", RepoDisplayName: "my-app",
			Title: "Rotation history demo", BranchName: "boss/rotation-history",
			State:        pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN,
			PrNumber:     i32(432),
			CreatedAt:    ts(-2 * time.Hour),
			WorktreePath: "/Users/demo/worktrees/my-app/rotation-history",
			RotationEvents: []*pb.RotationEvent{
				{
					Id:        "rot-432-1",
					Outcome:   pb.RotationOutcome_ROTATION_OUTCOME_UNSPECIFIED,
					Detail:    "stale failover-proxy port: pane baked 52106, live proxy on 44127 — restart this pane to reconnect (BOS-409)",
					CreatedAt: ts(-90 * time.Minute),
				},
				{
					Id:          "rot-432-2",
					Outcome:     pb.RotationOutcome_ROTATION_OUTCOME_ROTATED,
					FromAccount: "yuki@kamik.ai",
					ToAccount:   "dave@kamik.ai",
					Detail:      "resumed",
					CreatedAt:   ts(-2 * time.Hour),
				},
			},
		},
	}
}

// RotationHistoryChats returns the single chat for the BOS-432 session so the
// chat-picker renders a populated table and the full action bar.
func RotationHistoryChats() []*pb.ClaudeChat {
	return []*pb.ClaudeChat{
		{Id: "chat-432", AgentSessionId: "claude-432", SessionId: "sess-432-rotation", Title: "Implement the change", CreatedAt: ts(-2 * time.Hour)},
	}
}

// RespawnHistoryWorld builds the BOS-482 dataset: one session whose rotation
// history carries the two new respawn-in-place outcomes (RESPAWNED_SAME_ACCOUNT
// and, as the newest event, RESPAWN_CAP_EXHAUSTED). It proves both the
// chat-picker rotation-history labels and the home-list needs-attention hint
// that fires when the cap is reached.
func RespawnHistoryWorld() World {
	return World{
		Repos:    Repos(),
		Sessions: RespawnHistorySessions(),
		Chats:    RespawnHistoryChats(),
	}
}

// RespawnHistorySessions returns the single BOS-482 session. Its newest rotation
// event is RESPAWN_CAP_EXHAUSTED, so the home-list warning hint
// ("auth wedge unresolved — respawn cap reached, may need /login") fires; the two
// older RESPAWNED_SAME_ACCOUNT events exercise both label shapes ("refreshed auth
// in place on <acct>" when to_account is set, and the bare "refreshed auth in
// place" otherwise).
func RespawnHistorySessions() []*pb.Session {
	return []*pb.Session{
		{
			Id: "sess-482-respawn", RepoId: "repo-1", RepoDisplayName: "my-app",
			Title: "Auth-wedge respawn demo", BranchName: "boss/respawn-history",
			State:        pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN,
			PrNumber:     i32(482),
			CreatedAt:    ts(-2 * time.Hour),
			WorktreePath: "/Users/demo/worktrees/my-app/respawn-history",
			RotationEvents: []*pb.RotationEvent{
				{
					Id:        "rot-482-1",
					Outcome:   pb.RotationOutcome_ROTATION_OUTCOME_RESPAWN_CAP_EXHAUSTED,
					Detail:    "respawn cap reached (2/hour) — auth wedge persists",
					CreatedAt: ts(-10 * time.Minute),
				},
				{
					Id:        "rot-482-2",
					Outcome:   pb.RotationOutcome_ROTATION_OUTCOME_RESPAWNED_SAME_ACCOUNT,
					ToAccount: "dave@kamik.ai",
					Detail:    "resumed",
					CreatedAt: ts(-40 * time.Minute),
				},
				{
					Id:        "rot-482-3",
					Outcome:   pb.RotationOutcome_ROTATION_OUTCOME_RESPAWNED_SAME_ACCOUNT,
					Detail:    "started fresh",
					CreatedAt: ts(-70 * time.Minute),
				},
			},
		},
	}
}

// RespawnHistoryChats returns the single chat for the BOS-482 session so the
// chat-picker renders a populated table and the full action bar.
func RespawnHistoryChats() []*pb.ClaudeChat {
	return []*pb.ClaudeChat{
		{Id: "chat-482", AgentSessionId: "claude-482", SessionId: "sess-482-respawn", Title: "Implement the change", CreatedAt: ts(-2 * time.Hour)},
	}
}

// QuestionRowWorld builds the BOS-494 chat-picker dataset: one active session
// with two chats — a newer chat that is `working` and an older chat that is
// `? question` — plus heartbeat statuses. Navigating into the session must land
// the initial cursor on the older question chat (the one waiting on the user),
// not the newer working chat. The demo cloud-access pin makes boss land on the
// home session list.
func QuestionRowWorld() World {
	return World{
		Repos:        Repos(),
		Sessions:     QuestionRowSessions(),
		Chats:        QuestionRowChats(),
		ChatStatuses: QuestionRowChatStatuses(),
	}
}

// QuestionRowSessions returns the single BOS-494 session hosting the mixed
// working/question chats.
func QuestionRowSessions() []*pb.Session {
	return []*pb.Session{
		{
			Id: "sess-494-question", RepoId: "repo-1", RepoDisplayName: "my-app",
			Title: "Notification question demo", BranchName: "boss/notification-question",
			State:        pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN,
			PrNumber:     i32(494),
			CreatedAt:    ts(-2 * time.Hour),
			WorktreePath: "/Users/demo/worktrees/my-app/notification-question",
		},
	}
}

// QuestionRowChats returns the BOS-494 chats. The picker sorts newest-first, so
// the working chat (created more recently) renders on row 0 and the question
// chat on row 1 — the arrangement the fix must override by preselecting row 1.
func QuestionRowChats() []*pb.ClaudeChat {
	return []*pb.ClaudeChat{
		{Id: "chat-494-working", AgentSessionId: "claude-494-working", SessionId: "sess-494-question", Title: "Build the daily health sweep", CreatedAt: ts(-30 * time.Minute)},
		{Id: "chat-494-question", AgentSessionId: "claude-494-question", SessionId: "sess-494-question", Title: "Wire up web notifications", CreatedAt: ts(-2 * time.Hour)},
	}
}

// QuestionRowChatStatuses returns the daemon heartbeat statuses for the BOS-494
// chats: the newer chat is working, the older chat is asking a question.
func QuestionRowChatStatuses() []*pb.ChatStatusEntry {
	return []*pb.ChatStatusEntry{
		{AgentSessionId: "claude-494-working", Status: pb.ChatStatus_CHAT_STATUS_WORKING, LastOutputAt: ts(-2 * time.Minute)},
		{AgentSessionId: "claude-494-question", Status: pb.ChatStatus_CHAT_STATUS_QUESTION, LastOutputAt: ts(-50 * time.Minute)},
	}
}

// WaitingCallbackPRNumber is the PR the BOS-668 waiting session is parked on.
// Exported so the scenario/regression assertions can bind to the same number the
// reason string embeds rather than re-typing it.
const WaitingCallbackPRNumber = 668

// WaitingCallbackTrigger is the armed GitHub callback trigger the BOS-668
// waiting session is parked on.
const WaitingCallbackTrigger = "checks_passed_ready"

// WaitingCallbackReason is the canonical reason string for the BOS-668 fixture,
// composed by the shared displaystatus helper rather than hand-spelled here so
// the fixture can never drift from the wording the daemon actually emits.
var WaitingCallbackReason = displaystatus.CallbackWaitingReason(
	WaitingCallbackTrigger, "acme", "my-app", WaitingCallbackPRNumber,
)

// WaitingCallbackWorld builds the BOS-668 dataset: one session parked on an
// armed GitHub callback (rendered "waiting", INFO, with spinner, with the reason
// on its own sub-row) alongside a second session that is genuinely working.
//
// The two states live in SEPARATE sessions deliberately. The session-level
// aggregate ranks working above waiting, so a single session holding both a
// parked chat and a busy chat renders "working" — which would hide the very
// state this fixture exists to show on the home list.
func WaitingCallbackWorld() World {
	return World{
		Repos:           Repos(),
		Sessions:        WaitingCallbackSessions(),
		Chats:           WaitingCallbackChats(),
		ChatStatuses:    WaitingCallbackChatStatuses(),
		SessionStatuses: WaitingCallbackSessionStatuses(),
	}
}

// WaitingCallbackSessions returns the parked session and its working neighbour.
// The mock daemon serves sessions verbatim (it does not run displaystatus.Compute),
// so the Display* triple is spelled out here to exactly what the real cascade
// produces: "waiting" / INFO / spinner, and "working" / SUCCESS / spinner.
func WaitingCallbackSessions() []*pb.Session {
	return []*pb.Session{
		{
			Id: "sess-668-waiting", RepoId: "repo-1", RepoDisplayName: "my-app",
			// The title deliberately avoids the words "waiting" and the trigger
			// name so the proof assertions bind to the STATUS column and the
			// reason sub-row, not to a row title that happens to contain them.
			Title: "Ship the release checklist", BranchName: "boss/release-checklist",
			State:          pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN,
			PrNumber:       i32(WaitingCallbackPRNumber),
			DisplayLabel:   displaystatus.WaitingLabel,
			DisplayIntent:  pb.DisplayIntent_DISPLAY_INTENT_INFO,
			DisplaySpinner: true,
			CreatedAt:      ts(-3 * time.Hour),
			WorktreePath:   "/Users/demo/worktrees/my-app/release-checklist",
		},
		{
			Id: "sess-668-working", RepoId: "repo-1", RepoDisplayName: "my-app",
			Title: "Rebuild the search index", BranchName: "boss/search-index",
			State:          pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN,
			PrNumber:       i32(669),
			DisplayLabel:   "working",
			DisplayIntent:  pb.DisplayIntent_DISPLAY_INTENT_SUCCESS,
			DisplaySpinner: true,
			CreatedAt:      ts(-90 * time.Minute),
			WorktreePath:   "/Users/demo/worktrees/my-app/search-index",
		},
	}
}

// WaitingCallbackChats returns one chat per session, so each session's aggregate
// is unambiguous and the chat picker for the parked session shows a single
// waiting row plus its reason line.
func WaitingCallbackChats() []*pb.ClaudeChat {
	return []*pb.ClaudeChat{
		{Id: "chat-668-waiting", AgentSessionId: "claude-668-waiting", SessionId: "sess-668-waiting", Title: "Ship the release checklist", CreatedAt: ts(-3 * time.Hour)},
		{Id: "chat-668-working", AgentSessionId: "claude-668-working", SessionId: "sess-668-working", Title: "Rebuild the search index", CreatedAt: ts(-90 * time.Minute)},
	}
}

// WaitingCallbackChatStatuses returns the per-chat heartbeats: the parked chat
// carries CHAT_STATUS_WAITING plus the reason, the neighbour is working.
func WaitingCallbackChatStatuses() []*pb.ChatStatusEntry {
	return []*pb.ChatStatusEntry{
		{
			AgentSessionId: "claude-668-waiting",
			Status:         pb.ChatStatus_CHAT_STATUS_WAITING,
			WaitingReason:  WaitingCallbackReason,
			LastOutputAt:   ts(-40 * time.Minute),
		},
		{AgentSessionId: "claude-668-working", Status: pb.ChatStatus_CHAT_STATUS_WORKING, LastOutputAt: ts(-1 * time.Minute)},
	}
}

// WaitingCallbackSessionStatuses returns the aggregate per-session heartbeats.
// The home list reads the waiting reason from here (SessionStatusEntry), not
// from the Session, so the sub-row under the parked row only renders when this
// is seeded.
func WaitingCallbackSessionStatuses() []*pb.SessionStatusEntry {
	return []*pb.SessionStatusEntry{
		{
			SessionId:     "sess-668-waiting",
			Status:        pb.ChatStatus_CHAT_STATUS_WAITING,
			WaitingReason: WaitingCallbackReason,
		},
		{SessionId: "sess-668-working", Status: pb.ChatStatus_CHAT_STATUS_WORKING},
	}
}

// ArchiveSignalWorld builds the BOS-425 dataset: the demo repos (so
// archive-after-merge is configured) plus the two ArchiveSignal sessions and
// their chats. Used by the archive-signal proof scenarios.
func ArchiveSignalWorld() World {
	return World{
		Repos:    Repos(),
		Sessions: ArchiveSignalSessions(),
		Chats:    ArchiveSignalChats(),
	}
}

// ErroredStatusSessions returns two errored sessions (BOS-430) whose live chat
// is working, so the home list STATUS column must show the REAL underlying
// status recolored red (danger) rather than a static "orphaned"/"blocked"
// label. The mock daemon returns sessions verbatim, so the Display* fields are
// set here to exactly what displaystatus.Compute now produces for each — a
// working chat recolored DANGER with its spinner kept — plus the AttentionStatus
// that drives the "!" marker and the red subtext hint under the row:
//   - index 0 (sess-430-orphaned): State=ORPHANED — a headless run killed by a
//     daemon restart whose chat is still working; STATUS shows "working" (DANGER,
//     spinner) and the subtext explains the orphaned "why".
//   - index 1 (sess-430-blocked): State=BLOCKED — a blocked run whose chat is
//     working; STATUS shows "working" (DANGER, spinner) with a blocked subtext.
//
// Neither sets ArchivedAt, so both render on the default home list (the busy
// preset already lists ORPHANED/BLOCKED sessions, confirming they are not
// filtered out).
func ErroredStatusSessions() []*pb.Session {
	return []*pb.Session{
		{
			Id: "sess-430-orphaned", RepoId: "repo-1", RepoDisplayName: "my-app",
			// Title deliberately excludes the word "working" so the STATUS-column
			// "working" label binds uniquely in the proof scenario (it must not also
			// match a row title).
			Title: "Orphaned dark-mode run", BranchName: "boss/orphaned-working",
			State: pb.SessionState_SESSION_STATE_ORPHANED,
			// displaystatus.Compute: orphaned + WORKING chat → "working", DANGER,
			// spinner (the real status recolored red, not a static "orphaned").
			DisplayLabel:   "working",
			DisplayIntent:  pb.DisplayIntent_DISPLAY_INTENT_DANGER,
			DisplaySpinner: true,
			AttentionStatus: &pb.AttentionStatus{
				NeedsAttention: true,
				Reason:         pb.AttentionReason_ATTENTION_REASON_AWAITING_HUMAN_INPUT,
				Summary:        "orphaned — headless run killed by daemon restart; needs human",
			},
			CreatedAt:    ts(-2 * time.Hour),
			WorktreePath: "/Users/demo/worktrees/my-app/orphaned-working",
		},
		{
			Id: "sess-430-blocked", RepoId: "repo-1", RepoDisplayName: "my-app",
			// Title excludes "working" for the same STATUS-column binding reason.
			Title: "Blocked login-fix run", BranchName: "boss/blocked-working",
			State: pb.SessionState_SESSION_STATE_BLOCKED,
			// displaystatus.Compute: blocked + WORKING chat → "working", DANGER,
			// spinner (symmetric with orphaned).
			DisplayLabel:   "working",
			DisplayIntent:  pb.DisplayIntent_DISPLAY_INTENT_DANGER,
			DisplaySpinner: true,
			AttentionStatus: &pb.AttentionStatus{
				NeedsAttention: true,
				Reason:         pb.AttentionReason_ATTENTION_REASON_BLOCKED_MAX_ATTEMPTS,
				Summary:        "blocked — needs human intervention",
			},
			CreatedAt:    ts(-3 * time.Hour),
			WorktreePath: "/Users/demo/worktrees/my-app/blocked-working",
		},
	}
}

// ErroredStatusChats returns one chat per ErroredStatus session so both
// chatpickers render populated (not "Loading chats...").
func ErroredStatusChats() []*pb.ClaudeChat {
	return []*pb.ClaudeChat{
		{Id: "chat-430-o", AgentSessionId: "claude-430-o", SessionId: "sess-430-orphaned", Title: "Implement the change", CreatedAt: ts(-2 * time.Hour)},
		{Id: "chat-430-b", AgentSessionId: "claude-430-b", SessionId: "sess-430-blocked", Title: "Implement the change", CreatedAt: ts(-3 * time.Hour)},
	}
}

// ErroredStatusWorld builds the BOS-430 dataset: the demo repos plus the two
// errored (orphaned/blocked) working sessions and their chats. Used by the
// errored-status proof scenario.
func ErroredStatusWorld() World {
	return World{
		Repos:    Repos(),
		Sessions: ErroredStatusSessions(),
		Chats:    ErroredStatusChats(),
	}
}

// HTTPEndpointsSessions returns the BOS-474 dataset: one session running a dev
// server (:3000) and a Vite server (:5173) on loopback, plus a plain neighbour
// with no listeners. The pair proves both halves of the change — the endpoint
// session grows a ":3000 :5173" auxiliary row while the neighbour's rendering
// is untouched — and the two ports prove the single-space join (BOS-616) and
// the per-port hyperlinks.
func HTTPEndpointsSessions() []*pb.Session {
	return []*pb.Session{
		{
			Id: "sess-474-endpoints", RepoId: "repo-1", RepoDisplayName: "my-app",
			Title: "Local dev servers demo", BranchName: "boss/http-endpoints",
			State:        pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN,
			PrNumber:     i32(474),
			CreatedAt:    ts(-90 * time.Minute),
			WorktreePath: "/Users/demo/worktrees/my-app/http-endpoints",
			HttpEndpoints: []*pb.HttpEndpoint{
				{Port: 3000, Url: "http://127.0.0.1:3000"},
				{Port: 5173, Url: "http://127.0.0.1:5173"},
			},
		},
		{
			Id: "sess-474-plain", RepoId: "repo-1", RepoDisplayName: "my-app",
			Title: "No listeners demo", BranchName: "boss/no-listeners",
			State:        pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN,
			CreatedAt:    ts(-3 * time.Hour),
			WorktreePath: "/Users/demo/worktrees/my-app/no-listeners",
		},
	}
}

// HTTPEndpointsChats returns one chat per HTTPEndpoints session so both chat
// pickers render a populated table rather than "Loading chats...".
func HTTPEndpointsChats() []*pb.ClaudeChat {
	return []*pb.ClaudeChat{
		{Id: "chat-474-eps", AgentSessionId: "claude-474-eps", SessionId: "sess-474-endpoints", Title: "Run the dev servers", CreatedAt: ts(-80 * time.Minute)},
		{Id: "chat-474-plain", AgentSessionId: "claude-474-plain", SessionId: "sess-474-plain", Title: "Implement the change", CreatedAt: ts(-2 * time.Hour)},
	}
}

// HTTPEndpointsWorld builds the BOS-474 dataset: the demo repos plus the
// endpoint-bearing session, its endpoint-free neighbour, and their chats. Used
// by the bos-460-session-http-endpoints proof scenario.
func HTTPEndpointsWorld() World {
	return World{
		Repos:    Repos(),
		Sessions: HTTPEndpointsSessions(),
		Chats:    HTTPEndpointsChats(),
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
		Accounts: Accounts(),
	}
}
