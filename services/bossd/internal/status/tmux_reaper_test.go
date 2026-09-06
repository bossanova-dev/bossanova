package status

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossalib/config"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/tmux"
	"github.com/rs/zerolog"
)

// --- fixture ----------------------------------------------------------------

const reaperTestGrace = time.Minute

const (
	// Ids are shaped like sqlutil.NewID() output: 16 lowercase hex characters.
	reaperRepoID     = "aaaaaaaa11111111"
	reaperRepoPrefix = "aaaaaaaa"
	reaperSessionID  = "cccccccc33333333"
	reaperChatID     = "bbbbbbbb22222222"
	// The pane a chat with agent session id reaperChatID would be given.
	reaperChatPane = "boss-aaaaaaaa-bbbbbbbb"
	reaperDaemonID = "daemon-under-test"

	// The chat the idle-reap tests target. Its recorded pointer and its
	// recomputed name agree, so it resolves to exactly one owner.
	reaperIdleChatID   = "dddddddd44444444"
	reaperIdleChatPane = "boss-aaaaaaaa-dddddddd"
)

// reaperRepoStore is the minimum db.RepoStore the reaper needs: List, plus
// error injection for the fail-closed tests.
type reaperRepoStore struct {
	repos   []*models.Repo
	listErr error
}

func (s *reaperRepoStore) Create(context.Context, db.CreateRepoParams) (*models.Repo, error) {
	return nil, nil
}
func (s *reaperRepoStore) Get(context.Context, string) (*models.Repo, error)         { return nil, nil }
func (s *reaperRepoStore) GetByPath(context.Context, string) (*models.Repo, error)   { return nil, nil }
func (s *reaperRepoStore) GetByOrigin(context.Context, string) (*models.Repo, error) { return nil, nil }
func (s *reaperRepoStore) List(context.Context) ([]*models.Repo, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.repos, nil
}
func (s *reaperRepoStore) Update(context.Context, string, db.UpdateRepoParams) (*models.Repo, error) {
	return nil, nil
}
func (s *reaperRepoStore) Delete(context.Context, string) error { return nil }

// reaperSessionStore embeds the package's existing mock so only the two reads
// the reaper makes need overriding.
type reaperSessionStore struct {
	*mockSessionStore
	all           []*models.Session
	recordedNames []string
	listErr       error
	namesErr      error
}

func (s *reaperSessionStore) List(_ context.Context, _ string) ([]*models.Session, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.all, nil
}

func (s *reaperSessionStore) ListTmuxSessionNames(_ context.Context) ([]string, error) {
	if s.namesErr != nil {
		return nil, s.namesErr
	}
	return s.recordedNames, nil
}

// reaperChatStore embeds the package's existing mock and overrides only the
// global read the whitelist is assembled from.
type reaperChatStore struct {
	*mockChatStore
	all     []*models.AgentChat
	listErr error
}

func (s *reaperChatStore) ListRoutableChats(_ context.Context) ([]*models.AgentChat, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.all, nil
}

type reaperFixture struct {
	t        *testing.T
	tmuxFake *mockTmuxFactory
	chats    *reaperChatStore
	sessions *reaperSessionStore
	repos    *reaperRepoStore
	reaper   *TmuxReaper
	logs     *bytes.Buffer

	now    time.Time
	killed []string

	// tracker is the same status tracker production wires in. Tests seed it
	// with Update to give the idle path its evidence; leaving it empty is what
	// keeps every pre-BOS-886 test's panes alive, since no entry means unknown.
	tracker *Tracker

	// killedChats records the (sessionID, agentSessionID, paneName) triples the
	// idle path tore down, and killChatErr makes that seam fail so the retry
	// contract can be asserted.
	killedChats []string
	killChatErr error

	// wantsChatTeardown opts a test out of the fixture's default assertion that
	// NOTHING reached the idle teardown seam. Only a test whose subject is a
	// successful idle reap should set it.
	wantsChatTeardown bool

	// transcriptExists is the oracle's answer, and transcriptProbes counts how
	// many times it was consulted — the idle predicate must reach it only for
	// candidates every cheaper gate has already passed.
	transcriptExists bool
	transcriptProbes int
	transcriptArgs   []transcriptProbe
}

type transcriptProbe struct {
	agentName      string
	workDir        string
	agentSessionID string
}

// reaperConfig builds a TmuxReaperConfig with every tri-state boolean set
// explicitly, so a test never depends on which way a nil happens to default.
func reaperConfig(enabled, dryRun, reapUnstamped bool, sweep time.Duration) config.TmuxReaperConfig {
	return config.TmuxReaperConfig{
		Enabled:              &enabled,
		DryRun:               &dryRun,
		ReapUnstamped:        &reapUnstamped,
		SweepIntervalSeconds: int(sweep / time.Second),
		GracePeriodSeconds:   int(reaperTestGrace / time.Second),
	}
}

// reaperIdleConfig builds a TmuxIdleReapConfig with both tri-states set
// explicitly, for the same reason reaperConfig does.
func reaperIdleConfig(enabled, dryRun bool) config.TmuxIdleReapConfig {
	return config.TmuxIdleReapConfig{
		Enabled:              &enabled,
		DryRun:               &dryRun,
		IdleThresholdSeconds: int(reaperTestIdleThreshold / time.Second),
	}
}

// reaperTestIdleThreshold is the idle window every fixture uses. It is far
// longer than any orphan window in this file so the two paths' clocks can never
// be confused for one another.
const reaperTestIdleThreshold = 8 * time.Hour

// fixtureTranscriptOracle routes the oracle back to the fixture so a test can
// both control the answer and count the calls.
type fixtureTranscriptOracle struct{ fx *reaperFixture }

func (o fixtureTranscriptOracle) TranscriptExists(_ context.Context, agentName, workDir, agentSessionID string) bool {
	o.fx.transcriptProbes++
	o.fx.transcriptArgs = append(o.fx.transcriptArgs, transcriptProbe{
		agentName:      agentName,
		workDir:        workDir,
		agentSessionID: agentSessionID,
	})
	return o.fx.transcriptExists
}

