package session

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/recurser/bossd/internal/domain"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/kit/config"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/adapter/tmux"
	"github.com/recurser/bossd/internal/rotation"
)

// --- pure decision helper ---

func TestStalePaneProxyPort(t *testing.T) {
	cases := []struct {
		name      string
		bakedURL  string
		livePort  int
		wantPort  int
		wantStale bool
	}{
		{"baked equals live", "http://127.0.0.1:44127/s/tok", 44127, 44127, false},
		{"baked differs from live", "http://127.0.0.1:56249/s/tok", 44127, 56249, true},
		{"no baked url", "", 44127, 0, false},
		{"live port unbound", "http://127.0.0.1:56249/s/tok", 0, 0, false},
		{"baked url has no port", "http://127.0.0.1/s/tok", 44127, 0, false},
		{"garbage url", "://:::", 44127, 0, false},
		{"non-numeric port", "http://127.0.0.1:abc/s/tok", 44127, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotPort, gotStale := stalePaneProxyPort(tc.bakedURL, tc.livePort)
			if gotPort != tc.wantPort || gotStale != tc.wantStale {
				t.Fatalf("stalePaneProxyPort(%q, %d) = %d,%v want %d,%v",
					tc.bakedURL, tc.livePort, gotPort, gotStale, tc.wantPort, tc.wantStale)
			}
		})
	}
}

// sweepTmuxStub fakes the tmux calls the stale-port sweep makes (no real tmux)
// and records every kill-session target. true/false/sh are always present on
// the unix test hosts.
type sweepTmuxStub struct {
	hasSession bool
	bakedURL   string
	// envErr makes show-environment fail with an unclassified error
	// (tmux.ShowEnvError) rather than answering.
	envErr bool

	mu    sync.Mutex
	kills []string
}

func (s *sweepTmuxStub) killed() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.kills...)
}

func (s *sweepTmuxStub) client() *tmux.Client {
	return tmux.NewClient(tmux.WithPasteSettleWait(testPasteSettleWait), tmux.WithCommandFactory(func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		sub := ""
		if len(args) > 0 {
			sub = args[0]
		}
		switch sub {
		case "has-session":
			if s.hasSession {
				return exec.CommandContext(ctx, "true")
			}
			return exec.CommandContext(ctx, "sh", "-c", "printf '%s' \"can't find session\" >&2; exit 1")
		case "show-environment":
			if s.envErr {
				return exec.CommandContext(ctx, "sh", "-c", "printf '%s' 'lost server' >&2; exit 1")
			}
			if s.bakedURL == "" {
				return exec.CommandContext(ctx, "sh", "-c", "printf '%s' 'unknown variable' >&2; exit 1")
			}
			return exec.CommandContext(ctx, "sh", "-c", "printf '%s\\n' 'ANTHROPIC_BASE_URL="+s.bakedURL+"'")
		case "kill-session":
			s.mu.Lock()
			s.kills = append(s.kills, args[len(args)-1])
			s.mu.Unlock()
			return exec.CommandContext(ctx, "true")
		default:
			return exec.CommandContext(ctx, "true")
		}
	}))
}

// stubTmuxForSweep returns a tmux.Client whose has-session / show-environment
// calls are faked from the given knobs, for callers that do not need to observe
// kills.
func stubTmuxForSweep(hasSession bool, bakedURL string) *tmux.Client {
	return (&sweepTmuxStub{hasSession: hasSession, bakedURL: bakedURL}).client()
}

const sweepPaneName = "boss-repo-1-agent-01"

func newStaleSweepLifecycle(t *testing.T, proxyPort int, enabled bool, tmuxClient *tmux.Client, store *captureAuditStore) (*Lifecycle, *mockAgentChatStore) {
	t.Helper()
	sessions := newMockSessionStore()
	sessions.sessions["sess-1"] = &models.Session{ID: "sess-1", RepoID: "repo-1", AgentName: "claude"}

	name := sweepPaneName
	chats := &mockAgentChatStore{
		chatsWithTmux: []*domain.AgentChat{{
			ID:              "chat-1",
			SessionID:       "sess-1",
			AgentSessionID:  "agent-01",
			AgentName:       "claude",
			TmuxSessionName: &name,
		}},
	}

	lc := &Lifecycle{
		sessions:   sessions,
		agentChats: chats,
		tmux:       tmuxClient,
		logger:     zerolog.Nop(),
		proxyPort:  proxyPort,
	}
	on := enabled
	lc.SetRotationConfig(config.ManagedAgentAccountsConfig{FailoverProxy: &on})
	lc.SetRotationRecorder(rotation.NewRecorder(store, zerolog.Nop()))
	return lc, chats
}

