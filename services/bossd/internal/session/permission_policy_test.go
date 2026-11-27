package session

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/adapter/tmux"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/domain"
)

func TestUnattendedPermissionPolicy(t *testing.T) {
	t.Run("pure builder base, lockfiles, trusted commands, filtering, dedup and order", func(t *testing.T) {
		tests := []struct {
			name         string
			inputs       PolicyInputs
			wantWorktree string
			wantPrefixes []string
		}{
			{
				name: "base set only",
				inputs: PolicyInputs{
					WorktreePath: "/tmp/wt/repo/.",
				},
				wantWorktree: "/tmp/wt/repo",
				wantPrefixes: []string{"git", "gh", "boss", "node", "make"},
			},
			{
				name: "every lockfile maps in stable canonical order and deduplicates bun",
				inputs: PolicyInputs{
					WorktreePath: "/tmp/wt",
					Lockfiles: []string{
						"uv.lock",
						"Cargo.lock",
						"go.sum",
						"bun.lockb",
						"bun.lock",
						"yarn.lock",
						"package-lock.json",
						"pnpm-lock.yaml",
						"unknown.lock",
					},
				},
				wantWorktree: "/tmp/wt",
				wantPrefixes: []string{"git", "gh", "boss", "node", "make", "pnpm", "npm", "yarn", "bun", "go", "cargo", "uv"},
			},
			{
				name: "trusted repo commands add sorted unique safe prefixes and skip NAME=value",
				inputs: PolicyInputs{
					WorktreePath:  "/tmp/wt",
					Lockfiles:     []string{"go.sum"},
					IsRepoTrusted: true,
					SkillsCommands: map[string]string{
						"z_lint":      "golangci-lint run ./...",
						"a_test":      "BOSS_GATE_FORCE_UNCACHED=1 make test-full",
						"b_build":     "  FOO=1   BAR=a/b:c   bazel build //...  ",
						"c_dup":       "go test ./...",
						"d_plus":      "g++ main.cc",
						"e_ver":       "go1.22 test",
						"f_only_env":  "FOO=1 BAR=2",
						"g_blank":     "   \t  ",
						"h_dup_bazel": "bazel test //...",
					},
				},
				wantWorktree: "/tmp/wt",
				wantPrefixes: []string{"git", "gh", "boss", "node", "make", "go", "bazel", "g++", "go1.22", "golangci-lint"},
			},
			{
				name: "untrusted repo never widens list even if SkillsCommands populated",
				inputs: PolicyInputs{
					WorktreePath:  "/tmp/wt",
					Lockfiles:     []string{"pnpm-lock.yaml"},
					IsRepoTrusted: false,
					SkillsCommands: map[string]string{
						"build": "bazel build //...",
						"test":  "pytest -q",
					},
				},
				wantWorktree: "/tmp/wt",
				wantPrefixes: []string{"git", "gh", "boss", "node", "make", "pnpm"},
			},
			{
				name: "drops all interpreters and wrappers case-insensitively",
				inputs: PolicyInputs{
					WorktreePath:  "/tmp/wt",
					IsRepoTrusted: true,
					SkillsCommands: map[string]string{
						"01": "sh -c true",
						"02": "bash -c true",
						"03": "zsh -c true",
						"04": "fish -c true",
						"05": "env FOO=1 bazel test",
						"06": "sudo make install",
						"07": "xargs echo",
						"08": "eval echo",
						"09": "exec echo",
						"10": "python -m pytest",
						"11": "python3 -m pytest",
						"12": "ruby -e 1",
						"13": "perl -e 1",
						"14": "npx vitest",
						"15": "bunx vitest",
						"16": "BASH -c true",
						"17": "Python3 -m unittest",
						"18": "FOO=1 sh -c make",
						"19": "pytest -q",
					},
				},
				wantWorktree: "/tmp/wt",
				wantPrefixes: []string{"git", "gh", "boss", "node", "make", "pytest"},
			},
			{
				name: "drops hostile words with whitespace, slashes, or shell/rule metacharacters",
				inputs: PolicyInputs{
					WorktreePath:  "/tmp/wt",
					IsRepoTrusted: true,
					SkillsCommands: map[string]string{
						"01_relpath":   "./scripts/test.sh",
						"02_abspath":   "/usr/bin/bazel test",
						"03_backslash": `foo\bar`,
						"04_semi":      "foo;rm -rf /",
						"05_pipe":      "foo|bar",
						"06_amp":       "foo&bar",
						"07_dollar":    "$CMD test",
						"08_backtick":  "`cmd` test",
						"09_parens":    "(cmd)",
						"10_redir_out": "cmd>out",
						"11_redir_in":  "cmd<in",
						"12_glob_star": "cmd*",
						"13_glob_q":    "cmd?",
						"14_bracket":   "cmd[0]",
						"15_brace":     "{cmd}",
						"16_squote":    "'cmd'",
						"17_dquote":    `"cmd"`,
						"18_bang":      "!cmd",
						"19_hash":      "#cmd",
						"20_tilde":     "~cmd",
						"21_colon":     "cmd:arg",
						"22_dot":       ". ./env.sh",
						"23_dotdot":    ".. arg",
						"24_flag":      "-flag arg",
						"25_newline":   "foo\nbar",
						"26_cr":        "foo\rbar",
						"27_vtab":      "foo\vbar",
						"28_bad_env":   "FOO=$(evil) bazel test",
						"29_bad_env2":  "FOO=1;evil bazel test",
						"30_safe":      "vitest run",
					},
				},
				wantWorktree: "/tmp/wt",
				wantPrefixes: []string{"git", "gh", "boss", "node", "make", "vitest"},
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				got := BuildUnattendedPermissionPolicy(tc.inputs)
				if got.WorktreePath != tc.wantWorktree {
					t.Errorf("WorktreePath = %q, want %q", got.WorktreePath, tc.wantWorktree)
				}
				if !got.IsNetworkAllowed {
					t.Errorf("IsNetworkAllowed = false, want true")
				}
				if !slices.Equal(got.AllowedCommandPrefixes, tc.wantPrefixes) {
					t.Errorf("AllowedCommandPrefixes = %v, want %v", got.AllowedCommandPrefixes, tc.wantPrefixes)
				}
			})
		}
	})

	t.Run("each lockfile on disk maps to its package manager", func(t *testing.T) {
		for _, tc := range []struct {
			lockfile string
			manager  string
		}{
			{"pnpm-lock.yaml", "pnpm"},
			{"package-lock.json", "npm"},
			{"yarn.lock", "yarn"},
			{"bun.lock", "bun"},
			{"bun.lockb", "bun"},
			{"go.sum", "go"},
			{"Cargo.lock", "cargo"},
			{"uv.lock", "uv"},
		} {
			t.Run(tc.lockfile, func(t *testing.T) {
				wt := t.TempDir()
				if err := os.WriteFile(filepath.Join(wt, tc.lockfile), []byte("lock"), 0o600); err != nil {
					t.Fatal(err)
				}
				got := UnattendedPermissionPolicyFor(wt, nil)
				want := []string{"git", "gh", "boss", "node", "make", tc.manager}
				if !slices.Equal(got.AllowedCommandPrefixes, want) {
					t.Errorf("AllowedCommandPrefixes = %v, want %v", got.AllowedCommandPrefixes, want)
				}
				if got.WorktreePath != wt {
					t.Errorf("WorktreePath = %q, want %q", got.WorktreePath, wt)
				}
				if !got.IsNetworkAllowed {
					t.Error("IsNetworkAllowed = false, want true")
				}
			})
		}
	})

	t.Run("disk .boss-skills.json respected only for trusted repos", func(t *testing.T) {
		wt := t.TempDir()
		if err := os.WriteFile(filepath.Join(wt, "go.sum"), []byte(""), 0o600); err != nil {
			t.Fatal(err)
		}
		skillsJSON := `{
			"commands": {
				"test": "BOSS_GATE_FORCE_UNCACHED=1 bazel test //...",
				"lint": "golangci-lint run",
				"shell": "bash -c evil",
				"nested": {"ignored": "true"},
				"count": 42
			}
		}`
		if err := os.WriteFile(filepath.Join(wt, ".boss-skills.json"), []byte(skillsJSON), 0o600); err != nil {
			t.Fatal(err)
		}

		trusted := UnattendedPermissionPolicyFor(wt, &models.Repo{IsTrusted: true})
		wantTrusted := []string{"git", "gh", "boss", "node", "make", "go", "golangci-lint", "bazel"}
		if !slices.Equal(trusted.AllowedCommandPrefixes, wantTrusted) {
			t.Errorf("trusted prefixes = %v, want %v", trusted.AllowedCommandPrefixes, wantTrusted)
		}

		for name, repo := range map[string]*models.Repo{
			"nil repo":       nil,
			"untrusted repo": {IsTrusted: false},
		} {
			got := UnattendedPermissionPolicyFor(wt, repo)
			wantUntrusted := []string{"git", "gh", "boss", "node", "make", "go"}
			if !slices.Equal(got.AllowedCommandPrefixes, wantUntrusted) {
				t.Errorf("%s prefixes = %v, want %v", name, got.AllowedCommandPrefixes, wantUntrusted)
			}
		}
	})

	t.Run("invalid .boss-skills.json and empty worktree degrade cleanly", func(t *testing.T) {
		wt := t.TempDir()
		if err := os.WriteFile(filepath.Join(wt, ".boss-skills.json"), []byte("{not-json"), 0o600); err != nil {
			t.Fatal(err)
		}
		got := UnattendedPermissionPolicyFor(wt, &models.Repo{IsTrusted: true})
		want := []string{"git", "gh", "boss", "node", "make"}
		if !slices.Equal(got.AllowedCommandPrefixes, want) {
			t.Errorf("invalid json prefixes = %v, want %v", got.AllowedCommandPrefixes, want)
		}

		empty := UnattendedPermissionPolicyFor("", &models.Repo{IsTrusted: true})
		if empty.WorktreePath != "" || !slices.Equal(empty.AllowedCommandPrefixes, want) {
			t.Errorf("empty worktree policy = %+v, want empty worktree and base prefixes", empty)
		}
	})
}