// newReaperFixture wires a reaper whose tmux client is a fake command factory
// AND whose destructive seam records instead of executing.
//
// The two together are what keep `make test-bossd` from reaping the
// developer's own live agent panes (BOS-846 D8, the concrete BOS-349 failure).
// The registered cleanup is the explicit assertion that no test path reached a
// real `tmux kill-session`: the seam bypasses the tmux client entirely, so the
// fake's kill ledger must stay empty in every single reaper test.
//
// The idle path (BOS-886) is wired ENABLED here, matching production, so every
// pre-existing test in this file doubles as a fence for it: those panes are
// accounted for and now fall THROUGH to the idle gate, and they stay alive only
// because the fixture's tracker is empty and an absent entry means unknown.
// Turning the idle path off in the fixture would make that fence vacuous.
func newReaperFixture(t *testing.T, cfg config.TmuxReaperConfig) *reaperFixture {
	t.Helper()
	return newReaperFixtureWithIdle(t, cfg, reaperIdleConfig(true, false))
}

func newReaperFixtureWithIdle(t *testing.T, cfg config.TmuxReaperConfig, idleCfg config.TmuxIdleReapConfig) *reaperFixture {
	t.Helper()

	fx := &reaperFixture{
		t: t,
		tmuxFake: &mockTmuxFactory{
			sessions: map[string]bool{},
			captures: map[string]string{},
			dead:     map[string]bool{},
			killed:   map[string]bool{},
			created:  map[string]time.Time{},
			env:      map[string]map[string]string{},
			attached: map[string]int{},
		},
		chats:    &reaperChatStore{mockChatStore: &mockChatStore{chats: map[string]*models.AgentChat{}}},
		sessions: &reaperSessionStore{mockSessionStore: &mockSessionStore{sessions: map[string]*models.Session{}}},
		repos:    &reaperRepoStore{repos: []*models.Repo{{ID: reaperRepoID}}},
		logs:     &bytes.Buffer{},
		now:      time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC),
		tracker:  NewTracker(),
		// The permissive answer, so a test that means to prove D5 has to opt
		// into the missing-transcript case explicitly rather than inheriting it.
		transcriptExists: true,
	}

	logger := zerolog.New(fx.logs).Level(zerolog.TraceLevel)
	client := tmux.NewClient(tmux.WithCommandFactory(fx.tmuxFake.factory))
	fx.reaper = NewTmuxReaper(fx.chats, fx.sessions, fx.repos, client, reaperDaemonID, cfg, IdleReapDeps{
		Config:      idleCfg,
		Tracker:     fx.tracker,
		Transcripts: fixtureTranscriptOracle{fx: fx},
		KillChat: func(_ context.Context, sessionID, agentSessionID, paneName string) error {
			if fx.killChatErr != nil {
				return fx.killChatErr
			}
			fx.killedChats = append(fx.killedChats, sessionID+"/"+agentSessionID+"/"+paneName)
			return nil
		},
	}, logger)
	fx.reaper.setClock(func() time.Time { return fx.now })
	fx.reaper.setKillSeam(func(_ context.Context, name string) error {
		fx.killed = append(fx.killed, name)
		fx.tmuxFake.mu.Lock()
		delete(fx.tmuxFake.sessions, name)
		fx.tmuxFake.mu.Unlock()
		return nil
	})

	t.Cleanup(func() {
		fx.tmuxFake.mu.Lock()
		defer fx.tmuxFake.mu.Unlock()
		if len(fx.tmuxFake.killed) != 0 {
			t.Fatalf("kill seam was not stubbed: the reaper reached tmux kill-session for %v", fx.tmuxFake.killed)
		}
		// An idle reap does NOT go through fx.killed — it goes through the chat
		// teardown seam — so a test that only watches the orphan ledger would
		// pass through an idle-path regression. Every test is a fence for both
		// paths unless it opts in by setting wantsChatTeardown (BOS-886, the
		// other half of narrowing TestTmuxReaper_KeepsAccountedForPanes).
		if !fx.wantsChatTeardown && len(fx.killedChats) != 0 {
			t.Fatalf("idle chat teardown ran for %v; this test did not opt into a chat teardown", fx.killedChats)
		}
	})

	return fx
}

// addLivePane presents a pane to tmux, created at fixture-now minus age, and
// optionally stamped with a daemon id. A nil stamp leaves it unstamped, the way
// every pane predating BOS-846 is.
func (fx *reaperFixture) addLivePane(name string, age time.Duration, stamp *string) {
	fx.tmuxFake.mu.Lock()
	defer fx.tmuxFake.mu.Unlock()
	fx.tmuxFake.sessions[name] = true
	fx.tmuxFake.created[name] = fx.now.Add(-age)
	if stamp != nil {
		fx.tmuxFake.env[name] = map[string]string{tmux.DaemonIDEnvKey: *stamp}
	}
}

func (fx *reaperFixture) setAttached(name string, clients int) {
	fx.tmuxFake.mu.Lock()
	defer fx.tmuxFake.mu.Unlock()
	fx.tmuxFake.attached[name] = clients
}

// addSession registers a session row. recordedPane may be nil to model a row
// whose tmux_session_name was never written.
func (fx *reaperFixture) addSession(id string, recordedPane *string) {
	fx.sessions.all = append(fx.sessions.all, &models.Session{
		ID: id, RepoID: reaperRepoID, WorktreePath: "/tmp/reaper-worktree", TmuxSessionName: recordedPane,
	})
	if recordedPane != nil && *recordedPane != "" {
		fx.sessions.recordedNames = append(fx.sessions.recordedNames, *recordedPane)
	}
}