// TestDetectStalePaneProxyPorts covers the BOS-1563 kill policy: a live Claude
// pane baked to a proxy port other than the live one is killed (it can never
// reach this daemon again, and while it lives it sends credentials and prompts
// to whoever holds that port), with an audit marker saying why, and its chat
// row survives so the wake path restarts it on the live port. Every other
// shape — matching port, no or unreadable baked URL, dead pane, proxy off or
// unbound — is left untouched.
func TestDetectStalePaneProxyPorts(t *testing.T) {
	cases := []struct {
		name       string
		proxyPort  int
		enabled    bool
		hasSession bool
		bakedURL   string
		envErr     bool
		wantKilled bool
	}{
		{"stale pane killed", 44127, true, true, "http://127.0.0.1:56249/s/tok", false, true},
		{"matching port untouched", 44127, true, true, "http://127.0.0.1:44127/s/tok", false, false},
		{"no baked url untouched", 44127, true, true, "", false, false},
		{"unreadable env untouched", 44127, true, true, "", true, false},
		{"dead pane untouched", 44127, true, false, "http://127.0.0.1:56249/s/tok", false, false},
		{"proxy disabled untouched", 44127, false, true, "http://127.0.0.1:56249/s/tok", false, false},
		{"proxy unbound untouched", 0, true, true, "http://127.0.0.1:56249/s/tok", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &captureAuditStore{}
			stub := &sweepTmuxStub{hasSession: tc.hasSession, bakedURL: tc.bakedURL, envErr: tc.envErr}
			lc, chats := newStaleSweepLifecycle(t, tc.proxyPort, tc.enabled, stub.client(), store)

			lc.detectStalePaneProxyPorts(context.Background())

			kills := stub.killed()
			got := store.all()
			if !tc.wantKilled {
				if len(kills) != 0 {
					t.Fatalf("kill-session targets = %v, want none", kills)
				}
				if len(got) != 0 {
					t.Fatalf("audit events = %d, want 0", len(got))
				}
				return
			}
			if len(kills) != 1 || kills[0] != sweepPaneName {
				t.Fatalf("kill-session targets = %v, want [%s]", kills, sweepPaneName)
			}
			if len(got) != 1 {
				t.Fatalf("audit events = %d, want 1", len(got))
			}
			ev := got[0]
			if ev.ChatID != "chat-1" || ev.SessionID != "sess-1" {
				t.Fatalf("audit ids = %q/%q, want chat-1/sess-1", ev.ChatID, ev.SessionID)
			}
			if !strings.Contains(ev.Detail, "killed") || !strings.Contains(ev.Detail, "56249") || !strings.Contains(ev.Detail, "44127") {
				t.Fatalf("audit Detail = %q, want a kill marker naming the baked and live ports", ev.Detail)
			}
			// The chat row survives — only its pane pointer is cleared, which
			// is the idle-reaped shape the wake path restarts on demand.
			if len(chats.deletedAgentSessionIDs) != 0 {
				t.Fatalf("chat rows deleted = %v, want the row kept for resume", chats.deletedAgentSessionIDs)
			}
			chat, err := chats.GetByAgentSessionID(context.Background(), "agent-01")
			if err != nil || chat == nil {
				t.Fatalf("chat row lookup after kill: %v, %v; want the row kept", chat, err)
			}
			if chat.TmuxSessionName != nil {
				t.Fatalf("chat tmux pointer = %q after kill, want cleared (a pointed row over a dead pane finalizes the session)", *chat.TmuxSessionName)
			}
		})
	}
}
