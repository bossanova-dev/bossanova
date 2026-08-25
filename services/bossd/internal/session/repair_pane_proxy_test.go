package session

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/rotation"
)

// repairPaneCfg drives newRepairLifecycle. Every field is a gate
// RepairProxyPane must fail closed on, so the table below can flip exactly one
// at a time and assert "no dispatch".
type repairPaneCfg struct {
	enabled     bool
	proxyPort   int
	hasSession  bool
	bakedURL    string
	chatAgent   string
	noTmuxName  bool
	noDispatch  bool // leave paneRepair nil
	noChatStore bool
}

// recordingRepair captures the agent session ids dispatched for repair.
type recordingRepair struct {
	mu  sync.Mutex
	got []string
}

func (r *recordingRepair) dispatch(agentSessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, agentSessionID)
}

func (r *recordingRepair) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.got...)
}

func newRepairLifecycle(t *testing.T, cfg repairPaneCfg) (*Lifecycle, *recordingRepair) {
	t.Helper()
	if cfg.chatAgent == "" {
		cfg.chatAgent = "claude"
	}
	if cfg.proxyPort == 0 {
		cfg.proxyPort = 44127
	}
	if cfg.bakedURL == "" {
		cfg.bakedURL = canonicalBakedURL(cfg.proxyPort)
	}

	sessions := newMockSessionStore()
	sessions.sessions["sess-1"] = &models.Session{ID: "sess-1", RepoID: "repo-1", AgentName: "claude"}

	chat := &models.AgentChat{
		ID:             "chat-1",
		SessionID:      "sess-1",
		AgentSessionID: "agent-01",
		AgentName:      cfg.chatAgent,
	}
	if !cfg.noTmuxName {
		name := "boss-repo-1-agent-01"
		chat.TmuxSessionName = &name
	}
	chats := &mockAgentChatStore{chatsWithTmux: []*models.AgentChat{chat}}

	lc := &Lifecycle{
		sessions:  sessions,
		tmux:      stubTmuxForSweep(cfg.hasSession, cfg.bakedURL),
		logger:    zerolog.Nop(),
		proxyPort: cfg.proxyPort,
	}
	if !cfg.noChatStore {
		lc.agentChats = chats
	}
	on := cfg.enabled
	lc.SetRotationConfig(config.ManagedAccountsConfig{FailoverProxy: &on})
	lc.SetRotationRecorder(rotation.NewRecorder(&captureAuditStore{}, zerolog.Nop()))

	rec := &recordingRepair{}
	if !cfg.noDispatch {
		lc.SetPaneRepairDispatcher(rec.dispatch)
	}
	return lc, rec
}