// addChat registers a chat row. recordedPane nil models the create race the
// recomputed whitelist leg exists for: the pane is live but the name column is
// still NULL.
func (fx *reaperFixture) addChat(agentSessionID string, recordedPane *string) {
	fx.chats.all = append(fx.chats.all, &models.AgentChat{
		ID: "chat-" + agentSessionID, SessionID: reaperSessionID,
		AgentSessionID: agentSessionID, TmuxSessionName: recordedPane,
	})
}

// addResumableChat registers reaperIdleChatID as a chat with the default
// provider-backed shape used by the older idle-reaper tests.
func (fx *reaperFixture) addResumableChat(recordedPane *string) {
	fx.addAgentChat(reaperIdleChatID, "claude", recordedPane, ptr("provider-"+reaperIdleChatID))
}

func (fx *reaperFixture) addAgentChat(agentSessionID, agentName string, recordedPane, providerSessionID *string) {
	fx.chats.all = append(fx.chats.all, &models.AgentChat{
		ID: "chat-" + agentSessionID, SessionID: reaperSessionID,
		AgentSessionID:    agentSessionID,
		AgentName:         agentName,
		TmuxSessionName:   recordedPane,
		ProviderSessionID: providerSessionID,
	})
}

// markIdle gives the tracker the two signals the idle predicate reads: a
// reported IDLE status and a RAW output clock that last moved idleFor ago.
//
// ReceivedAt is stamped with the real wall clock by Update, not the fixture's,
// because Tracker.Get's staleness gate is real-time. That is why every idle
// test seeds inline rather than pre-seeding and then advancing fx.now: the
// fixture clock may jump hours, but the entry must stay fresh.
func (fx *reaperFixture) markIdle(idleFor time.Duration) {
	fx.markIdleChat(reaperIdleChatID, idleFor)
}

func (fx *reaperFixture) markIdleChat(agentSessionID string, idleFor time.Duration) {
	fx.tracker.Update(agentSessionID, pb.ChatStatus_CHAT_STATUS_IDLE, fx.now.Add(-idleFor))
}

func ptr(s string) *string { return &s }

func (fx *reaperFixture) sweep() int {
	fx.t.Helper()
	n, err := fx.reaper.SweepOnce(context.Background())
	if err != nil {
		fx.t.Fatalf("SweepOnce: unexpected error: %v", err)
	}
	return n
}

// --- name gate --------------------------------------------------------------

