package status

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/migrate"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
	"github.com/rs/zerolog"
)

// migrationsDir resolves the absolute path to the bossd migrations directory.
// Uses runtime.Caller because tests run with cwd set to the package, not the
// repo root.
func migrationsDir() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "..", "..", "migrations")
}

// fakeChatReader returns a preset Entry per claude_id.
type fakeChatReader struct {
	entries map[string]*Entry
}

func (f *fakeChatReader) Get(agentSessionID string) *Entry { return f.entries[agentSessionID] }

// newTestDB spins up an in-memory SQLite store with migrations applied.
func newTestDB(t *testing.T) (db.SessionStore, db.WorkflowStore, db.AgentChatStore, db.RepoStore) {
	t.Helper()
	database, err := db.OpenInMemory()
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := migrate.Run(database, os.DirFS(migrationsDir())); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db.NewSessionStore(database), db.NewWorkflowStore(database), db.NewAgentChatStore(database), db.NewRepoStore(database)
}

func mustRepo(t *testing.T, repos db.RepoStore) string {
	t.Helper()
	r, err := repos.Create(context.Background(), db.CreateRepoParams{
		DisplayName:       "test",
		LocalPath:         "/tmp/test-" + t.Name(),
		OriginURL:         "https://github.com/x/y",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp",
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	return r.ID
}

func mustSession(t *testing.T, sessions db.SessionStore, repoID string) string {
	t.Helper()
	s, err := sessions.Create(context.Background(), db.CreateSessionParams{
		RepoID:       repoID,
		Title:        "t",
		WorktreePath: "/tmp/wt-" + t.Name(),
		BranchName:   "br",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return s.ID
}

// TestRecompute_Matrix exercises the same precedence cascade as
// displaystatus.Compute, but through Recompute's hydration + persistence path.
// It asserts the persisted DisplayLabel matches what Compute would produce
// given the wired inputs.
func TestRecompute_Matrix(t *testing.T) {
	cases := []struct {
		name        string
		display     *DisplayEntry
		chat        pb.ChatStatus
		workflow    *db.CreateWorkflowParams
		wfStatus    models.WorkflowStatus
		wfFlightLeg int
		settingUp   bool
		wantLabel   string
		wantIntent  pb.DisplayIntent
		wantSpinner bool
	}{
		{
			name:        "chat question wins",
			chat:        pb.ChatStatus_CHAT_STATUS_QUESTION,
			display:     &DisplayEntry{Status: vcs.DisplayStatusPassing},
			wantLabel:   "? question",
			wantIntent:  pb.DisplayIntent_DISPLAY_INTENT_WARNING,
			wantSpinner: false,
		},
		{
			name:        "chat waiting wins over PR",
			chat:        pb.ChatStatus_CHAT_STATUS_WAITING,
			display:     &DisplayEntry{Status: vcs.DisplayStatusPassing},
			wantLabel:   "waiting",
			wantIntent:  pb.DisplayIntent_DISPLAY_INTENT_INFO,
			wantSpinner: true,
		},
		{
			name:        "chat working wins over PR",
			chat:        pb.ChatStatus_CHAT_STATUS_WORKING,
			display:     &DisplayEntry{Status: vcs.DisplayStatusPassing},
			wantLabel:   "working",
			wantIntent:  pb.DisplayIntent_DISPLAY_INTENT_SUCCESS,
			wantSpinner: true,
		},
		{
			name:        "chat working over PR conflict keeps working label with danger intent",
			chat:        pb.ChatStatus_CHAT_STATUS_WORKING,
			display:     &DisplayEntry{Status: vcs.DisplayStatusConflict},
			wantLabel:   "working",
			wantIntent:  pb.DisplayIntent_DISPLAY_INTENT_DANGER,
			wantSpinner: true,
		},
		{
			name:        "active running workflow wins over repairing",
			workflow:    &db.CreateWorkflowParams{PlanPath: "/p", MaxLegs: 4},
			wfStatus:    models.WorkflowStatusRunning,
			wfFlightLeg: 2,
			display:     &DisplayEntry{IsRepairing: true},
			wantLabel:   "running 2/4",
			wantIntent:  pb.DisplayIntent_DISPLAY_INTENT_INFO,
			wantSpinner: true,
		},
		{
			name:        "repairing wins over PR passing",
			display:     &DisplayEntry{Status: vcs.DisplayStatusPassing, IsRepairing: true},
			wantLabel:   "repairing",
			wantIntent:  pb.DisplayIntent_DISPLAY_INTENT_WARNING,
			wantSpinner: true,
		},
		{
			name:        "setting up yields initializing label",
			settingUp:   true,
			wantLabel:   "initializing",
			wantIntent:  pb.DisplayIntent_DISPLAY_INTENT_INFO,
			wantSpinner: true,
		},
		{
			name:        "PR passing",
			display:     &DisplayEntry{Status: vcs.DisplayStatusPassing},
			wantLabel:   "✓ passing",
			wantIntent:  pb.DisplayIntent_DISPLAY_INTENT_SUCCESS,
			wantSpinner: false,
		},
		{
			name:        "PR closed",
			display:     &DisplayEntry{Status: vcs.DisplayStatusClosed},
			wantLabel:   "closed",
			wantIntent:  pb.DisplayIntent_DISPLAY_INTENT_MUTED,
			wantSpinner: false,
		},
		{
			name:        "PR draft",
			display:     &DisplayEntry{Status: vcs.DisplayStatusDraft},
			wantLabel:   "draft",
			wantIntent:  pb.DisplayIntent_DISPLAY_INTENT_MUTED,
			wantSpinner: false,
		},
		{
			name:        "PR conflict",
			display:     &DisplayEntry{Status: vcs.DisplayStatusConflict},
			wantLabel:   "⨯ conflict",
			wantIntent:  pb.DisplayIntent_DISPLAY_INTENT_DANGER,
			wantSpinner: false,
		},
		{
			name:        "PR checking with failures bumps intent to danger",
			display:     &DisplayEntry{Status: vcs.DisplayStatusChecking, HasFailures: true},
			wantLabel:   "checking",
			wantIntent:  pb.DisplayIntent_DISPLAY_INTENT_DANGER,
			wantSpinner: true,
		},
		{
			name:       "chat idle falls through PR cascade",
			chat:       pb.ChatStatus_CHAT_STATUS_IDLE,
			wantLabel:  "idle",
			wantIntent: pb.DisplayIntent_DISPLAY_INTENT_WARNING,
		},
		{
			name:       "default stopped",
			chat:       pb.ChatStatus_CHAT_STATUS_STOPPED,
			wantLabel:  "stopped",
			wantIntent: pb.DisplayIntent_DISPLAY_INTENT_MUTED,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sessions, workflows, chats, repos := newTestDB(t)
			repoID := mustRepo(t, repos)
			sessID := mustSession(t, sessions, repoID)

			// Seed a chat row for the session and a tracker entry keyed by
			// the same claude_id. Recompute aggregates across every chat in
			// chats.ListBySession, so the row must exist for the tracker
			// entry to be visible to the precedence cascade.
			chatTr := &fakeChatReader{entries: map[string]*Entry{}}
			if tc.chat != pb.ChatStatus_CHAT_STATUS_UNSPECIFIED {
				agentSessionID := "claude-" + sessID
				chatTr.entries[agentSessionID] = &Entry{Status: tc.chat, ReceivedAt: time.Now()}
				if _, err := chats.Create(context.Background(), db.CreateAgentChatParams{
					SessionID:      sessID,
					AgentSessionID: agentSessionID,
					Title:          "test chat",
				}); err != nil {
					t.Fatalf("create chat: %v", err)
				}
			}

			// Seed display tracker.
			disp := NewDisplayTracker()
			if tc.display != nil {
				if tc.display.Status != 0 {
					disp.Set(sessID, vcs.DisplayInfo{
						Status:      tc.display.Status,
						HasFailures: tc.display.HasFailures,
					})
				}
				if tc.display.IsRepairing {
					disp.SetRepairing(sessID, true)
				}
			}
			if tc.settingUp {
				disp.SetSettingUp(sessID, true)
			}

			// Optionally seed an active workflow.
			if tc.workflow != nil {
				params := *tc.workflow
				params.SessionID = sessID
				params.RepoID = repoID
				w, err := workflows.Create(context.Background(), params)
				if err != nil {
					t.Fatalf("create workflow: %v", err)
				}
				if tc.wfStatus != "" {
					statusStr := string(tc.wfStatus)
					leg := tc.wfFlightLeg
					if _, err := workflows.Update(context.Background(), w.ID, db.UpdateWorkflowParams{
						Status:    &statusStr,
						FlightLeg: &leg,
					}); err != nil {
						t.Fatalf("update workflow: %v", err)
					}
				}
			}

			c := NewDisplayStatusComputer(sessions, disp, chatTr, chats, workflows, zerolog.Nop())
			if err := c.Recompute(context.Background(), sessID); err != nil {
				t.Fatalf("recompute: %v", err)
			}

			got, err := sessions.Get(context.Background(), sessID)
			if err != nil {
				t.Fatalf("get session: %v", err)
			}
			if got.DisplayLabel != tc.wantLabel {
				t.Errorf("DisplayLabel = %q, want %q", got.DisplayLabel, tc.wantLabel)
			}
			if pb.DisplayIntent(got.DisplayIntent) != tc.wantIntent {
				t.Errorf("DisplayIntent = %v, want %v", pb.DisplayIntent(got.DisplayIntent), tc.wantIntent)
			}
			if got.DisplaySpinner != tc.wantSpinner {
				t.Errorf("DisplaySpinner = %v, want %v", got.DisplaySpinner, tc.wantSpinner)
			}
		})
	}
}

func TestRecompute_DraftPRFailureHydratesBlockedReason(t *testing.T) {
	sessions, workflows, chats, repos := newTestDB(t)
	repoID := mustRepo(t, repos)
	sessID := mustSession(t, sessions, repoID)

	reason := "draft PR creation failed: gh pr create: authentication required"
	blockedReason := &reason
	if _, err := sessions.Update(context.Background(), sessID, db.UpdateSessionParams{
		BlockedReason: &blockedReason,
	}); err != nil {
		t.Fatalf("update blocked reason: %v", err)
	}

	c := NewDisplayStatusComputer(
		sessions,
		NewDisplayTracker(),
		&fakeChatReader{entries: map[string]*Entry{}},
		chats,
		workflows,
		zerolog.Nop(),
	)
	if err := c.Recompute(context.Background(), sessID); err != nil {
		t.Fatalf("recompute: %v", err)
	}

	got, err := sessions.Get(context.Background(), sessID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.DisplayLabel != "? PR failed" {
		t.Errorf("DisplayLabel = %q, want %q", got.DisplayLabel, "? PR failed")
	}
	if pb.DisplayIntent(got.DisplayIntent) != pb.DisplayIntent_DISPLAY_INTENT_WARNING {
		t.Errorf("DisplayIntent = %v, want %v", pb.DisplayIntent(got.DisplayIntent), pb.DisplayIntent_DISPLAY_INTENT_WARNING)
	}
	if got.DisplaySpinner {
		t.Errorf("DisplaySpinner = true, want false")
	}
}

// TestRecompute_AggregatesAcrossChats reproduces the bug where a session
// with two chats — one stopped (the original sess.AgentSessionID), one
// freshly created and actively working — was rendering "draft" on the
// session list while the chat picker showed "working" on the live chat.
// Recompute must aggregate across every chat in the session and surface
// the working chat over the PR's draft display status.
func TestRecompute_AggregatesAcrossChats(t *testing.T) {
	sessions, workflows, chats, repos := newTestDB(t)
	repoID := mustRepo(t, repos)
	sessID := mustSession(t, sessions, repoID)

	stoppedClaude := "claude-stopped-" + sessID
	workingClaude := "claude-working-" + sessID

	for _, agentSessionID := range []string{stoppedClaude, workingClaude} {
		if _, err := chats.Create(context.Background(), db.CreateAgentChatParams{
			SessionID:      sessID,
			AgentSessionID: agentSessionID,
			Title:          "chat",
		}); err != nil {
			t.Fatalf("create chat %s: %v", agentSessionID, err)
		}
	}

	chatTr := &fakeChatReader{entries: map[string]*Entry{
		stoppedClaude: {Status: pb.ChatStatus_CHAT_STATUS_STOPPED, ReceivedAt: time.Now()},
		workingClaude: {Status: pb.ChatStatus_CHAT_STATUS_WORKING, ReceivedAt: time.Now()},
	}}

	disp := NewDisplayTracker()
	disp.Set(sessID, vcs.DisplayInfo{Status: vcs.DisplayStatusDraft})

	c := NewDisplayStatusComputer(sessions, disp, chatTr, chats, workflows, zerolog.Nop())
	if err := c.Recompute(context.Background(), sessID); err != nil {
		t.Fatalf("recompute: %v", err)
	}

	got, err := sessions.Get(context.Background(), sessID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.DisplayLabel != "working" {
		t.Errorf("DisplayLabel = %q, want %q (working chat must outrank PR draft)", got.DisplayLabel, "working")
	}
	if !got.DisplaySpinner {
		t.Errorf("DisplaySpinner = false, want true for working status")
	}
}

// TestRecompute_QuestionChatOutranksWorkingSibling pins the precedence
// invariant (QUESTION > WORKING) across sibling chats: when one chat is
// asking a question and another is working, the session label must reflect
// the question regardless of the order ListBySession returns them. The
// aggregation loop relies on QUESTION short-circuiting the scan, so a WORKING
// sibling can never overwrite a QUESTION already chosen.
func TestRecompute_QuestionChatOutranksWorkingSibling(t *testing.T) {
	sessions, workflows, chats, repos := newTestDB(t)
	repoID := mustRepo(t, repos)
	sessID := mustSession(t, sessions, repoID)

	questionClaude := "claude-question-" + sessID
	workingClaude := "claude-working-" + sessID

	for _, agentSessionID := range []string{questionClaude, workingClaude} {
		if _, err := chats.Create(context.Background(), db.CreateAgentChatParams{
			SessionID:      sessID,
			AgentSessionID: agentSessionID,
			Title:          "chat",
		}); err != nil {
			t.Fatalf("create chat %s: %v", agentSessionID, err)
		}
	}

	chatTr := &fakeChatReader{entries: map[string]*Entry{
		questionClaude: {Status: pb.ChatStatus_CHAT_STATUS_QUESTION, ReceivedAt: time.Now()},
		workingClaude:  {Status: pb.ChatStatus_CHAT_STATUS_WORKING, ReceivedAt: time.Now()},
	}}

	disp := NewDisplayTracker()
	disp.Set(sessID, vcs.DisplayInfo{Status: vcs.DisplayStatusDraft})

	c := NewDisplayStatusComputer(sessions, disp, chatTr, chats, workflows, zerolog.Nop())
	if err := c.Recompute(context.Background(), sessID); err != nil {
		t.Fatalf("recompute: %v", err)
	}

	got, err := sessions.Get(context.Background(), sessID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.DisplayLabel != "? question" {
		t.Errorf("DisplayLabel = %q, want %q (question chat must outrank working sibling)", got.DisplayLabel, "? question")
	}
}

// TestRecompute_LimitedChatRanking pins the LIMITED rank: it must beat a
// WORKING sibling (LIMITED > WORKING) but lose to a QUESTION sibling
// (QUESTION > LIMITED), matching Server.GetSessionStatuses.
func TestRecompute_LimitedChatRanking(t *testing.T) {
	tests := []struct {
		name      string
		siblings  map[string]pb.ChatStatus
		wantLabel string
	}{
		{
			name: "limited beats working",
			siblings: map[string]pb.ChatStatus{
				"working": pb.ChatStatus_CHAT_STATUS_WORKING,
				"limited": pb.ChatStatus_CHAT_STATUS_LIMITED,
			},
			wantLabel: "usage-limited",
		},
		{
			name: "question beats limited",
			siblings: map[string]pb.ChatStatus{
				"question": pb.ChatStatus_CHAT_STATUS_QUESTION,
				"limited":  pb.ChatStatus_CHAT_STATUS_LIMITED,
			},
			wantLabel: "? question",
		},
		{
			name: "limited beats idle",
			siblings: map[string]pb.ChatStatus{
				"idle":    pb.ChatStatus_CHAT_STATUS_IDLE,
				"limited": pb.ChatStatus_CHAT_STATUS_LIMITED,
			},
			wantLabel: "usage-limited",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sessions, workflows, chats, repos := newTestDB(t)
			repoID := mustRepo(t, repos)
			sessID := mustSession(t, sessions, repoID)

			entries := map[string]*Entry{}
			for suffix, status := range tc.siblings {
				agentSessionID := "claude-" + suffix + "-" + sessID
				if _, err := chats.Create(context.Background(), db.CreateAgentChatParams{
					SessionID:      sessID,
					AgentSessionID: agentSessionID,
					Title:          "chat",
				}); err != nil {
					t.Fatalf("create chat %s: %v", agentSessionID, err)
				}
				entries[agentSessionID] = &Entry{Status: status, ReceivedAt: time.Now()}
			}

			chatTr := &fakeChatReader{entries: entries}
			disp := NewDisplayTracker()
			disp.Set(sessID, vcs.DisplayInfo{Status: vcs.DisplayStatusDraft})

			c := NewDisplayStatusComputer(sessions, disp, chatTr, chats, workflows, zerolog.Nop())
			if err := c.Recompute(context.Background(), sessID); err != nil {
				t.Fatalf("recompute: %v", err)
			}

			got, err := sessions.Get(context.Background(), sessID)
			if err != nil {
				t.Fatalf("get session: %v", err)
			}
			if got.DisplayLabel != tc.wantLabel {
				t.Errorf("DisplayLabel = %q, want %q", got.DisplayLabel, tc.wantLabel)
			}
		})
	}
}

// TestRecompute_LimitedChatComposesResetTime pins that a limited chat carrying
// a non-zero ResetAt renders the composed "usage-limited (resets ~HH:MM)" label.
func TestRecompute_LimitedChatComposesResetTime(t *testing.T) {
	sessions, workflows, chats, repos := newTestDB(t)
	repoID := mustRepo(t, repos)
	sessID := mustSession(t, sessions, repoID)

	agentSessionID := "claude-limited-" + sessID
	if _, err := chats.Create(context.Background(), db.CreateAgentChatParams{
		SessionID:      sessID,
		AgentSessionID: agentSessionID,
		Title:          "chat",
	}); err != nil {
		t.Fatalf("create chat: %v", err)
	}

	resetAt := time.Date(2026, 1, 2, 15, 0, 0, 0, time.UTC)
	chatTr := &fakeChatReader{entries: map[string]*Entry{
		agentSessionID: {Status: pb.ChatStatus_CHAT_STATUS_LIMITED, ResetAt: resetAt, ReceivedAt: time.Now()},
	}}
	disp := NewDisplayTracker()
	disp.Set(sessID, vcs.DisplayInfo{Status: vcs.DisplayStatusDraft})

	c := NewDisplayStatusComputer(sessions, disp, chatTr, chats, workflows, zerolog.Nop())
	if err := c.Recompute(context.Background(), sessID); err != nil {
		t.Fatalf("recompute: %v", err)
	}

	got, err := sessions.Get(context.Background(), sessID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if !strings.Contains(got.DisplayLabel, "usage-limited (resets ~") {
		t.Errorf("DisplayLabel = %q, want it to contain %q", got.DisplayLabel, "usage-limited (resets ~")
	}
}

// TestRecompute_Idempotent verifies that calling Recompute twice in a row
// with no input changes results in exactly one DB UPDATE.
func TestRecompute_Idempotent(t *testing.T) {
	sessions, workflows, chats, repos := newTestDB(t)
	repoID := mustRepo(t, repos)
	sessID := mustSession(t, sessions, repoID)

	disp := NewDisplayTracker()
	chatTr := &fakeChatReader{entries: map[string]*Entry{}}

	// Wrap the session store to count Update calls.
	counted := &countingSessionStore{SessionStore: sessions}
	c := NewDisplayStatusComputer(counted, disp, chatTr, chats, workflows, zerolog.Nop())

	// First call should write (DisplayLabel was empty → "stopped").
	if err := c.Recompute(context.Background(), sessID); err != nil {
		t.Fatalf("recompute 1: %v", err)
	}
	if got := atomic.LoadInt64(&counted.updates); got != 1 {
		t.Errorf("after first Recompute: updates = %d, want 1", got)
	}

	// Second call with no input changes should be a no-op.
	if err := c.Recompute(context.Background(), sessID); err != nil {
		t.Fatalf("recompute 2: %v", err)
	}
	if got := atomic.LoadInt64(&counted.updates); got != 1 {
		t.Errorf("after second Recompute: updates = %d, want still 1", got)
	}
}

// TestDisplayTracker_TriggersRecompute asserts that wiring a Recomputer into
// a DisplayTracker causes Set/SetRepairing/Remove to invoke it.
func TestDisplayTracker_TriggersRecompute(t *testing.T) {
	calls := &recordingRecomputer{}
	tr := NewDisplayTracker()
	tr.SetRecomputer(calls)

	tr.Set("s1", vcs.DisplayInfo{Status: vcs.DisplayStatusPassing})
	tr.SetRepairing("s2", true)
	tr.Remove("s1")

	got := calls.snapshot()
	want := []string{"s1", "s2", "s1"}
	if len(got) != len(want) {
		t.Fatalf("recompute calls = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("call[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestChatTracker_TriggersOnUpdate asserts the Tracker hook fires only when
// the chat status changes (not on every heartbeat).
func TestChatTracker_TriggersOnUpdate(t *testing.T) {
	tr := NewTracker()
	var calls atomic.Int32
	tr.SetOnUpdate(func(string) { calls.Add(1) })

	now := time.Now()
	tr.Update("c1", pb.ChatStatus_CHAT_STATUS_WORKING, now)
	tr.Update("c1", pb.ChatStatus_CHAT_STATUS_WORKING, now) // no-op
	tr.Update("c1", pb.ChatStatus_CHAT_STATUS_IDLE, now)    // change

	if got := calls.Load(); got != 2 {
		t.Errorf("hook fired %d times, want 2 (initial + change)", got)
	}
}

// --- helpers ---

// countingSessionStore wraps a SessionStore and tallies Update calls.
type countingSessionStore struct {
	db.SessionStore
	updates int64
}

func (c *countingSessionStore) Update(ctx context.Context, id string, params db.UpdateSessionParams) (*models.Session, error) {
	atomic.AddInt64(&c.updates, 1)
	return c.SessionStore.Update(ctx, id, params)
}

// recordingRecomputer records the session IDs Recompute was invoked with.
type recordingRecomputer struct {
	mu  sync.Mutex
	ids []string
}

func (r *recordingRecomputer) Recompute(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, id)
	return nil
}

func (r *recordingRecomputer) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.ids))
	copy(out, r.ids)
	return out
}

// TestClampInt32 is the BOS-413 boundary table-test for the package-local
// gosec-G115 clamp helper: normal values pass through; out-of-range int inputs
// clamp to the int32 extremes instead of wrapping. int32 range is
// [-2147483648, 2147483647].
func TestClampInt32(t *testing.T) {
	const (
		maxI32 = 2147483647
		minI32 = -2147483648
	)
	tests := []struct {
		name string
		in   int
		want int32
	}{
		{"normal", 42, 42},
		{"zero", 0, 0},
		{"max", maxI32, maxI32},
		{"min", minI32, minI32},
		{"clampsHigh", maxI32 + 1, maxI32},
		{"clampsLow", minI32 - 1, minI32},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clampInt32(tt.in); got != tt.want {
				t.Errorf("clampInt32(%d) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

// --- BOS-1096: a silent chat settles the session label -----------------------

// evictionSettleHarness wires a REAL Tracker into a REAL DisplayStatusComputer
// over a real (in-memory) database, so these tests exercise the composition
// cascade rather than a fake's idea of it. That matters here: the whole premise
// of the fix is that Get already returns nil past StaleThreshold, so an evicted
// chat folds out of the aggregate and the cascade reaches the right answer
// unaided — only the recompute was missing. If that premise were false, the
// first scenario below fails.
type evictionSettleHarness struct {
	t         *testing.T
	sessions  db.SessionStore
	chats     db.AgentChatStore
	disp      *DisplayTracker
	tracker   *Tracker
	computer  *DisplayStatusComputer
	sessionID string
}

func newEvictionSettleHarness(t *testing.T) *evictionSettleHarness {
	t.Helper()
	sessions, workflows, chats, repos := newTestDB(t)
	repoID := mustRepo(t, repos)
	sessID := mustSession(t, sessions, repoID)

	disp := NewDisplayTracker()
	tracker := NewTracker()
	computer := NewDisplayStatusComputer(sessions, disp, tracker, chats, workflows, zerolog.Nop())

	// Stands in for cmd's wireEvictionRecompute, which lives in package main and
	// is therefore out of reach from here — this unit has to live in the status
	// package because backdating entries requires in-package access. Same shape:
	// resolve each evicted id to its session, dedupe, recompute once each. The
	// daemon really does install the real one; TestEvictionRecomputeSeamsWired
	// is what proves that.
	tracker.SetOnEntriesEvicted(func(agentSessionIDs []string) {
		seen := map[string]struct{}{}
		for _, id := range agentSessionIDs {
			chat, err := chats.GetByAgentSessionID(context.Background(), id)
			if err != nil || chat == nil {
				continue
			}
			if _, dup := seen[chat.SessionID]; dup {
				continue
			}
			seen[chat.SessionID] = struct{}{}
			_ = computer.Recompute(context.Background(), chat.SessionID)
		}
	})

	return &evictionSettleHarness{
		t: t, sessions: sessions, chats: chats,
		disp: disp, tracker: tracker, computer: computer, sessionID: sessID,
	}
}

// reportChat seeds a chat row and a live tracker heartbeat for it.
func (h *evictionSettleHarness) reportChat(agentSessionID string, st pb.ChatStatus) {
	h.t.Helper()
	if _, err := h.chats.Create(context.Background(), db.CreateAgentChatParams{
		SessionID:      h.sessionID,
		AgentSessionID: agentSessionID,
		Title:          "test chat",
	}); err != nil {
		h.t.Fatalf("create chat %s: %v", agentSessionID, err)
	}
	h.tracker.Update(agentSessionID, st, time.Now())
}

// goSilent backdates a chat's heartbeat past StaleThreshold — the producer
// stopping without ever sending a terminal STOPPED, which is the reported bug.
func (h *evictionSettleHarness) goSilent(agentSessionID string) {
	h.t.Helper()
	h.tracker.mu.Lock()
	defer h.tracker.mu.Unlock()
	e, ok := h.tracker.entries[agentSessionID]
	if !ok {
		h.t.Fatalf("goSilent: no entry %q", agentSessionID)
	}
	e.ReceivedAt = time.Now().Add(-StaleThreshold - time.Second)
}

func (h *evictionSettleHarness) recompute() {
	h.t.Helper()
	if err := h.computer.Recompute(context.Background(), h.sessionID); err != nil {
		h.t.Fatalf("recompute: %v", err)
	}
}

func (h *evictionSettleHarness) label() string {
	h.t.Helper()
	got, err := h.sessions.Get(context.Background(), h.sessionID)
	if err != nil {
		h.t.Fatalf("get session: %v", err)
	}
	return got.DisplayLabel
}

// The reported symptom, at the level the user experiences it: a session whose
// only chat stops heartbeating settles to stopped on the cleanup tick, with no
// daemon restart. The freeze is not confined to idle — any label the last edge
// produced survives the same way, which is why every starting status is
// covered. question matters most: a dead session otherwise goes on claiming a
// human is needed.
func TestEviction_SettlesFrozenSessionLabel(t *testing.T) {
	for _, tc := range []struct {
		name      string
		start     pb.ChatStatus
		wantFirst string
	}{
		{"idle", pb.ChatStatus_CHAT_STATUS_IDLE, "idle"},
		{"working", pb.ChatStatus_CHAT_STATUS_WORKING, "working"},
		{"question", pb.ChatStatus_CHAT_STATUS_QUESTION, "? question"},
		// Listed for completeness rather than as new coverage: a waiting chat
		// holds a tracker waiting marker, which Cleanup already clears through
		// onWaitingChange's nil-entry branch.
		{"waiting", pb.ChatStatus_CHAT_STATUS_WAITING, "waiting"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newEvictionSettleHarness(t)
			h.reportChat("agent-1", tc.start)
			h.recompute()

			if got := h.label(); got != tc.wantFirst {
				t.Fatalf("label with a live chat = %q, want %q", got, tc.wantFirst)
			}

			// The producer goes quiet without a terminal STOPPED. No further
			// recompute is triggered by hand — the eviction is the only edge.
			h.goSilent("agent-1")
			h.tracker.Cleanup()

			if got := h.label(); got != "stopped" {
				t.Fatalf("label after eviction = %q, want %q (label stayed frozen)", got, "stopped")
			}
		})
	}
}

// The eviction must not overwrite a chat-status-independent branch of the
// cascade. A session whose label comes from its PR keeps that label when a chat
// is evicted — idle and stopped both fall THROUGH to the PR cascade, so the
// recompute reaches the same answer it did before.
// AC3: deleting a session's last chat settles its label the same way staleness
// does. Remove is the other site that deletes from entries, and it is reached by
// DeleteChat -- which calls it while the chat row is still present, so the
// eviction hook's lookup resolves and the recompute lands. The ordering that
// makes that true is pinned separately by
// TestDeleteChat_RecomputesWhileTheChatRowIsStillPresent in internal/server;
// this is the composition half, asserting the label actually settles.
func TestEviction_SettlesLabelWhenLastChatRemoved(t *testing.T) {
	h := newEvictionSettleHarness(t)
	h.reportChat("agent-1", pb.ChatStatus_CHAT_STATUS_WORKING)
	h.recompute()

	if got := h.label(); got != "working" {
		t.Fatalf("label with a live chat = %q, want %q", got, "working")
	}

	// The explicit-deletion edge, with no hand-rolled recompute after it.
	h.tracker.Remove("agent-1")

	if got := h.label(); got != "stopped" {
		t.Fatalf("label after removing the last chat = %q, want %q (label stayed frozen)", got, "stopped")
	}
}

func TestEviction_KeepsPRDerivedLabel(t *testing.T) {
	h := newEvictionSettleHarness(t)
	h.disp.Set(h.sessionID, vcs.DisplayInfo{Status: vcs.DisplayStatusPassing})
	h.reportChat("agent-1", pb.ChatStatus_CHAT_STATUS_IDLE)
	h.recompute()

	if got := h.label(); got != "✓ passing" {
		t.Fatalf("label with a live idle chat = %q, want %q", got, "✓ passing")
	}

	h.goSilent("agent-1")
	h.tracker.Cleanup()

	if got := h.label(); got != "✓ passing" {
		t.Fatalf("label after eviction = %q, want %q (eviction clobbered a PR-derived label)", got, "✓ passing")
	}
}

// Evicting one chat must not report the whole session dead: the aggregate is
// over every chat in the session, and a still-live one still wins.
func TestEviction_KeepsLabelFromRemainingLiveChat(t *testing.T) {
	h := newEvictionSettleHarness(t)
	h.reportChat("agent-dead", pb.ChatStatus_CHAT_STATUS_IDLE)
	h.reportChat("agent-live", pb.ChatStatus_CHAT_STATUS_WORKING)
	h.recompute()

	if got := h.label(); got != "working" {
		t.Fatalf("label with two live chats = %q, want %q", got, "working")
	}

	// Only one goes silent; the other keeps heartbeating.
	h.goSilent("agent-dead")
	h.tracker.Cleanup()

	if got := h.label(); got != "working" {
		t.Fatalf("label after one eviction = %q, want %q (a live chat was reported dead)", got, "working")
	}
}