func TestUnattendedLaunchCarriesPolicy_Sessions_AC267803(t *testing.T) {
	wt := t.TempDir()
	if err := os.WriteFile(filepath.Join(wt, "pnpm-lock.yaml"), []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".boss-skills.json"), []byte(`{"commands":{"test":"pytest -q"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name           string
		mutateSession  func(s *models.Session)
		repoTrusted    bool
		hookOpts       HookOpts
		wantPolicy     bool
		wantExtraWords []string
	}{
		{
			name: "cron session carries policy",
			mutateSession: func(s *models.Session) {
				s.CronJobID = ptr("cron-1")
			},
			repoTrusted:    true,
			wantPolicy:     true,
			wantExtraWords: []string{"pnpm", "pytest"},
		},
		{
			name: "tmux-unattended session carries policy",
			mutateSession: func(s *models.Session) {
				s.IsTmuxUnattended = true
			},
			repoTrusted:    true,
			wantPolicy:     true,
			wantExtraWords: []string{"pnpm", "pytest"},
		},
		{
			name: "detach session carries policy",
			mutateSession: func(s *models.Session) {
				s.Detach = true
			},
			repoTrusted:    true,
			wantPolicy:     true,
			wantExtraWords: []string{"pnpm", "pytest"},
		},
		{
			name: "autonomous hook launch on attended session carries policy",
			mutateSession: func(s *models.Session) {
				// Parent session itself is attended; AutonomousRun widens this launch.
			},
			repoTrusted:    true,
			hookOpts:       HookOpts{AutonomousRun: true},
			wantPolicy:     true,
			wantExtraWords: []string{"pnpm", "pytest"},
		},
		{
			name: "untrusted cron session omits skills commands",
			mutateSession: func(s *models.Session) {
				s.CronJobID = ptr("cron-2")
			},
			repoTrusted:    false,
			wantPolicy:     true,
			wantExtraWords: []string{"pnpm"},
		},
		{
			name: "attended chat omits policy",
			mutateSession: func(s *models.Session) {
				// No cron, tmux-unattended, detach, or autonomous flag.
			},
			repoTrusted: true,
			wantPolicy:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newStartTmuxChatHarness(t)
			h.repos.repos["repo-abcdef12"].IsTrusted = tc.repoTrusted
			sess := h.sessions.sessions["sess-1"]
			sess.WorktreePath = wt
			tc.mutateSession(sess)
			h.agentFake.ConsumesInitialInput = true

			_, err := h.lc.StartTmuxChat(t.Context(), "sess-1", ChatInput{Prompt: "run task", Delivery: DeliverySubmit}, "Chat", tc.hookOpts)
			if err != nil {
				t.Fatalf("StartTmuxChat: %v", err)
			}
			req := h.agentFake.LastBuildInteractiveCommand
			if req == nil {
				t.Fatal("BuildInteractiveCommand was not called")
			}
			got := req.GetUnattendedPermissionPolicy()
			if !tc.wantPolicy {
				if got != nil {
					t.Fatalf("UnattendedPermissionPolicy = %+v, want nil for attended chat", got)
				}
				return
			}
			if got == nil {
				t.Fatal("UnattendedPermissionPolicy = nil, want non-nil policy")
			}
			if got.GetWorktreePath() != wt {
				t.Errorf("WorktreePath = %q, want %q", got.GetWorktreePath(), wt)
			}
			if !got.GetIsNetworkAllowed() {
				t.Error("IsNetworkAllowed = false, want true")
			}
			wantPrefixes := append([]string{"git", "gh", "boss", "node", "make"}, tc.wantExtraWords...)
			if !slices.Equal(got.GetAllowedCommandPrefixes(), wantPrefixes) {
				t.Errorf("AllowedCommandPrefixes = %v, want %v", got.GetAllowedCommandPrefixes(), wantPrefixes)
			}
		})
	}

	t.Run("sendInputToLiveTmuxChat leaves policy nil", func(t *testing.T) {
		h := newStartTmuxChatHarness(t)
		sess := h.sessions.sessions["sess-1"]
		sess.WorktreePath = wt
		sess.IsTmuxUnattended = true
		h.tmuxFake.hasSessionFunc = func([]recordedTmuxCall) bool { return true }
		h.tmuxFake.capturePaneOutput = "❯\n"
		h.agentFake.ReadyMarker = "❯"
		tmuxName := "boss-live-chat"
		chat := &domain.AgentChat{
			ID:              "chat-1",
			SessionID:       sess.ID,
			AgentSessionID:  "agent-chat-1",
			AgentName:       "claude",
			TmuxSessionName: &tmuxName,
		}
		if _, err := h.lc.sendInputToLiveTmuxChat(t.Context(), sess, h.agentFake, ChatInput{Prompt: "follow up", Delivery: DeliverySubmit}, chat, "agent-chat-1", HookOpts{}); err != nil {
			t.Fatalf("sendInputToLiveTmuxChat: %v", err)
		}
		req := h.agentFake.LastBuildInteractiveCommand
		if req == nil {
			t.Fatal("BuildInteractiveCommand was not called")
		}
		if got := req.GetUnattendedPermissionPolicy(); got != nil {
			t.Fatalf("sendInputToLiveTmuxChat UnattendedPermissionPolicy = %+v, want nil", got)
		}
	})
}

func TestHeadlessRunsForwardPolicy_Lifecycle(t *testing.T) {
	wtDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wtDir, "Cargo.lock"), []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtDir, ".boss-skills.json"), []byte(`{"commands":{"test":"BOSS_GATE=1 bazel test //..."}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, trusted := range []bool{true, false} {
		label := "untrusted"
		wantPrefixes := []string{"git", "gh", "boss", "node", "make", "cargo"}
		if trusted {
			label = "trusted"
			wantPrefixes = append(wantPrefixes, "bazel")
		}

		t.Run("fresh headless start/"+label, func(t *testing.T) {
			sessions := newMockSessionStore()
			repos := newMockRepoStore()
			chats := &mockAgentChatStore{}
			wt := &mockWorktreeManager{worktreePath: wtDir}
			runner := newMockAgentRunner()
			provider := newMockVCSProvider()
			fake := newFakeTmux()
			fake.available = false // Force default: headless branch.
			tx := tmux.NewClient(tmux.WithPasteSettleWait(testPasteSettleWait), tmux.WithCommandFactory(fake.factory))
			repos.repos["repo-1"] = &models.Repo{
				ID: "repo-1", LocalPath: "/tmp/repo", DefaultBaseBranch: "main",
				WorktreeBaseDir: "/tmp/worktrees", OriginURL: "owner/repo", IsTrusted: trusted,
			}
			sessions.sessions["sess-1"] = &models.Session{
				ID: "sess-1", RepoID: "repo-1", Title: "Headless run", Plan: "implement change",
				BaseBranch: "main", PRNumber: ptr(42), AgentName: "claude", State: machine.CreatingWorktree,
			}
			lc := newTestLifecycle(sessions, repos, chats, nil, wt, runner, tx, provider, zerolog.Nop())
			lc.SetAgents(map[string]agent.RunnerClient{"claude": newFakeAgent()})
			lc.SetAgentLogsDir(t.TempDir())

			if err := lc.StartSession(context.Background(), "sess-1", StartSessionOpts{
				Detach: true, SkipSetupScript: true,
			}); err != nil {
				t.Fatalf("StartSession: %v", err)
			}
			awaitDraftPR(t, lc, "sess-1")
			if len(runner.started) != 1 {
				t.Fatalf("headless starts = %d, want 1", len(runner.started))
			}
			got := runner.started[0].unattendedPermissionPolicy
			assertPolicyMatches(t, got, wtDir, wantPrefixes)
		})

		t.Run("replacement headless run/"+label, func(t *testing.T) {
			runner := newMockAgentRunner()
			lc := newTestLifecycle(newMockSessionStore(), newMockRepoStore(), nil, nil, nil, runner, nil, newMockVCSProvider(), zerolog.Nop())
			resume := "prior-agent-session"
			repo := &models.Repo{IsTrusted: trusted}
			if _, err := lc.startHeadlessReplacementRun(context.Background(), &models.Session{ID: "sess-1", AgentName: "codex"}, repo, wtDir, "resume work", &resume, nil); err != nil {
				t.Fatalf("startHeadlessReplacementRun: %v", err)
			}
			if len(runner.started) != 1 {
				t.Fatalf("replacement starts = %d, want 1", len(runner.started))
			}
			assertPolicyMatches(t, runner.started[0].unattendedPermissionPolicy, wtDir, wantPrefixes)
		})
	}
}

func assertPolicyMatches(t *testing.T, got *bossanovav1.UnattendedPermissionPolicy, wantWorktree string, wantPrefixes []string) {
	t.Helper()
	if got == nil {
		t.Fatal("UnattendedPermissionPolicy = nil, want non-nil")
	}
	if got.GetWorktreePath() != wantWorktree {
		t.Errorf("WorktreePath = %q, want %q", got.GetWorktreePath(), wantWorktree)
	}
	if !got.GetIsNetworkAllowed() {
		t.Error("IsNetworkAllowed = false, want true")
	}
	if !slices.Equal(got.GetAllowedCommandPrefixes(), wantPrefixes) {
		t.Errorf("AllowedCommandPrefixes = %v, want %v", got.GetAllowedCommandPrefixes(), wantPrefixes)
	}
}

// TestSwitchAccount_RepairChatOnInteractiveSessionPreservesUnattendedPolicy
// proves that rotating an autonomous repair chat belonging to an otherwise
// interactive session keeps both UnattendedPermissionPolicy and unattended env
// markers (BOSS_UNATTENDED/BOSS_CRON), while rotating a regular attended chat
// on an interactive session still omits the policy.
func TestSwitchAccount_RepairChatOnInteractiveSessionPreservesUnattendedPolicy(t *testing.T) {
	wt := t.TempDir()
	if err := os.WriteFile(filepath.Join(wt, "go.sum"), []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name       string
		role       domain.AgentChatRole
		agentName  string
		wantPolicy bool
	}{
		{
			name:       "claude repair chat on interactive session keeps policy",
			role:       domain.AgentChatRoleRepair,
			agentName:  "claude",
			wantPolicy: true,
		},
		{
			name:       "codex repair chat on interactive session keeps policy and capability profile",
			role:       domain.AgentChatRoleRepair,
			agentName:  "codex",
			wantPolicy: true,
		},
		{
			name:       "regular attended chat on interactive session omits policy",
			role:       "",
			agentName:  "claude",
			wantPolicy: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newSwitchHarness(t)
			h.sessions.sessions["sess-1"].WorktreePath = wt
			h.chats.chatsBySession["sess-1"][0].Role = tc.role
			h.chats.chatsBySession["sess-1"][0].AgentName = tc.agentName
			h.lc.SetAgents(map[string]agent.RunnerClient{"claude": h.agentFake, "codex": h.agentFake})
			h.lc.accountSwitchRegistry = stubSwitchRegistry{acct: switchAccount{
				ID: "acct-2", Provider: tc.agentName, Label: "Backup", Status: AgentAccountActive,
			}}

			if _, err := h.lc.SwitchAgentAccount(context.Background(), SwitchAgentAccountParams{
				SessionID:       "sess-1",
				AgentSessionID:  "agent-1",
				TargetAccountID: "acct-2",
				Auto:            true,
			}); err != nil {
				t.Fatalf("SwitchAgentAccount: %v", err)
			}

			req := h.agentFake.LastBuildInteractiveCommand
			if req == nil {
				t.Fatal("expected respawn BuildInteractiveCommand call")
			}
			got := req.GetUnattendedPermissionPolicy()
			if !tc.wantPolicy {
				if got != nil {
					t.Fatalf("UnattendedPermissionPolicy = %+v, want nil for regular interactive chat", got)
				}
				return
			}
			assertPolicyMatches(t, got, wt, []string{"git", "gh", "boss", "node", "make", "go"})
			newSess := h.findCall("new-session")
			if newSess == nil {
				t.Fatal("expected tmux new-session call")
			}
			joined := strings.Join(newSess.args, "\n")
			if !strings.Contains(joined, "BOSS_UNATTENDED=true") || !strings.Contains(joined, "BOSS_CRON=true") {
				t.Fatalf("rotated repair chat tmux env missing unattended markers: %v", newSess.args)
			}
		})
	}
}