func TestBossOwnedName(t *testing.T) {
	prefixes := map[string]struct{}{reaperRepoPrefix: {}}

	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"well-formed name for a known repo", reaperChatPane, true},
		{"a user's own boss-notes session", "boss-notes", false},
		{"the bare prefix bindDetachKeys matches on", "boss-", false},
		{"a lookalike prefix", "bossy-abc-def", false},
		{"uppercase hex", "boss-AAAAAAAA-bbbbbbbb", false},
		{"non-hex characters", "boss-aaaaaaag-bbbbbbbh", false},
		{"a plain user session", "work", false},
		{"seven-character components", "boss-aaaaaaa-bbbbbbb", false},
		{"nine-character components", "boss-aaaaaaaaa-bbbbbbbbb", false},
		{"well-formed but for an unknown repo", "boss-deadbeef-bbbbbbbb", false},
		{"trailing content after a valid name", reaperChatPane + "-extra", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := bossOwnedName(tc.in, prefixes); got != tc.want {
				t.Fatalf("bossOwnedName(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// --- whitelist assembly -----------------------------------------------------

func TestTmuxReaper_Whitelist(t *testing.T) {
	t.Run("a chat with a recorded name contributes it", func(t *testing.T) {
		fx := newReaperFixture(t, reaperConfig(true, false, false, time.Minute))
		fx.addChat("dddddddd44444444", ptr("boss-aaaaaaaa-recorded"))

		wl := fx.whitelist()
		if _, ok := wl["boss-aaaaaaaa-recorded"]; !ok {
			t.Fatalf("recorded chat name missing from whitelist: %v", wl)
		}
	})

	// This is the load-bearing leg (BOS-846 D3). StartTmuxChat creates the pane
	// ~70 lines and one agent launch before it persists the name, so deleting
	// the recomputed leg makes the reaper kill panes out from under chats that
	// are still being created. This test fails if that leg is removed.
	t.Run("a chat with no recorded name still contributes its recomputed name", func(t *testing.T) {
		fx := newReaperFixture(t, reaperConfig(true, false, false, time.Minute))
		fx.addSession(reaperSessionID, nil)
		fx.addChat(reaperChatID, nil)

		wl := fx.whitelist()
		if _, ok := wl[reaperChatPane]; !ok {
			t.Fatalf("recomputed chat name %q missing from whitelist: %v", reaperChatPane, wl)
		}
	})

	t.Run("a legacy session row contributes its recorded name", func(t *testing.T) {
		fx := newReaperFixture(t, reaperConfig(true, false, false, time.Minute))
		fx.addSession(reaperSessionID, ptr("boss-aaaaaaaa-legacy01"))

		wl := fx.whitelist()
		if _, ok := wl["boss-aaaaaaaa-legacy01"]; !ok {
			t.Fatalf("recorded session name missing from whitelist: %v", wl)
		}
		// And the recomputed session leg.
		if _, ok := wl[tmux.SessionName(reaperRepoID, reaperSessionID)]; !ok {
			t.Fatalf("recomputed session name missing from whitelist: %v", wl)
		}
	})

	t.Run("rows from multiple repos all contribute", func(t *testing.T) {
		fx := newReaperFixture(t, reaperConfig(true, false, false, time.Minute))
		const otherRepo = "eeeeeeee55555555"
		fx.repos.repos = append(fx.repos.repos, &models.Repo{ID: otherRepo})
		fx.addSession(reaperSessionID, nil)
		fx.sessions.all = append(fx.sessions.all, &models.Session{
			ID: "ffffffff66666666", RepoID: otherRepo,
		})
		fx.addChat(reaperChatID, nil)

		wl := fx.whitelist()
		for _, want := range []string{
			tmux.SessionName(reaperRepoID, reaperSessionID),
			tmux.SessionName(otherRepo, "ffffffff66666666"),
			reaperChatPane,
		} {
			if _, ok := wl[want]; !ok {
				t.Fatalf("%q missing from whitelist: %v", want, wl)
			}
		}
	})
}

// whitelist is a test-side accessor for the assembled accounted-for set.
func (fx *reaperFixture) whitelist() map[string]struct{} {
	fx.t.Helper()
	wl, _, err := fx.reaper.accountedFor(context.Background())
	if err != nil {
		fx.t.Fatalf("accountedFor: %v", err)
	}
	return wl
}

// owners is a test-side accessor for the identification map assembled beside
// the whitelist (BOS-886 D1).
func (fx *reaperFixture) owners() map[string]*chatOwner {
	fx.t.Helper()
	_, owners, err := fx.reaper.accountedFor(context.Background())
	if err != nil {
		fx.t.Fatalf("accountedFor: %v", err)
	}
	return owners
}

// --- classifier -------------------------------------------------------------

// armedFixture is the common setup for classifier tests: enabled, not dry-run,
// one accounted-for chat so the whitelist is never empty, and a one-minute
// grace/confirmation window.
func armedFixture(t *testing.T, reapUnstamped bool) *reaperFixture {
	t.Helper()
	fx := newReaperFixture(t, reaperConfig(true, false, reapUnstamped, time.Minute))
	fx.addSession(reaperSessionID, nil)
	fx.addChat(reaperChatID, nil)
	return fx
}

func TestTmuxReaper_ReapsConfirmedOrphan(t *testing.T) {
	fx := armedFixture(t, false)
	fx.addLivePane("boss-aaaaaaaa-99999999", time.Hour, ptr(reaperDaemonID))

	// First sweep: records the strike, kills nothing.
	if n := fx.sweep(); n != 0 {
		t.Fatalf("first sweep reaped %d, want 0 (confirmation strike not yet aged)", n)
	}
	if len(fx.killed) != 0 {
		t.Fatalf("first sweep killed %v, want nothing", fx.killed)
	}

	// Second sweep, past the confirmation window.
	fx.now = fx.now.Add(2 * time.Minute)
	if n := fx.sweep(); n != 1 {
		t.Fatalf("second sweep reaped %d, want 1", n)
	}
	if len(fx.killed) != 1 || fx.killed[0] != "boss-aaaaaaaa-99999999" {
		t.Fatalf("killed = %v, want [boss-aaaaaaaa-99999999]", fx.killed)
	}
}

// TestTmuxReaper_KeepsAccountedForPanes is BOS-846's regression fence for the
// identification rule, NARROWED by BOS-886 rather than deleted. Its claim used
// to be "accounted for ⇒ always kept"; it is now "accounted for ⇒ kept unless
// idle-eligible", and the final subtest is what stops the other three from
// re-reading as the old unconditional claim. The fixture arms the idle path, so
// those three hold because their panes fail an idle gate, not because the gate
// is absent: they have no tracker entry at all, which means unknown, not idle.
func TestTmuxReaper_KeepsAccountedForPanes(t *testing.T) {
	t.Run("whitelisted by the recorded leg", func(t *testing.T) {
		fx := armedFixture(t, false)
		fx.addSession("aaaa000011112222", ptr("boss-aaaaaaaa-recorded"))
		fx.addLivePane("boss-aaaaaaaa-recorded", time.Hour, ptr(reaperDaemonID))

		fx.sweep()
		fx.now = fx.now.Add(2 * time.Minute)
		fx.sweep()
		if len(fx.killed) != 0 {
			t.Fatalf("killed %v, want nothing: the pane is recorded", fx.killed)
		}
	})

	t.Run("whitelisted only by the recomputed leg", func(t *testing.T) {
		fx := armedFixture(t, false)
		// reaperChatPane's chat row exists with a NULL tmux_session_name.
		fx.addLivePane(reaperChatPane, time.Hour, ptr(reaperDaemonID))

		fx.sweep()
		fx.now = fx.now.Add(2 * time.Minute)
		fx.sweep()
		if len(fx.killed) != 0 {
			t.Fatalf("killed %v, want nothing: the recomputed leg accounts for it", fx.killed)
		}
	})

	t.Run("younger than the grace period", func(t *testing.T) {
		fx := armedFixture(t, false)
		fx.addLivePane("boss-aaaaaaaa-99999999", 10*time.Second, ptr(reaperDaemonID))

		fx.sweep()
		fx.sweep()
		if len(fx.killed) != 0 {
			t.Fatalf("killed %v, want nothing: the pane is inside its grace window", fx.killed)
		}
	})

	// The narrowing itself: being accounted for is no longer sufficient.
	t.Run("but not one that is idle past the window", func(t *testing.T) {
		fx := armedFixture(t, false)
		fx.wantsChatTeardown = true
		fx.addResumableChat(ptr(reaperIdleChatPane))
		fx.addLivePane(reaperIdleChatPane, time.Hour, ptr(reaperDaemonID))
		fx.markIdle(reaperTestIdleThreshold + time.Hour)

		fx.sweep()
		fx.now = fx.now.Add(2 * reaperTestGrace)
		if n := fx.sweep(); n != 1 {
			t.Fatalf("reaped %d, want 1: the pane is accounted for but idle past the window", n)
		}
		if len(fx.killedChats) != 1 {
			t.Fatalf("killedChats = %v, want one idle teardown", fx.killedChats)
		}
		// The orphan kill seam must NOT have been used: an idle reap goes
		// through the chat teardown so the pane pointer is cleared with it.
		if len(fx.killed) != 0 {
			t.Fatalf("orphan kill seam ran for %v; an idle reap must use the chat teardown", fx.killed)
		}
	})
}

func TestTmuxReaper_IdleTranscriptProbeUsesResolvedResumeID(t *testing.T) {
	tests := []struct {
		name        string
		agent       string
		provider    *string
		wantProbeID string
	}{
		{
			name:        "claude without provider session id probes the agent session id",
			agent:       "claude",
			wantProbeID: reaperIdleChatID,
		},
		{
			name:        "claude with provider session id probes the provider session id",
			agent:       "claude",
			provider:    ptr("claude-provider"),
			wantProbeID: "claude-provider",
		},
		{
			name:        "codex without provider session id probes the agent session id",
			agent:       "codex",
			wantProbeID: reaperIdleChatID,
		},
		{
			name:        "codex with provider session id probes the rollout id",
			agent:       "codex",
			provider:    ptr("codex-rollout"),
			wantProbeID: "codex-rollout",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fx := armedFixture(t, false)
			fx.addAgentChat(reaperIdleChatID, tc.agent, ptr(reaperIdleChatPane), tc.provider)
			fx.addLivePane(reaperIdleChatPane, time.Hour, ptr(reaperDaemonID))
			fx.markIdle(reaperTestIdleThreshold + time.Hour)

			if n := fx.sweep(); n != 0 {
				t.Fatalf("first sweep reaped %d, want 0 before the confirming sighting", n)
			}
			if fx.transcriptProbes != 1 {
				t.Fatalf("transcript probes = %d, want 1", fx.transcriptProbes)
			}
			got := fx.transcriptArgs[0]
			if got.agentName != tc.agent || got.workDir != "/tmp/reaper-worktree" || got.agentSessionID != tc.wantProbeID {
				t.Fatalf("transcript probe = %+v, want agent=%q workDir=/tmp/reaper-worktree session=%q", got, tc.agent, tc.wantProbeID)
			}
		})
	}
}

func TestTmuxReaper_IdleReapsClaudeWithoutProviderSessionWhenTranscriptExists(t *testing.T) {
	fx := armedFixture(t, false)
	fx.wantsChatTeardown = true
	fx.addAgentChat(reaperIdleChatID, "claude", ptr(reaperIdleChatPane), nil)
	fx.addLivePane(reaperIdleChatPane, time.Hour, ptr(reaperDaemonID))
	fx.markIdle(reaperTestIdleThreshold + time.Hour)

	if n := fx.sweep(); n != 0 {
		t.Fatalf("first sweep reaped %d, want 0 before confirmation", n)
	}
	fx.now = fx.now.Add(time.Nanosecond)
	if n := fx.sweep(); n != 1 {
		t.Fatalf("second sweep reaped %d, want 1", n)
	}
	if len(fx.killedChats) != 1 || fx.killedChats[0] != reaperSessionID+"/"+reaperIdleChatID+"/"+reaperIdleChatPane {
		t.Fatalf("killedChats = %v, want the idle Claude chat pane", fx.killedChats)
	}
}

func TestTmuxReaper_IdleKeepsClaudeWithoutProviderSessionWhenTranscriptMissing(t *testing.T) {
	fx := armedFixture(t, false)
	fx.transcriptExists = false
	fx.addAgentChat(reaperIdleChatID, "claude", ptr(reaperIdleChatPane), nil)
	fx.addLivePane(reaperIdleChatPane, time.Hour, ptr(reaperDaemonID))
	fx.markIdle(reaperTestIdleThreshold + time.Hour)

	if n := fx.sweep(); n != 0 {
		t.Fatalf("sweep reaped %d, want 0 without transcript", n)
	}
	if fx.transcriptProbes != 1 {
		t.Fatalf("transcript probes = %d, want 1", fx.transcriptProbes)
	}
	assertLogged(t, fx.logs, `"reason":"idleTranscriptMissing"`)
}

func TestTmuxReaper_IdleKeepsCodexWithoutProviderSessionWhenTranscriptMissing(t *testing.T) {
	fx := armedFixture(t, false)
	fx.transcriptExists = false
	fx.addAgentChat(reaperIdleChatID, "codex", ptr(reaperIdleChatPane), nil)
	fx.addLivePane(reaperIdleChatPane, time.Hour, ptr(reaperDaemonID))
	fx.markIdle(reaperTestIdleThreshold + time.Hour)

	if n := fx.sweep(); n != 0 {
		t.Fatalf("sweep reaped %d, want 0 for unbound codex chat", n)
	}
	if fx.transcriptProbes != 1 {
		t.Fatalf("transcript probes = %d, want 1", fx.transcriptProbes)
	}
	if got := fx.transcriptArgs[0].agentSessionID; got != reaperIdleChatID {
		t.Fatalf("codex probe session id = %q, want bare agent session id %q", got, reaperIdleChatID)
	}
	assertLogged(t, fx.logs, `"reason":"idleTranscriptMissing"`)
}

func TestTmuxReaper_IdleReapsCodexWithProviderSessionWhenTranscriptExists(t *testing.T) {
	fx := armedFixture(t, false)
	fx.wantsChatTeardown = true
	fx.addAgentChat(reaperIdleChatID, "codex", ptr(reaperIdleChatPane), ptr("codex-rollout"))
	fx.addLivePane(reaperIdleChatPane, time.Hour, ptr(reaperDaemonID))
	fx.markIdle(reaperTestIdleThreshold + time.Hour)

	fx.sweep()
	if got := fx.transcriptArgs[0].agentSessionID; got != "codex-rollout" {
		t.Fatalf("codex probe session id = %q, want provider rollout id", got)
	}
	if len(fx.killedChats) != 0 {
		t.Fatalf("first sweep killed %v, want confirmation first", fx.killedChats)
	}
	if n := fx.sweep(); n != 1 {
		t.Fatalf("second sweep reaped %d, want 1", n)
	}
}

func TestTmuxReaper_IdleKeepsDistinctTranscriptEvidenceFailures(t *testing.T) {
	t.Run("unresolvable resume id keeps without probing", func(t *testing.T) {
		fx := armedFixture(t, false)
		fx.addAgentChat("", "claude", ptr(reaperIdleChatPane), nil)
		fx.addLivePane(reaperIdleChatPane, time.Hour, ptr(reaperDaemonID))
		fx.markIdleChat("", reaperTestIdleThreshold+time.Hour)

		if n := fx.sweep(); n != 0 {
			t.Fatalf("sweep reaped %d, want 0 with empty resume id", n)
		}
		if fx.transcriptProbes != 0 {
			t.Fatalf("transcript probes = %d, want 0 with empty resume id", fx.transcriptProbes)
		}
		assertLogged(t, fx.logs, `"reason":"idleNoResumeSessionID"`)
	})

	t.Run("unwired oracle keeps with its own reason", func(t *testing.T) {
		fx := armedFixture(t, false)
		fx.reaper.idle.Transcripts = nil
		fx.addAgentChat(reaperIdleChatID, "claude", ptr(reaperIdleChatPane), nil)
		fx.addLivePane(reaperIdleChatPane, time.Hour, ptr(reaperDaemonID))
		fx.markIdle(reaperTestIdleThreshold + time.Hour)

		if n := fx.sweep(); n != 0 {
			t.Fatalf("sweep reaped %d, want 0 with no oracle", n)
		}
		if fx.transcriptProbes != 0 {
			t.Fatalf("transcript probes = %d, want 0 with no oracle", fx.transcriptProbes)
		}
		assertLogged(t, fx.logs, `"reason":"idleTranscriptOracleMissing"`)
	})

	t.Run("missing transcript keeps with its own reason", func(t *testing.T) {
		fx := armedFixture(t, false)
		fx.transcriptExists = false
		fx.addAgentChat(reaperIdleChatID, "claude", ptr(reaperIdleChatPane), nil)
		fx.addLivePane(reaperIdleChatPane, time.Hour, ptr(reaperDaemonID))
		fx.markIdle(reaperTestIdleThreshold + time.Hour)

		if n := fx.sweep(); n != 0 {
			t.Fatalf("sweep reaped %d, want 0 with missing transcript", n)
		}
		if fx.transcriptProbes != 1 {
			t.Fatalf("transcript probes = %d, want 1", fx.transcriptProbes)
		}
		assertLogged(t, fx.logs, `"reason":"idleTranscriptMissing"`)
	})
}

func TestTmuxReaper_IdleKeepsAttachedPaneWithoutTranscriptProbe(t *testing.T) {
	fx := armedFixture(t, false)
	fx.addAgentChat(reaperIdleChatID, "claude", ptr(reaperIdleChatPane), nil)
	fx.addLivePane(reaperIdleChatPane, time.Hour, ptr(reaperDaemonID))
	fx.setAttached(reaperIdleChatPane, 1)
	fx.markIdle(reaperTestIdleThreshold + time.Hour)

	if n := fx.sweep(); n != 0 {
		t.Fatalf("sweep reaped %d, want 0 while a client is attached", n)
	}
	if fx.transcriptProbes != 0 {
		t.Fatalf("transcript probes = %d, want 0 for attached pane", fx.transcriptProbes)
	}
	assertLogged(t, fx.logs, `"reason":"idleAttachedClient"`)
}

func TestTmuxReaper_StrikeClearedWhenCandidateBecomesAccountedFor(t *testing.T) {
	fx := armedFixture(t, false)
	const pane = "boss-aaaaaaaa-88888888"
	fx.addLivePane(pane, time.Hour, ptr(reaperDaemonID))

	if n := fx.sweep(); n != 0 {
		t.Fatalf("first sweep reaped %d, want 0", n)
	}
	if fx.reaper.strikeCount() != 1 {
		t.Fatalf("strikeCount = %d, want 1 after the first observation", fx.reaper.strikeCount())
	}

	// The DB catches up: a row now claims the pane.
	fx.addSession("aaaa999988887777", ptr(pane))

	fx.now = fx.now.Add(2 * time.Minute)
	if n := fx.sweep(); n != 0 {
		t.Fatalf("sweep after the row appeared reaped %d, want 0", n)
	}
	if len(fx.killed) != 0 {
		t.Fatalf("killed %v, want nothing", fx.killed)
	}
	if fx.reaper.strikeCount() != 0 {
		t.Fatalf("strikeCount = %d, want 0: an accounted-for observation clears the marker", fx.reaper.strikeCount())
	}
}

func TestTmuxReaper_OwnershipBuckets(t *testing.T) {
	t.Run("a foreign stamp is unattributable", func(t *testing.T) {
		fx := armedFixture(t, false)
		fx.addLivePane("boss-aaaaaaaa-77777777", time.Hour, ptr("some-other-daemon"))

		fx.sweep()
		fx.now = fx.now.Add(2 * time.Minute)
		fx.sweep()
		if len(fx.killed) != 0 {
			t.Fatalf("killed %v, want nothing: a peer daemon's pane is never ours to kill", fx.killed)
		}
		assertLogged(t, fx.logs, `"reason":"foreignDaemon"`)
	})

	t.Run("an unstamped pane is unattributable by default", func(t *testing.T) {
		fx := armedFixture(t, false)
		fx.addLivePane("boss-aaaaaaaa-66666666", time.Hour, nil)

		fx.sweep()
		fx.now = fx.now.Add(2 * time.Minute)
		fx.sweep()
		if len(fx.killed) != 0 {
			t.Fatalf("killed %v, want nothing: unstamped panes need an explicit opt-in", fx.killed)
		}
		assertLogged(t, fx.logs, `"reason":"unstamped"`)
	})

	t.Run("an unstamped pane is reaped with reap_unstamped", func(t *testing.T) {
		fx := armedFixture(t, true)
		fx.addLivePane("boss-aaaaaaaa-66666666", time.Hour, nil)

		fx.sweep()
		fx.now = fx.now.Add(2 * time.Minute)
		if n := fx.sweep(); n != 1 {
			t.Fatalf("reaped %d, want 1 with reap_unstamped set", n)
		}
	})
}

func TestTmuxReaper_EmptyWhitelistMakesEveryBossPaneUnattributable(t *testing.T) {
	// No chat and no session rows at all: the whitelist comes back empty while
	// boss-shaped panes are live. Far likelier a broken read than a host on
	// which every pane is an orphan.
	fx := newReaperFixture(t, reaperConfig(true, false, true, time.Minute))
	fx.addLivePane("boss-aaaaaaaa-55555555", time.Hour, ptr(reaperDaemonID))
	fx.addLivePane("boss-aaaaaaaa-44444444", time.Hour, ptr(reaperDaemonID))

	fx.sweep()
	fx.now = fx.now.Add(2 * time.Minute)
	fx.sweep()

	if len(fx.killed) != 0 {
		t.Fatalf("killed %v, want nothing on an empty whitelist", fx.killed)
	}
	assertLogged(t, fx.logs, `"reason":"emptyWhitelist"`)
}

// --- boundary ---------------------------------------------------------------

// TestTmuxReaper_GraceBoundary pins the `<` vs `<=` semantics of both windows.
// BOS-400 shipped a staleness bug where `-le` vs `-lt` produced zero winners in
// a steal race; both comparisons here are "at least grace old ⇒ eligible".
func TestTmuxReaper_GraceBoundary(t *testing.T) {
	const grace = reaperTestGrace
	const pane = "boss-aaaaaaaa-33333333"

	tests := []struct {
		name       string
		paneAge    time.Duration
		strikeAge  time.Duration
		wantBucket string
		wantReason string
	}{
		{"pane one nanosecond inside grace", grace - 1, 2 * grace, bucketKeep, reasonWithinGrace},
		{"pane exactly at grace", grace, 2 * grace, bucketReap, reasonUnaccountedForTwice},
		{"pane one nanosecond past grace", grace + 1, 2 * grace, bucketReap, reasonUnaccountedForTwice},
		{"strike one nanosecond inside grace", 2 * grace, grace - 1, bucketKeep, reasonAwaitingConfirm},
		{"strike exactly at grace", 2 * grace, grace, bucketReap, reasonUnaccountedForTwice},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fx := armedFixture(t, false)
			fx.addLivePane(pane, tc.paneAge, ptr(reaperDaemonID))
			fx.reaper.strikes[strikeKey{name: pane, reason: reasonUnaccountedForTwice}] = fx.now.Add(-tc.strikeAge)

			live := tmux.LiveSession{Name: pane, Created: fx.now.Add(-tc.paneAge)}
			dec := fx.reaper.classify(
				context.Background(), live, fx.whitelist(), fx.owners(), fx.now, grace)
			if dec.bucket != tc.wantBucket || dec.reason != tc.wantReason {
				t.Fatalf("classify = (%q, %q), want (%q, %q)", dec.bucket, dec.reason, tc.wantBucket, tc.wantReason)
			}
		})
	}
}

