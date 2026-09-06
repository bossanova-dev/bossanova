package session

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/rotation"
	"github.com/recurser/bossd/internal/tmux"
)

// paneTok is a canonical 64-lowercase-hex token, as mintProxyToken emits.
const paneTok = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

// canonicalBakedURL builds the canonical ANTHROPIC_BASE_URL a managed pane
// bakes: loopback host, /s/<token> path, always the canonical paneTok.
func canonicalBakedURL(port int) string {
	return fmt.Sprintf("http://127.0.0.1:%d/s/%s", port, paneTok)
}

// --- pure strict-parse tests ---

func TestParseBakedProxyToken(t *testing.T) {
	const live = 44127
	cases := []struct {
		name      string
		bakedURL  string
		livePort  int
		wantToken string
		wantOK    bool
	}{
		{"canonical", canonicalBakedURL(live), live, paneTok, true},
		{"empty url", "", live, "", false},
		{"live port unbound", canonicalBakedURL(live), 0, "", false},
		{"port mismatch", canonicalBakedURL(56249), live, "", false},
		{"https scheme rejected", "https://127.0.0.1:44127/s/" + paneTok, live, "", false},
		{"non-loopback host rejected", "http://10.0.0.1:44127/s/" + paneTok, live, "", false},
		{"localhost name rejected", "http://localhost:44127/s/" + paneTok, live, "", false},
		{"userinfo rejected", "http://u:p@127.0.0.1:44127/s/" + paneTok, live, "", false},
		{"query rejected", canonicalBakedURL(live) + "?x=1", live, "", false},
		{"force-query rejected", canonicalBakedURL(live) + "?", live, "", false},
		{"fragment rejected", canonicalBakedURL(live) + "#f", live, "", false},
		{"extra path segment rejected", canonicalBakedURL(live) + "/v1", live, "", false},
		{"encoded slash rejected", "http://127.0.0.1:44127/s%2f" + paneTok, live, "", false},
		// %61 decodes to 'a' == paneTok[0], so the DECODED path is byte-identical
		// to the canonical token; only the RawPath guard rejects it.
		{"encoded hex char rejected", "http://127.0.0.1:44127/s/%61" + paneTok[1:], live, "", false},
		{"wrong prefix rejected", "http://127.0.0.1:44127/x/" + paneTok, live, "", false},
		{"short token rejected", "http://127.0.0.1:44127/s/abcd", live, "", false},
		{"uppercase hex rejected", "http://127.0.0.1:44127/s/" + "ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789AB"[:64], live, "", false},
		{"non-hex rejected", "http://127.0.0.1:44127/s/" + "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz", live, "", false},
		{"no port rejected", "http://127.0.0.1/s/" + paneTok, live, "", false},
		{"garbage rejected", "://:::", live, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotTok, gotOK := parseBakedProxyToken(tc.bakedURL, tc.livePort)
			if gotTok != tc.wantToken || gotOK != tc.wantOK {
				t.Fatalf("parseBakedProxyToken(%q,%d) = %q,%v want %q,%v",
					tc.bakedURL, tc.livePort, gotTok, gotOK, tc.wantToken, tc.wantOK)
			}
		})
	}
}

// --- recording registrar ---

type adoptCall struct {
	kind           string // "session" | "chat"
	token          string
	sessionID      string
	agentSessionID string
	accountID      string
}

type recordingRegistrar struct {
	mu    sync.Mutex
	calls []adoptCall
	// rebuildErr, when set, makes RebuildTokenRegistry fail so a test can prove
	// Bootstrap logs and continues into the sweep rather than aborting.
	rebuildErr error
}

func (r *recordingRegistrar) TokenForSession(string) string              { return "" }
func (r *recordingRegistrar) TokenForChat(string, string, string) string { return "" }
func (r *recordingRegistrar) ForgetBearer(string)                        {}
func (r *recordingRegistrar) ForgetAllBearers()                          {}
func (r *recordingRegistrar) RebuildTokenRegistry(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, adoptCall{kind: "rebuild"})
	return r.rebuildErr
}
func (r *recordingRegistrar) AdoptToken(token, sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, adoptCall{kind: "session", token: token, sessionID: sessionID})
}
func (r *recordingRegistrar) AdoptTokenForChat(sessionID, agentSessionID, accountID, token string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, adoptCall{
		kind: "chat", token: token, sessionID: sessionID,
		agentSessionID: agentSessionID, accountID: accountID,
	})
}
func (r *recordingRegistrar) RetargetChatToken(sessionID, priorAgentSessionID, newAgentSessionID, accountID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, adoptCall{
		kind: "retarget", token: priorAgentSessionID, sessionID: sessionID,
		agentSessionID: newAgentSessionID, accountID: accountID,
	})
}
func (r *recordingRegistrar) got() []adoptCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]adoptCall, len(r.calls))
	copy(out, r.calls)
	return out
}

