package taskorchestrator

import (
	"context"
	"os/exec"
	"sync"
	"testing"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/tmux"
)

// --- minimal tmux + chat doubles for the parked-verdict tests ---
//
// internal/testharness depends on this package, so its shared tmux fake cannot
// be imported here without an import cycle. These are the two arms the liveness
// ladder actually exercises.

type parkedTmuxFake struct {
	mu   sync.Mutex
	live map[string]bool
}

func (f *parkedTmuxFake) factory(ctx context.Context, name string, args ...string) *exec.Cmd {
	if name != "tmux" || len(args) == 0 || args[0] != "has-session" {
		return exec.CommandContext(ctx, "true")
	}
	var target string
	for i, a := range args[1:] {
		if a == "-t" && i+2 <= len(args[1:]) {
			target = args[1:][i+1]
			break
		}
	}
	f.mu.Lock()
	alive := f.live[target]
	f.mu.Unlock()
	if alive {
		return exec.CommandContext(ctx, "true")
	}
	// Mirror real tmux: a missing session is a clean "not alive", not an error.
	// #nosec G204 -- test-only fake; message passed as $1 to `sh -c`, no shell interpolation
	// owner=@recurser review-by=2027-01-18 issue=BOS-884
	return exec.CommandContext(ctx, "sh", "-c",
		`printf '%s\n' "$1" >&2; exit 1`, "sh", "can't find session")
}

// parkedChatStore is a read-only db.AgentChatStore serving a fixed chat list.
type parkedChatStore struct {
	bySession map[string][]*models.AgentChat
}

func (s *parkedChatStore) ListBySession(_ context.Context, sessionID string) ([]*models.AgentChat, error) {
	return s.bySession[sessionID], nil
}

func (s *parkedChatStore) Create(_ context.Context, _ db.CreateAgentChatParams) (*models.AgentChat, error) {
	return nil, nil
}

func (s *parkedChatStore) GetByAgentSessionID(_ context.Context, _ string) (*models.AgentChat, error) {
	return nil, db.ErrAgentChatNotFound
}

func (s *parkedChatStore) ListBySessions(_ context.Context, _ []string) (map[string][]*models.AgentChat, error) {
	return nil, nil
}
func (s *parkedChatStore) UpdateTitle(_ context.Context, _, _ string) error { return nil }
func (s *parkedChatStore) UpdateTitleByAgentSessionID(_ context.Context, _, _ string) error {
	return nil
}
func (s *parkedChatStore) UpdateAgentSessionID(_ context.Context, _, _, _ string) error { return nil }
func (s *parkedChatStore) UpdateTmuxSessionName(_ context.Context, _ string, _ *string) error {
	return nil
}
func (s *parkedChatStore) UpdateProviderSessionID(_ context.Context, _ string, _ *string) error {
	return nil
}
func (s *parkedChatStore) UpdateAccountIDByAgentSessionID(_ context.Context, _ string, _ *string) error {
	return nil
}
func (s *parkedChatStore) MarkStartFailed(_ context.Context, _, _ string) error     { return nil }
func (s *parkedChatStore) DeleteByAgentSessionID(_ context.Context, _ string) error { return nil }
func (s *parkedChatStore) ListWithTmuxSession(_ context.Context) ([]*models.AgentChat, error) {
	return nil, nil
}
func (s *parkedChatStore) ListRoutableChats(_ context.Context) ([]*models.AgentChat, error) {
	return nil, nil
}

// TestLivenessChecker_ClearedPanePointer pins BOS-884: a chat row that outlives
// its tmux pointer is the durable trace of a DELIBERATE teardown, so the
// session is parked, never dead. A row whose pointer is still set and whose
// pane is gone is the genuine-death case and must stay dead.
func TestLivenessChecker_ClearedPanePointer(t *testing.T) {
	paneName := "boss-parked-1"

	states := []struct {
		name  string
		state machine.State
	}{
		{"CreatingWorktree", machine.CreatingWorktree},
		{"StartingAgent", machine.StartingAgent},
		{"ImplementingPlan", machine.ImplementingPlan},
	}

	cases := []struct {
		name     string
		pointer  *string
		paneLive bool
		want     session.Liveness
	}{
		{
			name:    "cleared pointer is parked",
			pointer: nil,
			want:    session.LivenessParked,
		},
		{
			name:    "empty pointer is parked",
			pointer: ptrString(""),
			want:    session.LivenessParked,
		},
		{
			name:    "pointer set and pane gone is dead",
			pointer: &paneName,
			want:    session.LivenessDead,
		},
		{
			name:     "pointer set and pane live is alive",
			pointer:  &paneName,
			paneLive: true,
			want:     session.LivenessAlive,
		},
	}

	for _, st := range states {
		for _, tc := range cases {
			t.Run(st.name+"/"+tc.name, func(t *testing.T) {
				fake := &parkedTmuxFake{live: map[string]bool{}}
				if tc.paneLive {
					fake.live[paneName] = true
				}
				checker := &defaultLivenessChecker{
					sessions: &mockSessionStoreLiveness{
						sessions: map[string]*models.Session{
							"sess-parked": {ID: "sess-parked", State: st.state},
						},
					},
					chats: &parkedChatStore{
						bySession: map[string][]*models.AgentChat{
							"sess-parked": {{
								ID:              "chat-1",
								SessionID:       "sess-parked",
								AgentSessionID:  "run-1",
								TmuxSessionName: tc.pointer,
							}},
						},
					},
					agentForSession: constAgent(&mockAgentRunnerLiveness{running: map[string]bool{}}),
					tmux:            tmux.NewClient(tmux.WithCommandFactory(fake.factory)),
				}

				if got := checker.SessionLiveness(context.Background(), "sess-parked"); got != tc.want {
					t.Errorf("SessionLiveness = %s, want %s", got, tc.want)
				}
			})
		}
	}
}