// --- fail-closed ------------------------------------------------------------

func TestTmuxReaper_FailsClosed(t *testing.T) {
	sentinel := errors.New("boom")

	tests := []struct {
		name  string
		arm   func(fx *reaperFixture)
		inLog string
	}{
		{
			name:  "list-sessions fails for a reason other than no server running",
			arm:   func(fx *reaperFixture) { fx.tmuxFake.listSessionsStderr = "lost server" },
			inLog: "list-sessions failed",
		},
		{
			name:  "the repo read fails",
			arm:   func(fx *reaperFixture) { fx.repos.listErr = sentinel },
			inLog: "repo read failed",
		},
		{
			name:  "the recorded-session-names read fails",
			arm:   func(fx *reaperFixture) { fx.sessions.namesErr = sentinel },
			inLog: "store read failed",
		},
		{
			name:  "the session list read fails",
			arm:   func(fx *reaperFixture) { fx.sessions.listErr = sentinel },
			inLog: "store read failed",
		},
		{
			name:  "the chat list read fails",
			arm:   func(fx *reaperFixture) { fx.chats.listErr = sentinel },
			inLog: "store read failed",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fx := armedFixture(t, true)
			// A pane that would otherwise be reaped outright, so a partial
			// sweep would be visible as a kill.
			fx.addLivePane("boss-aaaaaaaa-22222222", time.Hour, ptr(reaperDaemonID))
			fx.reaper.strikes[strikeKey{name: "boss-aaaaaaaa-22222222", reason: reasonUnaccountedForTwice}] = fx.now.Add(-time.Hour)
			tc.arm(fx)

			n, err := fx.reaper.SweepOnce(context.Background())
			if err == nil {
				t.Fatal("SweepOnce returned nil error, want the read failure surfaced")
			}
			if n != 0 {
				t.Fatalf("SweepOnce reaped %d, want 0", n)
			}
			if len(fx.killed) != 0 {
				t.Fatalf("killed %v, want nothing on a failed read", fx.killed)
			}
			assertLogged(t, fx.logs, tc.inLog)
		})
	}
}

