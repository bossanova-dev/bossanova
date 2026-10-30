package server

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/recurser/bossalib/kit/config"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/account"
	"github.com/recurser/bossd/internal/accountwiring"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/session"
	"github.com/rs/zerolog"
)

type daemonCompanionRegistrar struct{ account string }

func (r *daemonCompanionRegistrar) TokenForCompanion(_, initial string) string {
	r.account = initial
	return "child-path"
}
func (r *daemonCompanionRegistrar) CompanionAgentAccount(string) (string, bool) {
	return r.account, r.account != ""
}
func (r *daemonCompanionRegistrar) RebindCompanion(_, account string) { r.account = account }

type daemonCompanionMaterializer struct{}

func (daemonCompanionMaterializer) Materialize(context.Context, *domain.AgentAccount) (map[string]string, error) {
	return map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "child-bearer"}, nil
}

func TestResolveChatAccountEnvForSpawn_CompanionClaude_Accounts_AC30(t *testing.T) {
	s, accts := newAccountServer(t, newFakeCredStore(), nil)
	codexAcct := mustAddCodex(t, s, "codex-child", []byte("blob"))
	mat := &fakeMaterializer{supports: true, envByAccount: map[string]map[string]string{codexAcct.Id: {"CODEX_HOME": "/codex"}}}
	s.resolver = account.NewResolver(accountwiring.NewRegistry(accts), mat, zerolog.Nop())
	lc := &session.Lifecycle{}
	lc.SetRotationConfig(config.ManagedAgentAccountsConfig{})
	lc.SetProxyPort(5555)
	lc.SetCompanionProxyRegistrar(&daemonCompanionRegistrar{})
	lc.SetDefaultAgentAccountResolver(func(context.Context, string, string, time.Time) (string, error) { return "claude-child", nil })
	lc.SetAgentAccountGetter(func(context.Context, string) (*domain.AgentAccount, error) {
		return &domain.AgentAccount{ID: "claude-child", Provider: domain.AgentAccountProviderClaude}, nil
	})
	lc.SetAgentAccountMaterializer(daemonCompanionMaterializer{})
	s.lifecycle = lc
	sess := &models.Session{ID: "parent", AgentName: "claude"}
	chat := &domain.AgentChat{SessionID: sess.ID, AgentSessionID: "codex-chat", AgentName: "codex", AccountID: &codexAcct.Id}
	env, err := s.resolveChatAccountEnvForSpawn(context.Background(), sess, chat, "", recordAccountUse)
	if err != nil {
		t.Fatal(err)
	}
	if env["CODEX_HOME"] != "/codex" || env["ANTHROPIC_API_KEY"] != domain.SentinelAPIKey || env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:5555/s/child-path" {
		t.Fatal("missing companion overlay")
	}
	if _, ok := env["CLAUDE_CODE_OAUTH_TOKEN"]; ok {
		t.Fatal("raw Claude bearer leaked")
	}
}

func TestSpawnChatTmux_CompanionSentinelFlag_Accounts_AC30(t *testing.T) {
	for _, key := range []string{"", "real-private-key", domain.SentinelAPIKey} {
		builder := claudeArgvBuilder()
		_, err := spawnChatTmux(context.Background(), spawnDeps{Tmux: &fakeTmuxClient{available: true}, Transcripts: &fakeTranscriptOracle{}, Argv: builder}, spawnInput{Chat: newTestChat(t), WorktreePath: t.TempDir(), TmuxName: "boss-companion", SessionEnvFunc: func() (map[string]string, error) { return map[string]string{"ANTHROPIC_API_KEY": key}, nil }})
		if err != nil {
			t.Fatal(err)
		}
		if len(builder.calls) != 1 || builder.calls[0].isClaudeProxyEnabled != (key == domain.SentinelAPIKey) {
			t.Fatal("incorrect companion sentinel flag")
		}
	}
}

func TestSpawnChatTmux_ForwardsConfigHomeEnvBeforeArgvBuild(t *testing.T) {
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	builder := claudeArgvBuilder()
	_, err := spawnChatTmux(context.Background(), spawnDeps{
		Tmux:        tmuxer,
		Transcripts: &fakeTranscriptOracle{exists: false},
		Argv:        builder,
	}, spawnInput{
		Chat:         newTestChat(t),
		WorktreePath: t.TempDir(),
		TmuxName:     "boss-agent-session-1",
		SessionEnvFunc: func() (map[string]string, error) {
			return map[string]string{"CODEX_HOME": "/selected/codex", "HOME": "/selected/home", "API_KEY": "secret"}, nil
		},
	})
	if err != nil {
		t.Fatalf("spawnChatTmux: %v", err)
	}
	if len(builder.calls) != 1 {
		t.Fatalf("BuildInteractive calls = %d, want 1", len(builder.calls))
	}
	if got := builder.calls[0].configHomeEnv; !reflect.DeepEqual(got, map[string]string{"CODEX_HOME": "/selected/codex", "HOME": "/selected/home"}) {
		t.Fatalf("ConfigHomeEnv = %#v, want selected homes only", got)
	}
}
