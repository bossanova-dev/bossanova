package taskorchestrator

import (
	"context"
	"fmt"
	"testing"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/claude"
	"github.com/recurser/bossd/internal/db"
)

// --- mock session store for liveness tests ---

type mockSessionStoreLiveness struct {
	sessions map[string]*models.Session
}

func (m *mockSessionStoreLiveness) Create(_ context.Context, _ db.CreateSessionParams) (*models.Session, error) {
	return nil, nil
}
func (m *mockSessionStoreLiveness) Get(_ context.Context, id string) (*models.Session, error) {
	if s, ok := m.sessions[id]; ok {
		return s, nil
	}
	return nil, fmt.Errorf("session not found")
}
func (m *mockSessionStoreLiveness) List(_ context.Context, _ string) ([]*models.Session, error) {
	return nil, nil
}
func (m *mockSessionStoreLiveness) ListActive(_ context.Context, _ string) ([]*models.Session, error) {
	return nil, nil
}
func (m *mockSessionStoreLiveness) ListActiveWithRepo(_ context.Context, _ string) ([]*db.SessionWithRepo, error) {
	return nil, nil
}
func (m *mockSessionStoreLiveness) ListWithRepo(_ context.Context, _ string) ([]*db.SessionWithRepo, error) {
	return nil, nil
}
func (m *mockSessionStoreLiveness) ListArchived(_ context.Context, _ string) ([]*models.Session, error) {
	return nil, nil
}
func (m *mockSessionStoreLiveness) Update(_ context.Context, _ string, _ db.UpdateSessionParams) (*models.Session, error) {
	return nil, nil
}
func (m *mockSessionStoreLiveness) Archive(_ context.Context, _ string) error { return nil }
func (m *mockSessionStoreLiveness) Resurrect(_ context.Context, _ string) error {
	return nil
}
func (m *mockSessionStoreLiveness) Delete(_ context.Context, _ string) error { return nil }
func (m *mockSessionStoreLiveness) AdvanceOrphanedSessions(_ context.Context) (int64, error) {
	return 0, nil
}

// --- mock claude runner for liveness tests ---

type mockClaudeRunnerLiveness struct {
	running map[string]bool
}

func (m *mockClaudeRunnerLiveness) Start(_ context.Context, _, _ string, _ *string, _ string) (string, error) {
	return "", nil
}
func (m *mockClaudeRunnerLiveness) Stop(_ string) error { return nil }
func (m *mockClaudeRunnerLiveness) IsRunning(sessionID string) bool {
	return m.running[sessionID]
}
func (m *mockClaudeRunnerLiveness) ExitError(_ string) error { return nil }
func (m *mockClaudeRunnerLiveness) Subscribe(_ context.Context, _ string) (<-chan claude.OutputLine, error) {
	return nil, nil
}
func (m *mockClaudeRunnerLiveness) History(_ string) []claude.OutputLine { return nil }

// --- tests ---

func TestLivenessChecker_SessionNotFound(t *testing.T) {
	checker := &defaultLivenessChecker{
		sessions: &mockSessionStoreLiveness{sessions: map[string]*models.Session{}},
		claude:   &mockClaudeRunnerLiveness{running: map[string]bool{}},
	}

	if checker.IsSessionAlive(context.Background(), "nonexistent") {
		t.Error("expected false when session not found")
	}
}

func TestLivenessChecker_SessionPastImplementingPlan(t *testing.T) {
	// All states beyond ImplementingPlan should be considered alive,
	// including PushingBranch and OpeningDraftPR which have lower
	// numeric values than ImplementingPlan in the iota ordering.
	aliveStates := []struct {
		name  string
		state machine.State
	}{
		{"PushingBranch", machine.PushingBranch},
		{"OpeningDraftPR", machine.OpeningDraftPR},
		{"AwaitingChecks", machine.AwaitingChecks},
		{"FixingChecks", machine.FixingChecks},
		{"GreenDraft", machine.GreenDraft},
		{"ReadyForReview", machine.ReadyForReview},
		{"Blocked", machine.Blocked},
		{"Merged", machine.Merged},
		{"Closed", machine.Closed},
	}

	for _, tt := range aliveStates {
		t.Run(tt.name, func(t *testing.T) {
			checker := &defaultLivenessChecker{
				sessions: &mockSessionStoreLiveness{
					sessions: map[string]*models.Session{
						"sess-1": {ID: "sess-1", State: tt.state},
					},
				},
				claude: &mockClaudeRunnerLiveness{running: map[string]bool{}},
			}

			if !checker.IsSessionAlive(context.Background(), "sess-1") {
				t.Errorf("expected true when session is in %s state", tt.name)
			}
		})
	}
}

func TestLivenessChecker_NoProcessIdentifiers(t *testing.T) {
	// When neither ClaudeSessionID nor TmuxSessionName is set, the session
	// is still initializing (e.g. quick chat waiting for first attach).
	checker := &defaultLivenessChecker{
		sessions: &mockSessionStoreLiveness{
			sessions: map[string]*models.Session{
				"sess-2": {ID: "sess-2", State: machine.ImplementingPlan, ClaudeSessionID: nil, TmuxSessionName: nil},
			},
		},
		claude: &mockClaudeRunnerLiveness{running: map[string]bool{}},
	}

	if !checker.IsSessionAlive(context.Background(), "sess-2") {
		t.Error("expected true when session has no process identifiers (still initializing)")
	}
}

func TestLivenessChecker_ClaudeDead(t *testing.T) {
	claudeID := "claude-123"
	checker := &defaultLivenessChecker{
		sessions: &mockSessionStoreLiveness{
			sessions: map[string]*models.Session{
				"sess-3": {ID: "sess-3", State: machine.ImplementingPlan, ClaudeSessionID: &claudeID},
			},
		},
		claude: &mockClaudeRunnerLiveness{running: map[string]bool{"claude-123": false}},
	}

	if checker.IsSessionAlive(context.Background(), "sess-3") {
		t.Error("expected false when Claude process is dead")
	}
}

func TestLivenessChecker_ClaudeRunning(t *testing.T) {
	claudeID := "claude-456"
	checker := &defaultLivenessChecker{
		sessions: &mockSessionStoreLiveness{
			sessions: map[string]*models.Session{
				"sess-4": {ID: "sess-4", State: machine.ImplementingPlan, ClaudeSessionID: &claudeID},
			},
		},
		claude: &mockClaudeRunnerLiveness{running: map[string]bool{"claude-456": true}},
	}

	if !checker.IsSessionAlive(context.Background(), "sess-4") {
		t.Error("expected true when Claude process is running")
	}
}