func TestTmuxReaper_NoServerRunningIsNotAFailure(t *testing.T) {
	fx := armedFixture(t, true)
	fx.tmuxFake.listSessionsStderr = "no server running on /tmp/tmux-501/default"

	n, err := fx.reaper.SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("SweepOnce: %v, want nil — an idle host genuinely has zero sessions", err)
	}
	if n != 0 {
		t.Fatalf("reaped %d, want 0", n)
	}
}

// --- dry run ----------------------------------------------------------------

func TestTmuxReaper_DryRunLogsButNeverKills(t *testing.T) {
	fx := newReaperFixture(t, reaperConfig(true, true, false, time.Minute))
	fx.addSession(reaperSessionID, nil)
	fx.addChat(reaperChatID, nil)
	const pane = "boss-aaaaaaaa-11111111"
	fx.addLivePane(pane, time.Hour, ptr(reaperDaemonID))

	fx.sweep()
	fx.now = fx.now.Add(2 * time.Minute)
	if n := fx.sweep(); n != 0 {
		t.Fatalf("dry-run sweep reaped %d, want 0", n)
	}

	// Asserted on the recorded calls, not on the absence of an error.
	if len(fx.killed) != 0 {
		t.Fatalf("dry run invoked the kill seam for %v", fx.killed)
	}
	fx.tmuxFake.mu.Lock()
	killedViaTmux := len(fx.tmuxFake.killed)
	fx.tmuxFake.mu.Unlock()
	if killedViaTmux != 0 {
		t.Fatalf("dry run ran tmux kill-session %d times", killedViaTmux)
	}

	logs := fx.logs.String()
	// Rendered so `go test -v` shows an operator exactly what a dry-run sweep
	// reports before they consider arming the reaper.
	t.Logf("dry-run sweep log:\n%s", logs)
	for _, want := range []string{
		`"tmuxSession":"` + pane + `"`,
		`"bucket":"reap"`,
		`"reason":"unaccountedForTwice"`,
		`"dryRun":true`,
	} {
		if !strings.Contains(logs, want) {
			t.Fatalf("dry-run decision log missing %s\nlogs:\n%s", want, logs)
		}
	}
}

