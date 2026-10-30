package session

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/recurser/bossalib/kit/config"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/rotation"
)

type companionRegistrarFixture struct {
	account string
	tokens  int
}

func (r *companionRegistrarFixture) TokenForCompanion(_, initial string) string {
	r.tokens++
	if r.account == "" {
		r.account = initial
	}
	return "companion-path"
}
func (r *companionRegistrarFixture) CompanionAgentAccount(string) (string, bool) {
	return r.account, r.account != ""
}
func (r *companionRegistrarFixture) RebindCompanion(_, account string) { r.account = account }

func TestCompanionProxyEnv_Accounts_AC30(t *testing.T) {
	for _, mode := range []string{"enabled", "claude", "legacy claude", "disabled", "unmanaged", "unbound", "no registrar", "no account", "resolver error", "materialize error", "no bearer", "rotated", "disabled account"} {
		t.Run(mode, func(t *testing.T) {
			f := newRotationFixture(t)
			enableFailoverProxy(f.lc)
			r := &companionRegistrarFixture{}
			f.lc.SetCompanionProxyRegistrar(r)
			f.lc.SetProxyPort(5555)
			f.lc.defaultAccountResolver = func(context.Context, string, string, time.Time) (string, error) {
				if mode == "resolver error" {
					return "", errors.New("secret resolver error")
				}
				if mode == "no account" {
					return "", nil
				}
				return "claude-default", nil
			}
			f.lc.SetAgentAccountGetter(func(_ context.Context, id string) (*domain.AgentAccount, error) {
				a := &domain.AgentAccount{ID: id, Provider: domain.AgentAccountProviderClaude}
				if mode == "disabled account" && id == "disabled" {
					a.Status = domain.AgentAccountStatusDisabled
				}
				return a, nil
			})
			if mode == "rotated" {
				r.account = "claude-rotated"
			}
			if mode == "disabled account" {
				r.account = "disabled"
			}
			switch mode {
			case "disabled":
				f.lc.SetRotationConfig(config.ManagedAgentAccountsConfig{FailoverProxy: boolPtr(false)})
			case "unmanaged":
				f.lc.SetRotationConfig(config.ManagedAgentAccountsConfig{Enabled: boolPtr(false)})
			case "unbound":
				f.lc.SetProxyPort(0)
			case "no registrar":
				f.lc.SetCompanionProxyRegistrar(nil)
			case "materialize error":
				f.materializer.err = errors.New("secret materialize error")
			case "no bearer":
				f.materializer.env = nil
			}
			agent := "codex"
			if mode == "claude" {
				agent = "claude"
			}
			if mode == "legacy claude" {
				agent = ""
			}
			base := map[string]string{"CODEX_HOME": "/codex", "HOME": "/home"}
			before := map[string]string{"CODEX_HOME": "/codex", "HOME": "/home"}
			got := f.lc.ApplyCompanionProxyEnv(context.Background(), &models.Session{ID: f.sessionID, AgentName: agent}, agent, base)
			enabled := mode == "enabled" || mode == "rotated" || mode == "disabled account"
			if enabled {
				if got["ANTHROPIC_API_KEY"] != domain.SentinelAPIKey || got["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:5555/s/companion-path" {
					t.Fatalf("overlay missing")
				}
				if _, ok := got[claudeOAuthTokenEnvKey]; ok {
					t.Fatal("raw bearer injected")
				}
				expected := "claude-default"
				if mode == "rotated" {
					expected = "claude-rotated"
				}
				if r.account != expected {
					t.Fatalf("account=%s", r.account)
				}
			} else if !reflect.DeepEqual(got, before) {
				t.Fatalf("expected unchanged env")
			}
			if !reflect.DeepEqual(base, before) || got["CODEX_HOME"] != "/codex" || got["HOME"] != "/home" {
				t.Fatal("base changed")
			}
		})
	}
}

func TestCompanionFailoverLeavesSessionBinding_Accounts_AC30(t *testing.T) {
	for _, status := range []int{429, 401} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			f := newRotationFixture(t)
			enableFailoverProxy(f.lc)
			r := &companionRegistrarFixture{account: "acct-capped"}
			f.lc.SetCompanionProxyRegistrar(r)
			own := "codex-account"
			sess := f.sessions.sessions[f.sessionID]
			sess.AgentName = "codex"
			sess.AccountID = &own
			f.lc.SetAgentAccountGetter(func(_ context.Context, id string) (*domain.AgentAccount, error) {
				return &domain.AgentAccount{ID: id, Provider: domain.AgentAccountProviderClaude}, nil
			})
			target := domain.ProxyTargetForCompanion(f.sessionID)
			bearer, err := f.lc.CurrentBearer(context.Background(), target)
			if err != nil || bearer != "next-token" || f.materializer.lastAccount.ID != "acct-capped" {
				t.Fatalf("bearer resolution failed: %v", err)
			}
			f.decider.outcome = rotation.Outcome{Kind: rotation.OutcomeRotate, NextAccount: &domain.AgentAccount{ID: "acct-next", Provider: domain.AgentAccountProviderClaude}}
			store := &captureAuditStore{}
			f.lc.SetRotationRecorder(rotation.NewRecorder(store, f.lc.logger))
			result, err := f.lc.PrepareFailover(context.Background(), target, status)
			if err != nil || !result.Rotate {
				t.Fatalf("prepare=%+v err=%v", result, err)
			}
			if f.binding.lastSession.AgentName != "claude" || *f.binding.lastSession.AccountID != "acct-capped" {
				t.Fatal("wrong rotation binding")
			}
			if err := f.lc.CommitFailover(context.Background(), target, result); err != nil {
				t.Fatal(err)
			}
			if r.account != "acct-next" || *sess.AccountID != own {
				t.Fatal("wrong binding after commit")
			}
			events := store.all()
			if len(events) != 1 || events[0].SessionID != sess.ID || events[0].Provider != "claude" {
				t.Fatalf("audit=%+v", events)
			}
		})
	}
}

