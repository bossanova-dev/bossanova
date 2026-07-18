package session

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/rotation"
	"github.com/recurser/bossd/internal/tmux"
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

// stubTmuxForSweep returns a tmux.Client whose has-session / show-environment
// calls are faked from the given knobs (no real tmux). true/false/sh are always
// present on the unix test hosts.
func stubTmuxForSweep(hasSession bool, bakedURL string) *tmux.Client {
	return tmux.NewClient(tmux.WithCommandFactory(func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		sub := ""
		if len(args) > 0 {
			sub = args[0]
		}
		switch sub {
		case "has-session":
			if hasSession {
				return exec.CommandContext(ctx, "true")
			}
			return exec.CommandContext(ctx, "sh", "-c", "printf '%s' \"can't find session\" >&2; exit 1")
		case "show-environment":
			if bakedURL == "" {
				return exec.CommandContext(ctx, "sh", "-c", "printf '%s' 'unknown variable' >&2; exit 1")
			}
			return exec.CommandContext(ctx, "sh", "-c", "printf '%s\\n' 'ANTHROPIC_BASE_URL="+bakedURL+"'")
		default:
			return exec.CommandContext(ctx, "true")
		}
	}))
}

func newStaleSweepLifecycle(t *testing.T, proxyPort int, enabled bool, tmuxClient *tmux.Client, store *captureAuditStore) *Lifecycle {
	t.Helper()
	sessions := newMockSessionStore()
	sessions.sessions["sess-1"] = &models.Session{ID: "sess-1", RepoID: "repo-1", AgentName: "claude"}

	name := "boss-repo-1-agent-01"
	chats := &mockAgentChatStore{
		chatsWithTmux: []*models.AgentChat{{
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
	lc.SetRotationConfig(config.ManagedAccountsConfig{FailoverProxy: &on})
	lc.SetRotationRecorder(rotation.NewRecorder(store, zerolog.Nop()))
	return lc
}

func TestDetectStalePaneProxyPorts(t *testing.T) {
	cases := []struct {
		name       string
		proxyPort  int
		enabled    bool
		hasSession bool
		bakedURL   string
		wantAudits int
	}{
		{"stale pane surfaced once", 44127, true, true, "http://127.0.0.1:56249/s/tok", 1},
		{"matching port inert", 44127, true, true, "http://127.0.0.1:44127/s/tok", 0},
		{"no baked url inert", 44127, true, true, "", 0},
		{"dead pane inert", 44127, true, false, "http://127.0.0.1:56249/s/tok", 0},
		{"proxy disabled inert", 44127, false, true, "http://127.0.0.1:56249/s/tok", 0},
		{"proxy unbound inert", 0, true, true, "http://127.0.0.1:56249/s/tok", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &captureAuditStore{}
			lc := newStaleSweepLifecycle(t, tc.proxyPort, tc.enabled,
				stubTmuxForSweep(tc.hasSession, tc.bakedURL), store)

			lc.detectStalePaneProxyPorts(context.Background())

			got := store.all()
			if len(got) != tc.wantAudits {
				t.Fatalf("audit events = %d, want %d", len(got), tc.wantAudits)
			}
			if tc.wantAudits == 1 {
				ev := got[0]
				if ev.ChatID != "chat-1" || ev.SessionID != "sess-1" {
					t.Fatalf("audit ids = %q/%q, want chat-1/sess-1", ev.ChatID, ev.SessionID)
				}
				if !strings.Contains(ev.Detail, "stale") || !strings.Contains(ev.Detail, "56249") {
					t.Fatalf("audit Detail = %q, want a stale-port marker mentioning the baked port", ev.Detail)
				}
			}
		})
	}
}