// TestTmuxReaper_SweepSummaryLoggedOnEveryTick pins the summary that an
// operator reads while deciding whether to arm the reaper. A summary only on
// non-zero ticks would make the sweep invisible exactly then.
func TestTmuxReaper_SweepSummaryLoggedOnEveryTick(t *testing.T) {
	fx := armedFixture(t, false)
	fx.addLivePane(reaperChatPane, time.Hour, ptr(reaperDaemonID)) // accounted for
	fx.addLivePane("zsh", time.Hour, nil)                          // a user's own shell

	if n := fx.sweep(); n != 0 {
		t.Fatalf("reaped %d, want 0", n)
	}
	logs := fx.logs.String()
	t.Logf("zero-reap tick log:\n%s", logs)
	for _, want := range []string{
		`"live":2`, `"bossShaped":1`, `"keep":1`,
		`"unattributable":0`, `"candidates":0`, `"reaped":0`, `"dryRun":false`,
		"sweep complete",
	} {
		if !strings.Contains(logs, want) {
			t.Fatalf("zero-reap summary missing %s\nlogs:\n%s", want, logs)
		}
	}
}

// --- leak -------------------------------------------------------------------

func TestTmuxReaper_StrikeMarkersAreGarbageCollected(t *testing.T) {
	fx := armedFixture(t, false)
	const pane = "boss-aaaaaaaa-00000000"
	fx.addLivePane(pane, time.Hour, ptr(reaperDaemonID))

	fx.sweep()
	if fx.reaper.strikeCount() != 1 {
		t.Fatalf("strikeCount = %d, want 1", fx.reaper.strikeCount())
	}

	// The pane goes away by some other route (a manual kill, a reboot).
	fx.tmuxFake.mu.Lock()
	delete(fx.tmuxFake.sessions, pane)
	fx.tmuxFake.mu.Unlock()

	fx.sweep()
	if fx.reaper.strikeCount() != 0 {
		t.Fatalf("strikeCount = %d, want 0: markers for dead names must be pruned", fx.reaper.strikeCount())
	}
}