func ptrString(s string) *string { return &s }

// TestRecoverStaleTasks_ParkedSession_LeavesTaskInProgress pins the recovery
// half of BOS-884: a deliberately reaped session must not have its task failed
// or its active mapping released.
func TestRecoverStaleTasks_ParkedSession_LeavesTaskInProgress(t *testing.T) {
	sessionID := "sess-parked"
	mappingID := "tm-parked"

	mapping := &models.TaskMapping{
		ID:        mappingID,
		RepoID:    "r1",
		Status:    models.TaskMappingStatusInProgress,
		SessionID: &sessionID,
	}

	store := &mockTaskMappingStore{
		mappings: map[string]*models.TaskMapping{},
		byID:     map[string]*models.TaskMapping{mappingID: mapping},
		bySession: map[string]*models.TaskMapping{
			sessionID: mapping,
		},
		updateFn: func(_ context.Context, _ string, params db.UpdateTaskMappingParams) (*models.TaskMapping, error) {
			if params.Status != nil {
				mapping.Status = *params.Status
			}
			return mapping, nil
		},
	}

	var checked string
	checker := &mockLivenessChecker{
		livenessFn: func(_ context.Context, sid string) session.Liveness {
			checked = sid
			return session.LivenessParked
		},
	}

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.taskMappings = store
		o.livenessChecker = checker
	})

	orch.mu.Lock()
	orch.active["r1"] = true
	orch.activeMapping["r1"] = mappingID
	orch.mu.Unlock()

	orch.recoverStaleTasks(context.Background())

	if checked != sessionID {
		t.Errorf("liveness checked for %q, want %q", checked, sessionID)
	}
	if mapping.Status != models.TaskMappingStatusInProgress {
		t.Errorf("mapping status = %v, want in_progress (a parked session is not a failure)", mapping.Status)
	}
	orch.mu.Lock()
	_, hasActive := orch.activeMapping["r1"]
	orch.mu.Unlock()
	if !hasActive {
		t.Error("expected activeMapping to remain for a parked session")
	}
}

