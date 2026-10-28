package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/domain"
	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

// TestHeadlessCapabilityProfileFor locks the launch policy: only an unattended
// codex launch requires the tracker-plan-attachment surface. Every other
// combination must stay UNSPECIFIED so its launch path is byte-for-byte
// unchanged.
func TestHeadlessCapabilityProfileFor(t *testing.T) {
	tests := []struct {
		name  string
		agent string
		opts  StartSessionOpts
		want  pb.HeadlessCapabilityProfile
	}{
		{
			name:  "codex detach requires the tracker-plan-attachment surface",
			agent: "codex",
			opts:  StartSessionOpts{Detach: true},
			want:  pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1,
		},
		{
			name:  "codex tmux-unattended requires the tracker-plan-attachment surface",
			agent: "codex",
			opts:  StartSessionOpts{IsTmuxUnattended: true},
			want:  pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1,
		},
		{
			name:  "codex cron requires the tracker-plan-attachment surface",
			agent: "codex",
			opts:  StartSessionOpts{CronJobID: "cron-1"},
			want:  pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1,
		},
		{
			name:  "codex zero-output cron still requires the surface",
			agent: "codex",
			opts:  StartSessionOpts{CronJobID: "cron-1", ZeroOutput: true},
			want:  pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1,
		},
		{
			name:  "codex interactive requires nothing",
			agent: "codex",
			opts:  StartSessionOpts{},
			want:  pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_UNSPECIFIED,
		},
		{
			name:  "claude detach requires nothing",
			agent: "claude",
			opts:  StartSessionOpts{Detach: true},
			want:  pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_UNSPECIFIED,
		},
		{
			name:  "claude interactive requires nothing",
			agent: "claude",
			opts:  StartSessionOpts{},
			want:  pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_UNSPECIFIED,
		},
		{
			name:  "unresolved empty agent name never opts into a new gate",
			agent: "",
			opts:  StartSessionOpts{Detach: true},
			want:  pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_UNSPECIFIED,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := headlessCapabilityProfileFor(tc.agent, "linear", tc.opts); got != tc.want {
				t.Fatalf("headlessCapabilityProfileFor(%q, %+v) = %v, want %v", tc.agent, tc.opts, got, tc.want)
			}
		})
	}
}

// TestHeadlessCapabilityProfileForAutonomousRun locks the agent-keyed half of
// the policy in isolation. Every enforcement point now shares this function, so
// "which agents need a profile" is answered in exactly one place: if this table
// and TestHeadlessCapabilityProfileFor ever disagree, the interactivity check
// and the agent check have drifted apart.
//
// The caller has already established the run is autonomous, so there is no opts
// argument to vary — only the name matters, and an unresolved (empty) name must
// still decline to opt a launch into a gate its runner may not implement.
func TestHeadlessCapabilityProfileForAutonomousRun(t *testing.T) {
	tests := []struct {
		name  string
		agent string
		want  pb.HeadlessCapabilityProfile
	}{
		{
			name:  "codex requires the tracker-plan-attachment surface",
			agent: "codex",
			want:  pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1,
		},
		{
			name:  "claude requires nothing",
			agent: "claude",
			want:  pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_UNSPECIFIED,
		},
		{
			name:  "an unrecognized runner requires nothing",
			agent: "opencode",
			want:  pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_UNSPECIFIED,
		},
		{
			name:  "unresolved empty agent name never opts into a new gate",
			agent: "",
			want:  pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_UNSPECIFIED,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := headlessCapabilityProfileForAutonomousRun(tc.agent, "linear"); got != tc.want {
				t.Fatalf("headlessCapabilityProfileForAutonomousRun(%q) = %v, want %v", tc.agent, got, tc.want)
			}
		})
	}
}

// TestHeadlessCapabilityProfileForIsSideEffectFree pins that the policy does
// not mutate the opts it is handed — it is read-only over the launch signals.
func TestHeadlessCapabilityProfileForDoesNotMutateOpts(t *testing.T) {
	opts := StartSessionOpts{Detach: true, CronJobID: "cron-9", BranchName: "b"}
	before := opts
	_ = headlessCapabilityProfileFor(agentNameCodex, "linear", opts)
	if opts != before {
		t.Fatalf("opts mutated: %+v, want %+v", opts, before)
	}
}

func TestHeadlessCapabilityProfilesByTracker_Sessions_AC109_AC110(t *testing.T) {
	for _, tracker := range []string{"github", "linear", "jira", ""} {
		for _, agentName := range []string{"codex", "claude", ""} {
			t.Run(agentName+"/"+tracker, func(t *testing.T) {
				want := pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_UNSPECIFIED
				if agentName == "codex" && tracker != "github" {
					want = pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1
				}
				if got := headlessCapabilityProfileForAutonomousRun(agentName, tracker); got != want {
					t.Fatalf("autonomous profile = %v, want %v", got, want)
				}
				for _, opts := range []StartSessionOpts{{Detach: true}, {CronJobID: "cron"}, {IsTmuxUnattended: true}} {
					if got := headlessCapabilityProfileFor(agentName, tracker, opts); got != want {
						t.Fatalf("profile for %+v = %v, want %v", opts, got, want)
					}
				}
				if got := headlessCapabilityProfileFor(agentName, tracker, StartSessionOpts{}); got != pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_UNSPECIFIED {
					t.Fatalf("interactive profile = %v", got)
				}
			})
		}
	}
}