func strptr(s string) *string { return &s }

// adoptSweepCfg builds a single-session/single-chat sweep Lifecycle.
type adoptSweepCfg struct {
	sessAgent   string // default "claude"
	chatAgent   string // default "claude"
	sessAccount *string
	chatAccount *string
	defaultAcct string // when non-empty, wires a resolver returning it
	proxyPort   int    // default 44127
	enabled     bool   // default true
	hasSession  bool   // default true
	bakedURL    string // default canonical at proxyPort with paneTok
	unbound     bool   // when true, force lc.proxyPort = 0 after building
	emptyEnv    bool   // when true, the pane has no ANTHROPIC_BASE_URL at all
}

func newAdoptSweepLifecycle(t *testing.T, cfg adoptSweepCfg) (*Lifecycle, *recordingRegistrar) {
	t.Helper()
	if cfg.sessAgent == "" {
		cfg.sessAgent = "claude"
	}
	if cfg.chatAgent == "" {
		cfg.chatAgent = "claude"
	}
	if cfg.proxyPort == 0 {
		cfg.proxyPort = 44127
	}
	if cfg.emptyEnv {
		cfg.bakedURL = "" // stub returns unknown-variable → ShowEnv ok=false
	} else if cfg.bakedURL == "" {
		cfg.bakedURL = canonicalBakedURL(cfg.proxyPort)
	}

	sessions := newMockSessionStore()
	sessions.sessions["sess-1"] = &models.Session{
		ID: "sess-1", RepoID: "repo-1", AgentName: cfg.sessAgent, AccountID: cfg.sessAccount,
	}
	name := "boss-repo-1-agent-01"
	chats := &mockAgentChatStore{chatsWithTmux: []*models.AgentChat{{
		ID:              "chat-1",
		SessionID:       "sess-1",
		AgentSessionID:  "agent-01",
		AgentName:       cfg.chatAgent,
		AccountID:       cfg.chatAccount,
		TmuxSessionName: &name,
	}}}

	reg := &recordingRegistrar{}
	lc := &Lifecycle{
		sessions:       sessions,
		agentChats:     chats,
		tmux:           stubTmuxForSweep(cfg.hasSession, cfg.bakedURL),
		logger:         zerolog.Nop(),
		proxyPort:      cfg.proxyPort,
		proxyRegistrar: reg,
	}
	on := cfg.enabled
	lc.SetRotationConfig(config.ManagedAccountsConfig{FailoverProxy: &on})
	lc.SetRotationRecorder(rotation.NewRecorder(&captureAuditStore{}, zerolog.Nop()))
	if cfg.defaultAcct != "" {
		want := cfg.defaultAcct
		lc.SetDefaultAccountResolver(func(_ context.Context, provider string, _ time.Time) (string, error) {
			if provider != "claude" {
				t.Errorf("resolver provider = %q, want claude", provider)
			}
			return want, nil
		})
	}
	return lc, reg
}