// TestIsDuplicateSessionAlive_ParkedKeepsGuard pins the other BOS-884 call
// site: the duplicate-PR/branch guard is released only for a definitively dead
// session, never for one whose pane was reaped on purpose.
func TestIsDuplicateSessionAlive_ParkedKeepsGuard(t *testing.T) {
	tests := []struct {
		name    string
		checker SessionLivenessChecker
		want    bool
	}{
		{
			name:    "unwired checker keeps the guard",
			checker: nil,
			want:    true,
		},
		{
			name:    "alive keeps the guard",
			checker: &mockDuplicateLiveness{alive: map[string]bool{"sess-dup": true}},
			want:    true,
		},
		{
			name:    "parked keeps the guard",
			checker: &mockDuplicateLiveness{parked: map[string]bool{"sess-dup": true}},
			want:    true,
		},
		{
			name:    "dead releases the guard",
			checker: &mockDuplicateLiveness{},
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &lifecycleSessionCreator{duplicateLiveness: tt.checker}
			if got := c.isDuplicateSessionAlive(context.Background(), "sess-dup"); got != tt.want {
				t.Errorf("isDuplicateSessionAlive = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestLivenessChecker_PanelessRowsStayDead pins the converse of BOS-884: a chat
// row that never had a pane is not evidence of a deliberate teardown, so it must
// not park its session. Two rows are paneless by construction — the headless
// primary row (codex exec / claude --print never reach StartTmuxChat, the only
// place a pointer is stamped) and a failed-start row (recordFailedStartChat).
// Parking either one would make the session permanently un-reapable and silently
// disable sweepOrphanedHeadlessRuns, the cron/stranded completion evidence,
// recoverStaleTasks and the duplicate-PR/branch guard release.
func TestLivenessChecker_PanelessRowsStayDead(t *testing.T) {
	startErr := "spawn failed"

	cases := []struct {
		name string
		sess *models.Session
		chat *models.AgentChat
		want session.Liveness
	}{
		{
			name: "headless primary row is dead, not parked",
			sess: &models.Session{
				ID:             "sess-x",
				State:          machine.ImplementingPlan,
				AgentSessionID: ptrString("run-1"),
			},
			chat: &models.AgentChat{ID: "chat-1", SessionID: "sess-x", AgentSessionID: "run-1"},
			want: session.LivenessDead,
		},
		{
			name: "failed-start row is dead, not parked",
			sess: &models.Session{
				ID:               "sess-x",
				State:            machine.ImplementingPlan,
				AgentSessionID:   ptrString("run-1"),
				IsTmuxUnattended: true,
			},
			chat: &models.AgentChat{
				ID: "chat-1", SessionID: "sess-x", AgentSessionID: "run-1", StartError: &startErr,
			},
			want: session.LivenessDead,
		},
		{
			name: "reaped pane on an unattended session is still parked",
			sess: &models.Session{
				ID:               "sess-x",
				State:            machine.ImplementingPlan,
				AgentSessionID:   ptrString("run-1"),
				IsTmuxUnattended: true,
			},
			chat: &models.AgentChat{ID: "chat-1", SessionID: "sess-x", AgentSessionID: "run-1"},
			want: session.LivenessParked,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &parkedTmuxFake{live: map[string]bool{}}
			checker := &defaultLivenessChecker{
				sessions: &mockSessionStoreLiveness{
					sessions: map[string]*models.Session{"sess-x": tc.sess},
				},
				chats: &parkedChatStore{
					bySession: map[string][]*models.AgentChat{"sess-x": {tc.chat}},
				},
				agentForSession: constAgent(&mockAgentRunnerLiveness{running: map[string]bool{}}),
				tmux:            tmux.NewClient(tmux.WithCommandFactory(fake.factory)),
			}

			if got := checker.SessionLiveness(context.Background(), "sess-x"); got != tc.want {
				t.Errorf("SessionLiveness = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestLivenessChecker_ReapedSiblingDoesNotMaskDeadChat pins that a session's
// chats are torn down independently: one chat's reap says nothing about a
// sibling that still holds its pointer and has lost its pane. That sibling is
// positive evidence of death and must outrank the reap, or a single stale
// reaped row keeps the session out of stale-task recovery and the
// duplicate-PR/branch guard release for good.
func TestLivenessChecker_ReapedSiblingDoesNotMaskDeadChat(t *testing.T) {
	livePane, deadPane := "boss-live-1", "boss-dead-1"

	cases := []struct {
		name     string
		sibling  *models.AgentChat
		livePane bool
		want     session.Liveness
	}{
		{
			name:    "reaped chat plus a dead pointed chat is dead",
			sibling: &models.AgentChat{ID: "chat-2", SessionID: "sess-m", AgentSessionID: "run-2", TmuxSessionName: &deadPane},
			want:    session.LivenessDead,
		},
		{
			name:     "reaped chat plus a live chat is alive",
			sibling:  &models.AgentChat{ID: "chat-2", SessionID: "sess-m", AgentSessionID: "run-2", TmuxSessionName: &livePane},
			livePane: true,
			want:     session.LivenessAlive,
		},
		{
			name:    "reaped chat alone is parked",
			sibling: nil,
			want:    session.LivenessParked,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &parkedTmuxFake{live: map[string]bool{}}
			if tc.livePane {
				fake.live[livePane] = true
			}
			chats := []*models.AgentChat{
				{ID: "chat-1", SessionID: "sess-m", AgentSessionID: "run-1"}, // reaped: pointer cleared
			}
			if tc.sibling != nil {
				chats = append(chats, tc.sibling)
			}
			checker := &defaultLivenessChecker{
				sessions: &mockSessionStoreLiveness{
					sessions: map[string]*models.Session{
						"sess-m": {ID: "sess-m", State: machine.ImplementingPlan},
					},
				},
				chats:           &parkedChatStore{bySession: map[string][]*models.AgentChat{"sess-m": chats}},
				agentForSession: constAgent(&mockAgentRunnerLiveness{running: map[string]bool{}}),
				tmux:            tmux.NewClient(tmux.WithCommandFactory(fake.factory)),
			}

			if got := checker.SessionLiveness(context.Background(), "sess-m"); got != tc.want {
				t.Errorf("SessionLiveness = %s, want %s", got, tc.want)
			}
		})
	}
}