// TestRepairProxyPaneAttribution pins the gates that decide whether a token the
// proxy could not resolve may be attributed to a pane. Every negative row here
// is a case the adoption sweep also skips: attribution keyed on an
// attacker-supplied token must never be more permissive than adoption.
func TestRepairProxyPaneAttribution(t *testing.T) {
	otherTok := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cases := []struct {
		name  string
		cfg   repairPaneCfg
		token string
		want  bool
	}{
		{
			name:  "live pane whose baked token matches -> dispatch",
			cfg:   repairPaneCfg{enabled: true, hasSession: true},
			token: paneTok,
			want:  true,
		},
		{
			name:  "tmux name absent -> ChatSessionName fallback still dispatches",
			cfg:   repairPaneCfg{enabled: true, hasSession: true, noTmuxName: true},
			token: paneTok,
			want:  true,
		},
		{
			name:  "pane not live -> never attributed",
			cfg:   repairPaneCfg{enabled: true, hasSession: false},
			token: paneTok,
			want:  false,
		},
		{
			name:  "port-mismatched baked URL -> never attributed",
			cfg:   repairPaneCfg{enabled: true, hasSession: true, bakedURL: canonicalBakedURL(45999)},
			token: paneTok,
			want:  false,
		},
		{
			name:  "non-canonical baked URL (https) -> never attributed",
			cfg:   repairPaneCfg{enabled: true, hasSession: true, bakedURL: fmt.Sprintf("https://127.0.0.1:%d/s/%s", 44127, paneTok)},
			token: paneTok,
			want:  false,
		},
		{
			name:  "non-loopback baked URL -> never attributed",
			cfg:   repairPaneCfg{enabled: true, hasSession: true, bakedURL: fmt.Sprintf("http://evil.example:%d/s/%s", 44127, paneTok)},
			token: paneTok,
			want:  false,
		},
		{
			name:  "no baked URL at all -> never attributed",
			cfg:   repairPaneCfg{enabled: true, hasSession: true, bakedURL: "-"},
			token: paneTok,
			want:  false,
		},
		{
			name:  "presented token is not the pane's token -> never attributed",
			cfg:   repairPaneCfg{enabled: true, hasSession: true},
			token: otherTok,
			want:  false,
		},
		{
			name:  "failover proxy disabled -> never attributed",
			cfg:   repairPaneCfg{enabled: false, hasSession: true},
			token: paneTok,
			want:  false,
		},
		{
			name:  "proxy port unbound -> never attributed",
			cfg:   repairPaneCfg{enabled: true, hasSession: true, proxyPort: -1},
			token: paneTok,
			want:  false,
		},
		{
			name:  "non-claude chat -> never attributed",
			cfg:   repairPaneCfg{enabled: true, hasSession: true, chatAgent: "codex"},
			token: paneTok,
			want:  false,
		},
		{
			name:  "no dispatcher wired -> inert",
			cfg:   repairPaneCfg{enabled: true, hasSession: true, noDispatch: true},
			token: paneTok,
			want:  false,
		},
		{
			name:  "no chat store -> inert",
			cfg:   repairPaneCfg{enabled: true, hasSession: true, noChatStore: true},
			token: paneTok,
			want:  false,
		},
		{
			name:  "empty token -> never attributed",
			cfg:   repairPaneCfg{enabled: true, hasSession: true},
			token: "",
			want:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			if cfg.proxyPort == -1 {
				// Sentinel for "port never bound": newRepairLifecycle's default
				// would otherwise substitute the live port.
				cfg.proxyPort = 0
				cfg.bakedURL = canonicalBakedURL(44127)
			}
			if cfg.bakedURL == "-" {
				cfg.bakedURL = ""
				cfg.proxyPort = 44127
				lc, rec := newRepairLifecycleNoBake(t, cfg)
				assertRepair(t, lc, rec, tc.token, tc.want)
				return
			}
			lc, rec := newRepairLifecycle(t, cfg)
			if tc.cfg.proxyPort == -1 {
				lc.proxyPort = 0
			}
			assertRepair(t, lc, rec, tc.token, tc.want)
		})
	}
}

// newRepairLifecycleNoBake builds the "pane exposes no ANTHROPIC_BASE_URL"
// variant, which stubTmuxForSweep expresses as an empty baked URL.
func newRepairLifecycleNoBake(t *testing.T, cfg repairPaneCfg) (*Lifecycle, *recordingRepair) {
	t.Helper()
	lc, rec := newRepairLifecycle(t, cfg)
	lc.tmux = stubTmuxForSweep(cfg.hasSession, "")
	return lc, rec
}

func assertRepair(t *testing.T, lc *Lifecycle, rec *recordingRepair, token string, want bool) {
	t.Helper()
	got, err := lc.RepairProxyPane(context.Background(), "sess-1", "agent-01", token)
	if err != nil {
		t.Fatalf("RepairProxyPane returned error: %v", err)
	}
	if got != want {
		t.Fatalf("RepairProxyPane repaired = %v, want %v", got, want)
	}
	calls := rec.calls()
	if want && (len(calls) != 1 || calls[0] != "agent-01") {
		t.Fatalf("dispatches = %v, want exactly [agent-01]", calls)
	}
	if !want && len(calls) != 0 {
		t.Fatalf("dispatches = %v, want none", calls)
	}
}

// TestRepairProxyPaneUnknownChat pins the unattributable case: a token whose
// durable row names a chat this daemon no longer has. It must be a quiet
// no-dispatch, not an error the proxy has to reason about.
func TestRepairProxyPaneUnknownChat(t *testing.T) {
	lc, rec := newRepairLifecycle(t, repairPaneCfg{enabled: true, hasSession: true})
	got, err := lc.RepairProxyPane(context.Background(), "sess-1", "agent-gone", paneTok)
	if err != nil {
		t.Fatalf("RepairProxyPane returned error: %v", err)
	}
	if got {
		t.Fatal("RepairProxyPane repaired an unknown chat")
	}
	if calls := rec.calls(); len(calls) != 0 {
		t.Fatalf("dispatches = %v, want none", calls)
	}
}

// TestRepairProxyPaneNilLifecycle guards the dual-nil safety the proxy relies on
// when no Lifecycle was ever constructed.
func TestRepairProxyPaneNilLifecycle(t *testing.T) {
	var lc *Lifecycle
	got, err := lc.RepairProxyPane(context.Background(), "sess-1", "agent-01", paneTok)
	if err != nil || got {
		t.Fatalf("nil Lifecycle RepairProxyPane = (%v, %v), want (false, nil)", got, err)
	}
}