// --- seam -------------------------------------------------------------------

// TestTmuxReaper_KillSeamIsStubbedInTests is the explicit assertion the plan
// requires: an unstubbed seam would make `make test-bossd` reap the developer's
// own live agent panes.
func TestTmuxReaper_KillSeamIsStubbedInTests(t *testing.T) {
	fx := armedFixture(t, false)
	if fx.reaper.kill == nil {
		t.Fatal("kill seam is nil")
	}

	const pane = "boss-aaaaaaaa-abcdef01"
	fx.addLivePane(pane, time.Hour, ptr(reaperDaemonID))
	if err := fx.reaper.kill(context.Background(), pane); err != nil {
		t.Fatalf("stubbed seam returned %v", err)
	}
	if len(fx.killed) != 1 || fx.killed[0] != pane {
		t.Fatalf("stubbed seam recorded %v, want [%s]", fx.killed, pane)
	}

	fx.tmuxFake.mu.Lock()
	defer fx.tmuxFake.mu.Unlock()
	if len(fx.tmuxFake.killed) != 0 {
		t.Fatalf("the stubbed seam still reached tmux kill-session: %v", fx.tmuxFake.killed)
	}
}

// --- lifecycle --------------------------------------------------------------

// TestTmuxReaper_DisabledNeverSweeps turns BOTH knobs off: since BOS-886 the
// loop starts if either path is armed, so leaving the idle knob on here would
// prove nothing about the orphan knob.
func TestTmuxReaper_DisabledNeverSweeps(t *testing.T) {
	fx := newReaperFixtureWithIdle(t,
		reaperConfig(false, true, false, time.Minute),
		reaperIdleConfig(false, false))
	fx.addLivePane("boss-aaaaaaaa-deadbeef", time.Hour, ptr(reaperDaemonID))

	fx.reaper.Run(context.Background())
	select {
	case <-fx.reaper.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("a disabled reaper's Run did not close Done")
	}
	if len(fx.killed) != 0 {
		t.Fatalf("a disabled reaper killed %v", fx.killed)
	}
}

func TestTmuxReaper_RunStopsOnContextCancel(t *testing.T) {
	fx := newReaperFixture(t, reaperConfig(true, true, false, time.Second))
	ctx, cancel := context.WithCancel(context.Background())
	fx.reaper.Run(ctx)
	cancel()
	select {
	case <-fx.reaper.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not exit after context cancellation")
	}
}

func assertLogged(t *testing.T, buf *bytes.Buffer, want string) {
	t.Helper()
	if got := buf.String(); !strings.Contains(got, want) {
		t.Fatalf("logs missing %q\nlogs:\n%s", want, got)
	}
}