func TestResolveTrackerAdapter_Sessions_AC110(t *testing.T) {
	for _, tc := range []struct {
		name, worktree, repo, want string
		emptyWorktree              bool
	}{
		{name: "missing", want: "linear"},
		{name: "empty config", worktree: `{}`, want: "linear"},
		{name: "github", worktree: `{"adapters":{"tracker":"github"}}`, want: "github"},
		{name: "malformed", worktree: `{`, want: "linear"},
		{name: "non string", worktree: `{"adapters":{"tracker":123}}`, want: "linear"},
		{name: "empty tracker", worktree: `{"adapters":{"tracker":""}}`, want: "linear"},
		{name: "worktree wins", worktree: `{"adapters":{"tracker":"github"}}`, repo: `{"adapters":{"tracker":"linear"}}`, want: "github"},
		{name: "malformed wins", worktree: `{`, repo: `{"adapters":{"tracker":"github"}}`, want: "linear"},
		{name: "missing worktree falls back", repo: `{"adapters":{"tracker":"github"}}`, want: "github"},
		{name: "empty worktree falls back", emptyWorktree: true, repo: `{"adapters":{"tracker":"github"}}`, want: "github"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			worktree, repo := t.TempDir(), t.TempDir()
			for dir, data := range map[string]string{worktree: tc.worktree, repo: tc.repo} {
				if data != "" {
					if err := os.WriteFile(filepath.Join(dir, ".boss-skills.json"), []byte(data), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if tc.emptyWorktree {
				worktree = ""
			}
			if got := resolveTrackerAdapter(worktree, repo); got != tc.want {
				t.Fatalf("tracker = %q, want %q", got, tc.want)
			}
		})
	}
	if got := (*Lifecycle)(nil).trackerAdapterForSession(nil, nil); got != "linear" {
		t.Fatalf("nil tracker = %q", got)
	}
}

// TestStartSession_AppliesHeadlessCapabilityProfilePolicy locks the launch
// policy where it actually matters: inside StartSession, over the real
// preflight seam, for every agent/interactivity combination that reaches the
// headless or idle branch. Only an unattended codex launch may reach the gate.
func TestStartSession_AppliesHeadlessCapabilityProfilePolicy_Sessions_AC109_AC110(t *testing.T) {
	tests := []struct {
		name          string
		agentName     string
		tracker       string
		opts          StartSessionOpts
		wantPreflight bool
		wantStarts    int
	}{
		{
			name:          "codex detach derives the tracker-plan-attachment profile",
			agentName:     "codex",
			opts:          StartSessionOpts{Detach: true, DeferPR: true},
			wantPreflight: true,
			wantStarts:    1,
		},
		{
			name:       "github codex detach skips connector preflight",
			agentName:  "codex",
			tracker:    "github",
			opts:       StartSessionOpts{Detach: true, DeferPR: true},
			wantStarts: 1,
		},
		{
			name:       "interactive codex stays unprofiled",
			agentName:  "codex",
			opts:       StartSessionOpts{DeferPR: true},
			wantStarts: 0,
		},
		{
			name:       "claude detach stays unprofiled",
			agentName:  "claude",
			opts:       StartSessionOpts{Detach: true, DeferPR: true},
			wantStarts: 1,
		},
		{
			name:       "interactive claude stays unprofiled",
			agentName:  "claude",
			opts:       StartSessionOpts{DeferPR: true},
			wantStarts: 0,
		},
		{
			// mockAgentRunner is not an agent.NameResolver, so the
			// resolved name is the raw empty string: an unresolved agent must
			// never opt a launch into a gate its runner may not implement.
			name:       "unresolved empty agent name stays unprofiled",
			agentName:  "",
			opts:       StartSessionOpts{Detach: true, DeferPR: true},
			wantStarts: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			sessions := newMockSessionStore()
			repos := newMockRepoStore()
			worktrees := &mockWorktreeManager{}
			runner := newMockAgentRunner()
			repoDir := t.TempDir()
			if tc.tracker != "" {
				if err := os.WriteFile(filepath.Join(repoDir, ".boss-skills.json"), []byte(`{"adapters":{"tracker":"`+tc.tracker+`"}}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			repos.repos["repo-1"] = &models.Repo{
				ID:                "repo-1",
				LocalPath:         repoDir,
				DefaultBaseBranch: "main",
				WorktreeBaseDir:   "/tmp/worktrees",
			}
			sessions.sessions["sess-1"] = &models.Session{
				ID:         "sess-1",
				RepoID:     "repo-1",
				Title:      "Policy",
				Plan:       "do work",
				BaseBranch: "main",
				AgentName:  tc.agentName,
				Model:      "some-model",
				State:      machine.CreatingWorktree,
			}
			lifecycle := newTestLifecycle(sessions, repos, &mockAgentChatStore{}, nil, worktrees, runner, nil, newMockVCSProvider(), zerolog.Nop())

			if err := lifecycle.StartSession(ctx, "sess-1", tc.opts); err != nil {
				t.Fatalf("StartSession: %v", err)
			}

			wantCalls := 0
			if tc.wantPreflight {
				wantCalls = 1
			}
			if runner.preflightCalls != wantCalls || len(runner.preflights) != wantCalls {
				t.Fatalf("preflight calls = %d, records = %d; want %d each", runner.preflightCalls, len(runner.preflights), wantCalls)
			}
			if tc.wantPreflight {
				got := runner.preflights[0]
				if got.agentName != tc.agentName {
					t.Errorf("preflight agent = %q, want %q", got.agentName, tc.agentName)
				}
				if got.profile != pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1 {
					t.Errorf("preflight profile = %s, want tracker-plan-attachment-v1", got.profile)
				}
			}
			if len(runner.started) != tc.wantStarts {
				t.Fatalf("agent starts = %d, want %d", len(runner.started), tc.wantStarts)
			}
			if tc.wantStarts == 1 {
				wantProfile := pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_UNSPECIFIED
				if tc.wantPreflight {
					wantProfile = pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1
				}
				if got := runner.started[0].profile; got != wantProfile {
					t.Errorf("headless start profile = %s, want %s", got, wantProfile)
				}
			}
		})
	}
}

func TestStartTmuxChat_AutonomousGithubCodexSkipsCapabilityProfile_Sessions_AC109(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	h := newStartTmuxChatHarness(t)
	h.sessions.sessions["sess-1"].AgentName = "codex"
	h.lc.SetAgents(map[string]agent.RunnerClient{"codex": h.agentFake})
	h.sessions.sessions["sess-1"].WorktreePath = t.TempDir()
	if err := os.WriteFile(filepath.Join(h.sessions.sessions["sess-1"].WorktreePath, ".boss-skills.json"), []byte(`{"adapters":{"tracker":"github"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	h.agentRun.preflightErr = grpcstatus.Error(codes.FailedPrecondition, "connector_plugin_disabled_or_missing")
	if _, err := h.lc.StartTmuxChat(context.Background(), "sess-1", ChatInput{Prompt: "/boss-repair", Delivery: DeliverySubmit}, "title", HookOpts{AutonomousRun: true}); err != nil {
		t.Fatal(err)
	}
	if h.agentRun.preflightCalls != 0 {
		t.Fatalf("preflight calls = %d, want 0", h.agentRun.preflightCalls)
	}
}

func TestSwitchAccount_GithubCodexRespawnSkipsCapabilityProfile_Sessions_AC109(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	for _, repair := range []bool{false, true} {
		t.Run(fmt.Sprint("repair=", repair), func(t *testing.T) {
			h := newSwitchHarness(t)
			h.sessions.sessions["sess-1"].IsTmuxUnattended = !repair
			if repair {
				h.chats.chatsBySession["sess-1"][0].Role = domain.AgentChatRoleRepair
			}
			h.chats.chatsBySession["sess-1"][0].AgentName = "codex"
			h.lc.SetAgents(map[string]agent.RunnerClient{"claude": h.agentFake, "codex": h.agentFake})
			h.lc.accountSwitchRegistry = stubSwitchRegistry{acct: switchAccount{ID: "acct-2", Provider: "codex", Label: "Codex Work", Status: AgentAccountActive}}
			h.sessions.sessions["sess-1"].WorktreePath = t.TempDir()
			if err := os.WriteFile(filepath.Join(h.sessions.sessions["sess-1"].WorktreePath, ".boss-skills.json"), []byte(`{"adapters":{"tracker":"github"}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := h.lc.SwitchAgentAccount(context.Background(), SwitchAgentAccountParams{SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "acct-2"}); err != nil {
				t.Fatal(err)
			}
			if h.agentRun.preflightCalls != 0 {
				t.Fatalf("preflight calls = %d, want 0", h.agentRun.preflightCalls)
			}
		})
	}
}

func TestReadTrackerConfig_GitHubRepository(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".boss-skills.json"), []byte(`{"adapters":{"tracker":"github"},"trackerConfig":{"github":{"repository":"ghe.example/o/r"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	got := ReadTrackerConfig(dir)
	if got.Adapter != "github" || got.GitHubRepository != "ghe.example/o/r" {
		t.Fatalf("config = %+v", got)
	}
}