func TestCompanionLifecycleSpawnPaths_Accounts_AC30(t *testing.T) {
	for _, chatBound := range []bool{false, true} {
		t.Run(strconv.FormatBool(chatBound), func(t *testing.T) {
			f := newRotationFixture(t)
			enableFailoverProxy(f.lc)
			f.lc.SetProxyPort(5555)
			f.lc.SetCompanionProxyRegistrar(&companionRegistrarFixture{account: "claude-child"})
			f.lc.SetAgentAccountGetter(func(context.Context, string) (*domain.AgentAccount, error) {
				return &domain.AgentAccount{ID: "claude-child", Provider: domain.AgentAccountProviderClaude}, nil
			})
			f.lc.SetAgentAccountEnvResolver(&fakeAccountEnvResolver{env: map[string]string{"CODEX_HOME": "/codex"}})
			sess := &models.Session{ID: f.sessionID, AgentName: "codex"}
			var env map[string]string
			var err error
			if chatBound {
				id := "codex-account"
				env, err = f.lc.resolveAccountEnvForChat(context.Background(), sess, &domain.AgentChat{AgentName: "codex", AgentSessionID: "chat", AccountID: &id})
			} else {
				env, err = f.lc.resolveAccountEnv(context.Background(), sess)
			}
			if err != nil || env["CODEX_HOME"] != "/codex" || env["ANTHROPIC_API_KEY"] != domain.SentinelAPIKey {
				t.Fatalf("spawn overlay missing: %v", err)
			}
		})
	}
}

func TestCompanionTmuxSentinelFlag_Accounts_AC30(t *testing.T) {
	if testing.Short() {
		t.Skip("run make test-bossd for tmux coverage")
	}
	for _, key := range []string{"", "real-private-key", domain.SentinelAPIKey} {
		h := newStartTmuxChatHarness(t)
		h.lc.SetAgentAccountEnvResolver(&fakeAccountEnvResolver{env: map[string]string{"ANTHROPIC_API_KEY": key}})
		_, err := h.lc.StartTmuxChat(context.Background(), "sess-1", ChatInput{Prompt: "/boss-repair", Delivery: DeliverySubmit}, "title", HookOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if h.agentFake.LastBuildInteractiveCommand.GetIsClaudeProxyEnabled() != (key == domain.SentinelAPIKey) {
			t.Fatal("incorrect sentinel flag")
		}
	}
}
