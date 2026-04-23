package status

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
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

func (f *fakeChatReader) Get(claudeID string) *Entry { return f.entries[claudeID] }

// newTestDB spins up an in-memory SQLite store with migrations applied.
func newTestDB(t *testing.T) (db.SessionStore, db.WorkflowStore, db.ClaudeChatStore, db.RepoStore) {
	t.Helper()
	database, err := db.OpenInMemory()
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := migrate.Run(database, os.DirFS(migrationsDir())); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db.NewSessionStore(database), db.NewWorkflowStore(database), db.NewClaudeChatStore(database), db.NewRepoStore(database)
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
			name:        "chat working wins over PR",
			chat:        pb.ChatStatus_CHAT_STATUS_WORKING,
			display:     &DisplayEntry{Status: vcs.DisplayStatusPassing},
			wantLabel:   "working",
			wantIntent:  pb.DisplayIntent_DISPLAY_INTENT_SUCCESS,
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
			name:        "PR passing",
			display:     &DisplayEntry{Status: vcs.DisplayStatusPassing},
			wantLabel:   "✓ passing",
			wantIntent:  pb.DisplayIntent_DISPLAY_INTENT_SUCCESS,
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
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			sessions, workflows, chats, repos := newTestDB(t)
			repoID := mustRepo(t, repos)
			sessID := mustSession(t, sessions, repoID)

			// Set chat tracker entry keyed by claudeID; bind it to the
			// session by writing the claude_session_id field.
			chatTr := &fakeChatReader{entries: map[string]*Entry{}}
			if tc.chat != pb.ChatStatus_CHAT_STATUS_UNSPECIFIED {
				claudeID := "claude-" + sessID
				chatTr.entries[claudeID] = &Entry{Status: tc.chat, ReceivedAt: time.Now()}
				ptrToClaude := ptr(claudeID)
				if _, err := sessions.Update(context.Background(), sessID, db.UpdateSessionParams{
					ClaudeSessionID: &ptrToClaude,
				}); err != nil {
					t.Fatalf("set claude_session_id: %v", err)
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

func ptr[T any](v T) *T { return &v }

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
