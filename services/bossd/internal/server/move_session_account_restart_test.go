package server

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/tmux"
)

const restartProxyPort = 45673

// paneEnvTmux answers has-session for every pane and show-environment from
// baked, keyed by pane name: the frozen ANTHROPIC_BASE_URL each pane was
// started with.
func paneEnvTmux(baked map[string]string) *tmux.Client {
	return tmux.NewClient(tmux.WithCommandFactory(func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		if len(args) == 4 && args[0] == "show-environment" {
			if url, ok := baked[args[2]]; ok {
				return exec.CommandContext(ctx, "printf", "%s\n", args[3]+"="+url)
			}
			return exec.CommandContext(ctx, "sh", "-c", "echo 'unknown variable' >&2; exit 1")
		}
		return exec.CommandContext(ctx, "true")
	}))
}

type sessionAccountMove func(ctx context.Context, lc *session.Lifecycle, sessions db.SessionStore, sessionID string) error

// A pane started from the session's account before a daemon restart still
// bills through the session afterwards. When the session's account moves,
// by a switch or by an automatic proxy failover, that pane's row must name the
// account it now bills. A pane holding its own chat token keeps its account.
func TestMoveSessionAccount_PaneThatSurvivedARestartFollowsTheSession(t *testing.T) {
	moves := []struct {
		name string
		move sessionAccountMove
	}{
		{"switch", func(ctx context.Context, lc *session.Lifecycle, sessions db.SessionStore, sessionID string) error {
			return lc.MoveSessionAgentAccount(ctx, mustGetSession(ctx, t, sessions, sessionID), "acct-2")
		}},
		{"automatic failover", func(ctx context.Context, lc *session.Lifecycle, _ db.SessionStore, sessionID string) error {
			return lc.CommitFailover(ctx, sessionID, session.FailoverResult{
				Rotate: true, FromAccountID: "acct-1", NextAccountID: "acct-2",
				Provider: "claude", Trigger: "ROTATION_TRIGGER_USAGE_LIMIT",
			})
		}},
	}
	for _, tc := range moves {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			sqlDB := rebuildTestDB(t)
			tokens := db.NewProxyTokenStore(sqlDB)
			sessions := db.NewSessionStore(sqlDB)
			chats := db.NewAgentChatStore(sqlDB)
			sessionID := rebuildTestSession(t, sqlDB)
			start := "acct-1"
			startPtr := &start
			sess, err := sessions.Update(ctx, sessionID, db.UpdateSessionParams{AccountID: &startPtr})
			if err != nil {
				t.Fatalf("bind session: %v", err)
			}
			for _, agentSessionID := range []string{"agent-build", "agent-own"} {
				acct := "acct-1"
				if _, err := chats.Create(ctx, db.CreateAgentChatParams{
					SessionID: sessionID, AgentSessionID: agentSessionID, AgentName: "claude", AccountID: &acct,
				}); err != nil {
					t.Fatalf("create chat %s: %v", agentSessionID, err)
				}
			}

			daemonA := newRebuildProxy(t, tokens, &bytes.Buffer{})
			proxyURL := func(token string) string {
				return fmt.Sprintf("http://127.0.0.1:%d/s/%s", restartProxyPort, token)
			}
			sessionToken := daemonA.TokenForSession(sessionID)
			chatToken := daemonA.TokenForChat(sessionID, "agent-own", "acct-1")
			baked := map[string]string{
				tmux.ChatSessionName(sess.RepoID, "agent-build"): proxyURL(sessionToken),
				tmux.ChatSessionName(sess.RepoID, "agent-own"):   proxyURL(chatToken),
			}

			daemonB := newRebuildProxy(t, tokens, &bytes.Buffer{})
			if err := daemonB.RebuildTokenRegistry(ctx); err != nil {
				t.Fatalf("RebuildTokenRegistry: %v", err)
			}
			// What the adoption sweep does for rows that carry an account.
			daemonB.AdoptTokenForChat(sessionID, "agent-build", "acct-1", sessionToken)
			daemonB.AdoptTokenForChat(sessionID, "agent-own", "acct-1", chatToken)
			if target, ok := daemonB.TokenTarget(sessionToken); !ok || target != sessionID {
				t.Fatalf("after the restart the build pane's token resolves to %q, want the session", target)
			}
			tokensBefore, err := tokens.List(ctx)
			if err != nil {
				t.Fatalf("list proxy tokens: %v", err)
			}

			lc := session.NewLifecycle(sessions, nil, chats, nil, nil, nil, paneEnvTmux(baked), nil, zerolog.Nop())
			lc.SetProxyPort(restartProxyPort)
			lc.SetProxyRegistrar(daemonB)
			lc.SetChatRunningProbe(func(string) bool { return true })

			if err := tc.move(ctx, lc, sessions, sessionID); err != nil {
				t.Fatalf("move: %v", err)
			}

			if got := mustGetSession(ctx, t, sessions, sessionID).AccountID; got == nil || *got != "acct-2" {
				t.Fatalf("session account = %v, want acct-2", got)
			}
			// A failover rewrites the rows after its commit returns.
			if got := awaitChatAccountID(ctx, t, chats, "agent-build", "acct-2"); got != "acct-2" {
				t.Errorf("build pane row = %q, want acct-2, the account its pane now bills through the session", got)
			}
			time.Sleep(200 * time.Millisecond)
			if got := chatAccountID(ctx, t, chats, "agent-own"); got != "acct-1" {
				t.Errorf("chat-token pane row = %q, want acct-1 kept", got)
			}
			tokensAfter, err := tokens.List(ctx)
			if err != nil {
				t.Fatalf("list proxy tokens: %v", err)
			}
			if len(tokensAfter) != len(tokensBefore) {
				t.Errorf("proxy tokens = %d after the move, want %d: the move minted a token no pane holds", len(tokensAfter), len(tokensBefore))
			}
		})
	}
}

// awaitChatAccountID polls the chat's account for up to five seconds until it
// reads want, and returns the last value read.
func awaitChatAccountID(ctx context.Context, t *testing.T, chats db.AgentChatStore, agentSessionID, want string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := chatAccountID(ctx, t, chats, agentSessionID)
		if got == want || time.Now().After(deadline) {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func mustGetSession(ctx context.Context, t *testing.T, sessions db.SessionStore, sessionID string) *models.Session {
	t.Helper()
	sess, err := sessions.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	return sess
}

func chatAccountID(ctx context.Context, t *testing.T, chats db.AgentChatStore, agentSessionID string) string {
	t.Helper()
	chat, err := chats.GetByAgentSessionID(ctx, agentSessionID)
	if err != nil {
		t.Fatalf("get chat %s: %v", agentSessionID, err)
	}
	if chat.AccountID == nil {
		return "<nil>"
	}
	return *chat.AccountID
}