func TestAdoptSurvivingPaneProxyTokens(t *testing.T) {
	cases := []struct {
		name string
		cfg  adoptSweepCfg
		want []adoptCall
	}{
		{
			name: "same-agent unbound chat inherits session account -> SESSION shape",
			cfg:  adoptSweepCfg{enabled: true, hasSession: true, sessAccount: strptr("acct-sess")},
			want: []adoptCall{{kind: "session", token: paneTok, sessionID: "sess-1"}},
		},
		{
			name: "chat-bound -> CHAT shape with chat account",
			cfg:  adoptSweepCfg{enabled: true, hasSession: true, chatAccount: strptr("acct-chat")},
			want: []adoptCall{{kind: "chat", token: paneTok, sessionID: "sess-1", agentSessionID: "agent-01", accountID: "acct-chat"}},
		},
		{
			name: "both nil + default resolves -> CHAT shape with default",
			cfg:  adoptSweepCfg{enabled: true, hasSession: true, defaultAcct: "acct-def"},
			want: []adoptCall{{kind: "chat", token: paneTok, sessionID: "sess-1", agentSessionID: "agent-01", accountID: "acct-def"}},
		},
		{
			name: "both nil + no resolver -> skip",
			cfg:  adoptSweepCfg{enabled: true, hasSession: true},
			want: nil,
		},
		{
			name: "cross-agent chat -> CHAT shape with provider default",
			cfg:  adoptSweepCfg{enabled: true, hasSession: true, sessAgent: "codex", chatAgent: "claude", defaultAcct: "acct-def"},
			want: []adoptCall{{kind: "chat", token: paneTok, sessionID: "sess-1", agentSessionID: "agent-01", accountID: "acct-def"}},
		},
		{
			name: "non-claude chat -> skip (filtered)",
			cfg:  adoptSweepCfg{enabled: true, hasSession: true, chatAgent: "codex", chatAccount: strptr("acct-chat")},
			want: nil,
		},
		{
			name: "port mismatch -> skip (left to stale sweep)",
			cfg:  adoptSweepCfg{enabled: true, hasSession: true, sessAccount: strptr("acct-sess"), bakedURL: canonicalBakedURL(56249)},
			want: nil,
		},
		{
			name: "non-canonical token -> skip",
			cfg:  adoptSweepCfg{enabled: true, hasSession: true, sessAccount: strptr("acct-sess"), bakedURL: "http://127.0.0.1:44127/s/short"},
			want: nil,
		},
		{
			name: "dead pane -> skip",
			cfg:  adoptSweepCfg{enabled: true, hasSession: false, sessAccount: strptr("acct-sess")},
			want: nil,
		},
		{
			name: "no baked url -> skip",
			cfg:  adoptSweepCfg{enabled: true, hasSession: true, sessAccount: strptr("acct-sess"), emptyEnv: true},
			want: nil,
		},
		{
			name: "proxy disabled -> inert",
			cfg:  adoptSweepCfg{enabled: false, hasSession: true, sessAccount: strptr("acct-sess")},
			want: nil,
		},
		{
			name: "proxy unbound (port 0) -> inert",
			cfg:  adoptSweepCfg{enabled: true, hasSession: true, sessAccount: strptr("acct-sess"), unbound: true},
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lc, reg := newAdoptSweepLifecycle(t, tc.cfg)
			if tc.cfg.unbound {
				lc.proxyPort = 0
			}

			lc.adoptSurvivingPaneProxyTokens(context.Background())

			got := reg.got()
			if len(got) != len(tc.want) {
				t.Fatalf("adopt calls = %+v, want %+v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("adopt call[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestAdoptSweep_abortsOnListError proves a chat-list failure aborts the whole
// sweep (no adopt calls) rather than panicking or partial-processing. The sweep
// reads ListRoutableChats since BOS-979 (ListWithTmuxSession filtered out the
// NULL-tmux_session_name rows the fallback exists for), so the error is injected
// on that method.
func TestAdoptSweep_abortsOnListError(t *testing.T) {
	lc, reg := newAdoptSweepLifecycle(t, adoptSweepCfg{enabled: true, hasSession: true, sessAccount: strptr("acct-sess")})
	lc.agentChats.(*mockAgentChatStore).listRoutableErr = errors.New("db down")
	lc.adoptSurvivingPaneProxyTokens(context.Background())
	if len(reg.got()) != 0 {
		t.Fatalf("adopt calls on list error = %+v, want none", reg.got())
	}
}

// TestAdoptSweep_perRowSessionGetErrorSkipsRow proves a per-row sessions.Get miss
// skips only that row; a healthy sibling row is still adopted.
func TestAdoptSweep_perRowSessionGetErrorSkipsRow(t *testing.T) {
	sessions := newMockSessionStore()
	sessions.sessions["sess-live"] = &models.Session{ID: "sess-live", RepoID: "repo-1", AgentName: "claude", AccountID: strptr("acct-live")}
	nameA := "boss-repo-1-agent-A"
	nameB := "boss-repo-1-agent-B"
	chats := &mockAgentChatStore{chatsWithTmux: []*models.AgentChat{
		{ID: "chat-missing", SessionID: "sess-gone", AgentSessionID: "agent-A", AgentName: "claude", TmuxSessionName: &nameA},
		{ID: "chat-live", SessionID: "sess-live", AgentSessionID: "agent-B", AgentName: "claude", TmuxSessionName: &nameB},
	}}
	reg := &recordingRegistrar{}
	lc := &Lifecycle{
		sessions: sessions, agentChats: chats,
		tmux:           stubTmuxForSweep(true, canonicalBakedURL(44127)),
		logger:         zerolog.Nop(),
		proxyPort:      44127,
		proxyRegistrar: reg,
	}
	on := true
	lc.SetRotationConfig(config.ManagedAccountsConfig{FailoverProxy: &on})
	lc.SetRotationRecorder(rotation.NewRecorder(&captureAuditStore{}, zerolog.Nop()))

	lc.adoptSurvivingPaneProxyTokens(context.Background())

	got := reg.got()
	want := []adoptCall{{kind: "session", token: paneTok, sessionID: "sess-live"}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("adopt calls = %+v, want exactly %+v", got, want)
	}
}

// TestAdoptSweep_siblingsShareSessionToken proves two same-agent unbound sibling
// chats each yield a SESSION-shape adopt of the same token (idempotency is the
// registrar's job, verified in the server package).
func TestAdoptSweep_siblingsShareSessionToken(t *testing.T) {
	sessions := newMockSessionStore()
	sessions.sessions["sess-1"] = &models.Session{ID: "sess-1", RepoID: "repo-1", AgentName: "claude", AccountID: strptr("acct-sess")}
	nameA := "boss-repo-1-agent-A"
	nameB := "boss-repo-1-agent-B"
	chats := &mockAgentChatStore{chatsWithTmux: []*models.AgentChat{
		{ID: "chat-A", SessionID: "sess-1", AgentSessionID: "agent-A", AgentName: "claude", TmuxSessionName: &nameA},
		{ID: "chat-B", SessionID: "sess-1", AgentSessionID: "agent-B", AgentName: "claude", TmuxSessionName: &nameB},
	}}
	reg := &recordingRegistrar{}
	lc := &Lifecycle{
		sessions: sessions, agentChats: chats,
		tmux:           stubTmuxForSweep(true, canonicalBakedURL(44127)),
		logger:         zerolog.Nop(),
		proxyPort:      44127,
		proxyRegistrar: reg,
	}
	on := true
	lc.SetRotationConfig(config.ManagedAccountsConfig{FailoverProxy: &on})
	lc.SetRotationRecorder(rotation.NewRecorder(&captureAuditStore{}, zerolog.Nop()))

	lc.adoptSurvivingPaneProxyTokens(context.Background())

	got := reg.got()
	if len(got) != 2 {
		t.Fatalf("adopt calls = %d, want 2 (one per sibling)", len(got))
	}
	for _, c := range got {
		if c.kind != "session" || c.token != paneTok || c.sessionID != "sess-1" {
			t.Fatalf("sibling adopt = %+v, want session/paneTok/sess-1", c)
		}
	}
}

// TestAdoptSweep_inertWithoutRegistrarOrChats proves the nil-dependency guards.
func TestAdoptSweep_inertWithoutRegistrarOrChats(t *testing.T) {
	lc, reg := newAdoptSweepLifecycle(t, adoptSweepCfg{enabled: true, hasSession: true, sessAccount: strptr("acct-sess")})
	lc.proxyRegistrar = nil
	lc.adoptSurvivingPaneProxyTokens(context.Background()) // must not panic
	if len(reg.got()) != 0 {
		t.Fatalf("adopt calls with nil registrar = %+v, want none", reg.got())
	}

	lc2, reg2 := newAdoptSweepLifecycle(t, adoptSweepCfg{enabled: true, hasSession: true, sessAccount: strptr("acct-sess")})
	lc2.agentChats = nil
	lc2.adoptSurvivingPaneProxyTokens(context.Background()) // must not panic
	if len(reg2.got()) != 0 {
		t.Fatalf("adopt calls with nil agentChats = %+v, want none", reg2.got())
	}
}

// --- BOS-979: rebuild-before-sweep ordering, and the sweep's NULL-name gap ---

// recordingTmux is a tmux stub that RECORDS every session name it is asked
// about. stubTmuxForSweep ignores the name argument entirely, so it cannot tell
// a sweep that read chat.TmuxSessionName from one that derived the name via
// tmux.ChatSessionName — which is exactly the distinction these tests turn on.
type recordingTmux struct {
	mu       sync.Mutex
	asked    []string
	alive    map[string]bool
	baked    string
	askedEnv []string
}

func newRecordingTmux(baked string, alive ...string) (*tmux.Client, *recordingTmux) {
	rec := &recordingTmux{alive: map[string]bool{}, baked: baked}
	for _, name := range alive {
		rec.alive[name] = true
	}
	client := tmux.NewClient(tmux.WithCommandFactory(func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		sub, target := "", ""
		if len(args) > 0 {
			sub = args[0]
		}
		// Both `has-session -t <name>` and `show-environment -t <name> VAR`
		// carry the target after -t.
		for i, a := range args {
			if a == "-t" && i+1 < len(args) {
				target = args[i+1]
			}
		}
		rec.mu.Lock()
		switch sub {
		case "has-session":
			rec.asked = append(rec.asked, target)
		case "show-environment":
			rec.askedEnv = append(rec.askedEnv, target)
		}
		live := rec.alive[target]
		baked := rec.baked
		rec.mu.Unlock()

		switch sub {
		case "has-session":
			if live {
				return exec.CommandContext(ctx, "true")
			}
			return exec.CommandContext(ctx, "sh", "-c", "printf '%s' \"can't find session\" >&2; exit 1")
		case "show-environment":
			if baked == "" {
				return exec.CommandContext(ctx, "sh", "-c", "printf '%s' 'unknown variable' >&2; exit 1")
			}
			return exec.CommandContext(ctx, "sh", "-c", "printf '%s\\n' 'ANTHROPIC_BASE_URL="+baked+"'")
		default:
			return exec.CommandContext(ctx, "true")
		}
	}))
	return client, rec
}

func (r *recordingTmux) askedAbout(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, got := range r.asked {
		if got == name {
			return true
		}
	}
	return false
}

// TestAdoptSweep_considersChatsWithNoPersistedTmuxName closes the BOS-979 gap.
//
// The sweep has always carried a `tmux.ChatSessionName(repoID, agentSessionID)`
// fallback for a chat whose tmux_session_name is NULL — but it read the chat
// list through ListWithTmuxSession, whose WHERE clause excludes precisely those
// rows, so the fallback was unreachable code. A pane whose name was never
// persisted (or was cleared on a partial write) held a live baked token that no
// restart could ever recover.
//
// This asserts the fallback now RUNS: with TmuxSessionName nil the sweep asks
// tmux about the derived name and adopts the pane's token.
func TestAdoptSweep_considersChatsWithNoPersistedTmuxName(t *testing.T) {
	derived := tmux.ChatSessionName("repo-1", "agent-01")

	sessions := newMockSessionStore()
	sessions.sessions["sess-1"] = &models.Session{
		ID: "sess-1", RepoID: "repo-1", AgentName: "claude", AccountID: strptr("acct-sess"),
	}
	chats := &mockAgentChatStore{chatsWithTmux: []*models.AgentChat{{
		ID:             "chat-1",
		SessionID:      "sess-1",
		AgentSessionID: "agent-01",
		AgentName:      "claude",
		// The whole point: no persisted tmux session name.
		TmuxSessionName: nil,
	}}}

	tmuxClient, rec := newRecordingTmux(canonicalBakedURL(44127), derived)
	reg := &recordingRegistrar{}
	lc := &Lifecycle{
		sessions: sessions, agentChats: chats, tmux: tmuxClient,
		logger: zerolog.Nop(), proxyPort: 44127, proxyRegistrar: reg,
	}
	on := true
	lc.SetRotationConfig(config.ManagedAccountsConfig{FailoverProxy: &on})
	lc.SetRotationRecorder(rotation.NewRecorder(&captureAuditStore{}, zerolog.Nop()))

	lc.adoptSurvivingPaneProxyTokens(context.Background())

	if !rec.askedAbout(derived) {
		t.Fatalf("sweep never asked tmux about the derived name %q; asked=%v", derived, rec.asked)
	}
	got := reg.got()
	want := []adoptCall{{kind: "session", token: paneTok, sessionID: "sess-1"}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("NULL-tmux-name chat not adopted: got %+v want %+v", got, want)
	}
}

// TestAdoptSweep_stillAdoptsAPaneWithNoPersistedRow is the other half of AC7:
// the rebuild becoming the primary path must not have disabled the fallback.
// The registrar here restores nothing on rebuild (as it would for a pane spawned
// before the migration existed), and the sweep still recovers the pane.
func TestAdoptSweep_stillAdoptsAPaneWithNoPersistedRow(t *testing.T) {
	lc, reg := newAdoptSweepLifecycle(t, adoptSweepCfg{
		enabled: true, hasSession: true, sessAccount: strptr("acct-sess"),
	})

	lc.rebuildProxyTokenRegistry(context.Background()) // restores nothing
	lc.adoptSurvivingPaneProxyTokens(context.Background())

	got := reg.got()
	if len(got) != 2 || got[0].kind != "rebuild" ||
		got[1] != (adoptCall{kind: "session", token: paneTok, sessionID: "sess-1"}) {
		t.Fatalf("pane with no persisted row was not adopted by the fallback: %+v", got)
	}
}

// TestAdoptSweep_survivesARebuildFailure proves a rebuild error degrades to the
// pre-BOS-979 behaviour instead of aborting recovery: the sweep still runs and
// still adopts. A durable-read failure that also killed the fallback would turn
// one bad boot into every surviving pane wedging.
func TestAdoptSweep_survivesARebuildFailure(t *testing.T) {
	lc, reg := newAdoptSweepLifecycle(t, adoptSweepCfg{
		enabled: true, hasSession: true, sessAccount: strptr("acct-sess"),
	})
	reg.rebuildErr = errors.New("durable registry unreadable")

	lc.rebuildProxyTokenRegistry(context.Background())
	lc.adoptSurvivingPaneProxyTokens(context.Background())

	got := reg.got()
	if len(got) != 2 || got[1].kind != "session" || got[1].token != paneTok {
		t.Fatalf("sweep did not run after a rebuild failure: %+v", got)
	}
}

// TestBootstrap_rebuildsRegistryBeforeTheSweep is AC6's ordering assertion. It
// runs the REAL Bootstrap and reads the registrar's call log, rather than
// asserting on the source, so reordering the two statements fails the test.
//
// Order matters for correctness, not tidiness: a persisted row was written by
// the daemon that actually minted the token, while the sweep RECONSTRUCTS one
// from tmux env. Because both AdoptToken and the rebuild keep whatever is
// already registered for a digest, whichever runs first wins — so running the
// sweep first would let a tmux-env reconstruction outrank the authoritative row.
func TestBootstrap_rebuildsRegistryBeforeTheSweep(t *testing.T) {
	sessions := newMockSessionStore()
	sessions.sessions["sess-1"] = &models.Session{
		ID: "sess-1", RepoID: "repo-1", AgentName: "claude", AccountID: strptr("acct-sess"),
	}
	name := "boss-repo-1-agent-01"
	chats := &mockAgentChatStore{chatsWithTmux: []*models.AgentChat{{
		ID: "chat-1", SessionID: "sess-1", AgentSessionID: "agent-01",
		AgentName: "claude", TmuxSessionName: &name,
	}}}

	tmuxClient, _ := newRecordingTmux(canonicalBakedURL(44127), name)
	lc := newTestLifecycle(
		sessions, newMockRepoStore(), chats, &stubCronJobStore{},
		&mockWorktreeManager{}, newMockAgentRunner(), tmuxClient,
		newMockVCSProvider(), zerolog.Nop(),
	)
	reg := &recordingRegistrar{}
	lc.SetProxyRegistrar(reg)
	lc.SetProxyPort(44127)
	on := true
	lc.SetRotationConfig(config.ManagedAccountsConfig{FailoverProxy: &on})
	lc.SetRotationRecorder(rotation.NewRecorder(&captureAuditStore{}, zerolog.Nop()))

	lc.Bootstrap(context.Background())

	got := reg.got()
	var rebuildAt, adoptAt = -1, -1
	for i, call := range got {
		switch {
		case call.kind == "rebuild" && rebuildAt < 0:
			rebuildAt = i
		case (call.kind == "session" || call.kind == "chat") && adoptAt < 0:
			adoptAt = i
		}
	}
	if rebuildAt < 0 {
		t.Fatalf("Bootstrap never rebuilt the durable registry: %+v", got)
	}
	if adoptAt < 0 {
		t.Fatalf("Bootstrap never ran the adoption sweep, so the ordering is unproven: %+v", got)
	}
	if rebuildAt > adoptAt {
		t.Fatalf("Bootstrap ran the sweep before the rebuild (rebuild=%d adopt=%d): %+v", rebuildAt, adoptAt, got)
	}
}
