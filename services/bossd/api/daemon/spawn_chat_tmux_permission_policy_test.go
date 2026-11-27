package server

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	bossalog "github.com/recurser/bossalib/kit/log"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/domain"
)

func TestUnattendedLaunchCarriesPolicy_Respawn(t *testing.T) {
	wt := t.TempDir()
	if err := os.WriteFile(filepath.Join(wt, "go.sum"), []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".boss-skills.json"), []byte(`{"commands":{"test":"bazel test //..."}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("spawnChatTmux carries policy only when IsUnattended is true", func(t *testing.T) {
		for _, tc := range []struct {
			name         string
			isUnattended bool
			repo         *models.Repo
			wantPolicy   bool
			wantPrefixes []string
		}{
			{
				name:         "unattended trusted repo",
				isUnattended: true,
				repo:         &models.Repo{LocalPath: t.TempDir(), IsTrusted: true},
				wantPolicy:   true,
				wantPrefixes: []string{"git", "gh", "boss", "node", "make", "go", "bazel"},
			},
			{
				name:         "unattended untrusted repo",
				isUnattended: true,
				repo:         &models.Repo{LocalPath: t.TempDir(), IsTrusted: false},
				wantPolicy:   true,
				wantPrefixes: []string{"git", "gh", "boss", "node", "make", "go"},
			},
			{
				name:         "attended chat omits policy",
				isUnattended: false,
				repo:         &models.Repo{LocalPath: t.TempDir(), IsTrusted: true},
				wantPolicy:   false,
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				builder := claudeArgvBuilder()
				wantLogPath := filepath.Join(t.TempDir(), "agent.log")
				if _, err := spawnChatTmux(context.Background(), spawnDeps{
					Tmux:        &fakeTmuxClient{available: true, hasSession: false},
					Transcripts: &fakeTranscriptOracle{exists: false},
					Argv:        builder,
				}, spawnInput{
					Chat:         newTestChat(t),
					WorktreePath: wt,
					TmuxName:     "boss-agent-session-1",
					Repo:         tc.repo,
					IsUnattended: tc.isUnattended,
					LogPath:      wantLogPath,
				}); err != nil {
					t.Fatalf("spawnChatTmux: %v", err)
				}
				if len(builder.calls) != 1 {
					t.Fatalf("BuildInteractive calls = %d, want 1", len(builder.calls))
				}
				if builder.calls[0].logPath != wantLogPath {
					t.Fatalf("logPath = %q, want %q", builder.calls[0].logPath, wantLogPath)
				}
				got := builder.calls[0].unattendedPolicy
				if !tc.wantPolicy {
					if got != nil {
						t.Fatalf("unattendedPolicy = %+v, want nil", got)
					}
					return
				}
				if got == nil {
					t.Fatal("unattendedPolicy = nil, want non-nil")
				}
				if got.GetWorktreePath() != wt || !got.GetIsNetworkAllowed() || !slices.Equal(got.GetAllowedCommandPrefixes(), tc.wantPrefixes) {
					t.Fatalf("unattendedPolicy = %+v, want worktree=%q network=true prefixes=%v", got, wt, tc.wantPrefixes)
				}
			})
		}
	})

	t.Run("liveArgvBuilder forwards UnattendedPermissionPolicy to RPC", func(t *testing.T) {
		want := &bossanovav1.UnattendedPermissionPolicy{
			WorktreePath:           wt,
			AllowedCommandPrefixes: []string{"git", "gh", "boss", "node", "make", "go"},
			IsNetworkAllowed:       true,
		}
		for _, tc := range []struct {
			name   string
			policy *bossanovav1.UnattendedPermissionPolicy
		}{
			{name: "non-nil", policy: want},
			{name: "nil", policy: nil},
		} {
			t.Run(tc.name, func(t *testing.T) {
				client := &trustRecordingAgentClient{listAgentsFakeClient: &listAgentsFakeClient{}}
				builder := liveArgvBuilder{clients: map[string]agent.RunnerClient{"claude": client}}
				if _, err := builder.BuildInteractive(context.Background(), "claude", "a1", false, wt, "", "", "", "", nil, false, bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_UNSPECIFIED, tc.policy); err != nil {
					t.Fatalf("BuildInteractive: %v", err)
				}
				got := client.last.GetUnattendedPermissionPolicy()
				if tc.policy == nil {
					if got != nil {
						t.Fatalf("UnattendedPermissionPolicy = %+v, want nil", got)
					}
					return
				}
				if got == nil || got.GetWorktreePath() != wt || !got.GetIsNetworkAllowed() || !slices.Equal(got.GetAllowedCommandPrefixes(), tc.policy.GetAllowedCommandPrefixes()) {
					t.Fatalf("UnattendedPermissionPolicy = %+v, want %+v", got, tc.policy)
				}
			})
		}
	})

	t.Run("WakeChatInternal forwards policy iff session is unattended", func(t *testing.T) {
		cronID := "cron-1"
		for _, tc := range []struct {
			name       string
			sess       *models.Session
			wantPolicy bool
		}{
			{
				name:       "cron",
				sess:       &models.Session{ID: "s1", RepoID: "r1", WorktreePath: wt, CronJobID: &cronID},
				wantPolicy: true,
			},
			{
				name:       "tmux-unattended",
				sess:       &models.Session{ID: "s1", RepoID: "r1", WorktreePath: wt, IsTmuxUnattended: true},
				wantPolicy: true,
			},
			{
				name:       "detach",
				sess:       &models.Session{ID: "s1", RepoID: "r1", WorktreePath: wt, Detach: true},
				wantPolicy: true,
			},
			{
				name:       "attended",
				sess:       &models.Session{ID: "s1", RepoID: "r1", WorktreePath: wt},
				wantPolicy: false,
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				chat := &domain.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1", AgentName: "claude"}
				builder := claudeArgvBuilder()
				s := newWakeTestServer(t, chat, tc.sess, &fakeTmuxClient{available: true})
				s.wakeHook.argv = builder
				if _, _, _, err := s.WakeChatInternal(context.Background(), "agent-1", false); err != nil {
					t.Fatalf("WakeChatInternal: %v", err)
				}
				if len(builder.calls) != 1 {
					t.Fatalf("BuildInteractive calls = %d, want 1", len(builder.calls))
				}
				got := builder.calls[0].unattendedPolicy
				if tc.wantPolicy && got == nil {
					t.Fatalf("%s: unattendedPolicy = nil, want non-nil", tc.name)
				}
				if !tc.wantPolicy && got != nil {
					t.Fatalf("%s: unattendedPolicy = %+v, want nil", tc.name, got)
				}
				wantLogPath := bossalog.AgentLogFile(s.agentLogsDir, chat.AgentSessionID)
				if builder.calls[0].logPath != wantLogPath {
					t.Fatalf("%s: logPath = %q, want %q", tc.name, builder.calls[0].logPath, wantLogPath)
				}
			})
		}
	})

	t.Run("ensureChatTmuxSession forwards agent logPath alongside unattended policy", func(t *testing.T) {
		logsDir := filepath.Join(t.TempDir(), "agent-logs")
		sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: wt, AgentName: "claude", IsTmuxUnattended: true}
		chat := &domain.AgentChat{ID: "c1", AgentSessionID: "agent-record-1", SessionID: "s1", AgentName: "claude"}
		chats := &chatStoreFake{chat: chat}
		tmuxer := &fakeTmuxClient{available: true}
		srv := newEnsureRollbackTestServer(t, chats, sess, tmuxer, nil)
		srv.agentLogsDir = logsDir
		builder := claudeArgvBuilder()
		srv.wakeHook.argv = builder
		if err := srv.ensureChatTmuxSession(context.Background(), chat, true); err != nil {
			t.Fatalf("ensureChatTmuxSession: %v", err)
		}
		if len(builder.calls) != 1 {
			t.Fatalf("BuildInteractive calls = %d, want 1", len(builder.calls))
		}
		if builder.calls[0].unattendedPolicy == nil {
			t.Fatal("unattendedPolicy = nil, want non-nil")
		}
		wantLogPath := bossalog.AgentLogFile(logsDir, chat.AgentSessionID)
		if builder.calls[0].logPath != wantLogPath {
			t.Fatalf("logPath = %q, want %q", builder.calls[0].logPath, wantLogPath)
		}
	})

	t.Run("spawnChatTmux removes claude policy file when tmux creation fails", func(t *testing.T) {
		policyDir := t.TempDir()
		policyPath := filepath.Join(policyDir, "claude-policy-respawn-9999.json")
		if err := os.WriteFile(policyPath, []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		builder := &fakeArgvBuilder{
			fresh: map[string][]string{"claude": {"claude", "--settings", policyPath, "--permission-mode", "acceptEdits"}},
		}
		_, err := spawnChatTmux(context.Background(), spawnDeps{
			Tmux:        &fakeTmuxClient{available: true, hasSession: false, createErr: os.ErrPermission},
			Transcripts: &fakeTranscriptOracle{exists: false},
			Argv:        builder,
		}, spawnInput{
			Chat:         newTestChat(t),
			WorktreePath: wt,
			TmuxName:     "boss-agent-session-fail",
			IsUnattended: true,
		})
		if err == nil {
			t.Fatal("spawnChatTmux succeeded with failing tmux NewSessionWithCmd")
		}
		if _, statErr := os.Stat(policyPath); !os.IsNotExist(statErr) {
			t.Fatalf("policy settings file %q survived failed spawnChatTmux: stat err=%v", policyPath, statErr)
		}
	})
}
