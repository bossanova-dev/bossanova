package session

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sessionreason"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
	gitpkg "github.com/recurser/bossd/internal/git"
	"github.com/recurser/bossd/internal/proofenv"
	"github.com/recurser/bossd/internal/status/questionsignal"
	"github.com/recurser/bossd/internal/tmux"
)

// Compile-time interface assertions for test mocks.
var (
	_ db.SessionStore                                    = (*mockSessionStore)(nil)
	_ db.RepoStore                                       = (*mockRepoStore)(nil)
	_ db.AgentChatStore                                  = (*mockAgentChatStore)(nil)
	_ gitpkg.WorktreeManager                             = (*mockWorktreeManager)(nil)
	_ agent.AgentRunner                                  = (*mockAgentRunner)(nil)
	_ agent.AgentDispatcher                              = (*mockAgentRunner)(nil)
	_ agent.HeadlessCapabilityProfileDispatcher          = (*mockAgentRunner)(nil)
	_ agent.HeadlessCapabilityProfilePreflightDispatcher = (*mockAgentRunner)(nil)
	_ vcs.Provider                                       = (*mockVCSProvider)(nil)
)

// newTestLifecycle builds a Lifecycle exactly like NewLifecycle but injects a
// keyring-free proof-env resolver. Every session test constructs its lifecycle
// through this so the whole package stays hermetic in one place: the real
// spawn paths (StartSession --detach, cron tmux chats, ResurrectSession,
// StartTmuxChat) resolve the proof env overlay, and the production resolver
// opens the real OS keyring — which is non-deterministic (it can prompt) and,
// on Linux, spawns a godbus connection goroutine that is never closed (a leak
// goleak flags). Production wires proofenv.New via NewLifecycle.
func newTestLifecycle(
	sessions db.SessionStore,
	repos db.RepoStore,
	agentChats db.AgentChatStore,
	cronJobs db.CronJobStore,
	worktrees gitpkg.WorktreeManager,
	agentRunner agent.AgentDispatcher,
	tmuxClient *tmux.Client,
	provider vcs.Provider,
	logger zerolog.Logger,
) *Lifecycle {
	lc := NewLifecycle(sessions, repos, agentChats, cronJobs, worktrees, agentRunner, tmuxClient, provider, logger)
	lc.SetProofEnvResolver(proofenv.NewNoop())
	return lc
}

// --- Mock SessionStore ---

type mockSessionStore struct {
	mu       sync.Mutex
	sessions map[string]*models.Session
	updates  []mockSessionUpdate

	updateHook                 func(id string, params db.UpdateSessionParams) error
	updateStateConditionalHook func(id string)
	orphanResumeCommitHook     func(id string)
	orphanResumeCommitErr      error
	orphanResumeReparkErr      error
}

type mockSessionUpdate struct {
	id     string
	params db.UpdateSessionParams
}

func newMockSessionStore() *mockSessionStore {
	return &mockSessionStore{sessions: make(map[string]*models.Session)}
}

func (m *mockSessionStore) Create(_ context.Context, params db.CreateSessionParams) (*models.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := &models.Session{
		ID:           "sess-1",
		RepoID:       params.RepoID,
		Title:        params.Title,
		Plan:         params.Plan,
		WorktreePath: params.WorktreePath,
		BranchName:   params.BranchName,
		BaseBranch:   params.BaseBranch,
		State:        machine.CreatingWorktree,
	}
	m.sessions[s.ID] = s
	return s, nil
}

func (m *mockSessionStore) Get(_ context.Context, id string) (*models.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, fmt.Errorf("session %s not found", id)
	}
	return s, nil
}

// List takes m.mu like Get and Update do. Lifecycle now reaches this map from a
// background goroutine (the BOS-540 draft-PR step), so an unguarded range here
// is one refactor away from being a real -race source.
func (m *mockSessionStore) List(_ context.Context, repoID string) ([]*models.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []*models.Session
	for _, s := range m.sessions {
		if repoID != "" && s.RepoID != repoID {
			continue
		}
		result = append(result, s)
	}
	return result, nil
}

func (m *mockSessionStore) ListActive(_ context.Context, repoID string) ([]*models.Session, error) {
	return m.List(context.Background(), repoID)
}

func (m *mockSessionStore) ListActiveWithRepo(_ context.Context, _ string) ([]*db.SessionWithRepo, error) {
	var result []*db.SessionWithRepo
	for _, s := range m.sessions {
		result = append(result, &db.SessionWithRepo{Session: s})
	}
	return result, nil
}

func (m *mockSessionStore) ListWithRepo(_ context.Context, _ string) ([]*db.SessionWithRepo, error) {
	var result []*db.SessionWithRepo
	for _, s := range m.sessions {
		result = append(result, &db.SessionWithRepo{Session: s})
	}
	return result, nil
}

func (m *mockSessionStore) ListByRepoAndPR(_ context.Context, repoID string, prNumber int) ([]*db.SessionWithRepo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []*db.SessionWithRepo
	for _, s := range m.sessions {
		if s.RepoID == repoID && s.PRNumber != nil && *s.PRNumber == prNumber && s.ArchivedAt == nil {
			result = append(result, &db.SessionWithRepo{Session: s})
		}
	}
	return result, nil
}

func (m *mockSessionStore) ListArchived(_ context.Context, _ string) ([]*models.Session, error) {
	return nil, nil
}

func (m *mockSessionStore) Update(_ context.Context, id string, params db.UpdateSessionParams) (*models.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, fmt.Errorf("session %s not found", id)
	}
	if m.updateHook != nil {
		if err := m.updateHook(id, params); err != nil {
			return nil, err
		}
	}
	m.updates = append(m.updates, mockSessionUpdate{id: id, params: params})
	if params.State != nil {
		s.State = machine.State(*params.State)
	}
	if params.Title != nil {
		s.Title = *params.Title
	}
	if params.WorktreePath != nil {
		s.WorktreePath = *params.WorktreePath
	}
	if params.BranchName != nil {
		s.BranchName = *params.BranchName
	}
	if params.AgentSessionID != nil {
		s.AgentSessionID = *params.AgentSessionID
	}
	if params.PRNumber != nil {
		s.PRNumber = *params.PRNumber
	}
	if params.PRURL != nil {
		s.PRURL = *params.PRURL
	}
	if params.LastCheckState != nil {
		s.LastCheckState = machine.CheckState(*params.LastCheckState)
	}
	if params.LastObservedReviewState != nil {
		s.LastObservedReviewState = *params.LastObservedReviewState
	}
	if params.AttemptCount != nil {
		s.AttemptCount = *params.AttemptCount
	}
	if params.BlockedReason != nil {
		s.BlockedReason = *params.BlockedReason
	}
	if params.LastAttemptHeadSHA != nil {
		s.LastAttemptHeadSHA = *params.LastAttemptHeadSHA
	}
	if params.TmuxSessionName != nil {
		s.TmuxSessionName = *params.TmuxSessionName
	}
	if params.CronJobID != nil {
		s.CronJobID = *params.CronJobID
	}
	if params.HookToken != nil {
		s.HookToken = *params.HookToken
	}
	if params.AccountID != nil {
		s.AccountID = *params.AccountID
	}
	if params.IsTmuxUnattended != nil {
		s.IsTmuxUnattended = *params.IsTmuxUnattended
	}
	if params.IsQuickChat != nil {
		s.IsQuickChat = *params.IsQuickChat
	}
	if params.Detach != nil {
		s.Detach = *params.Detach
	}
	if params.RotationAttemptCount != nil {
		s.RotationAttemptCount = *params.RotationAttemptCount
	}
	if params.RotationResumeAt != nil {
		if *params.RotationResumeAt == nil {
			s.RotationResumeAt = nil
		} else if parsed, err := time.Parse(time.RFC3339, **params.RotationResumeAt); err == nil {
			s.RotationResumeAt = &parsed
		}
	}
	return s, nil
}

func (m *mockSessionStore) updatesFor(id, field string) []db.UpdateSessionParams {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []db.UpdateSessionParams
	for _, update := range m.updates {
		if update.id != id {
			continue
		}
		switch field {
		case "worktree_path":
			if update.params.WorktreePath != nil {
				result = append(result, update.params)
			}
		default:
			panic("unsupported mock session update field: " + field)
		}
	}
	return result
}

func (m *mockSessionStore) Archive(_ context.Context, id string) error {
	s, ok := m.sessions[id]
	if !ok {
		return fmt.Errorf("session %s not found", id)
	}
	_ = s
	return nil
}

func (m *mockSessionStore) Resurrect(_ context.Context, id string) error {
	s, ok := m.sessions[id]
	if !ok {
		return fmt.Errorf("session %s not found", id)
	}
	s.ArchivedAt = nil
	return nil
}

func (m *mockSessionStore) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
	return nil
}

func (m *mockSessionStore) AdvanceOrphanedSessions(_ context.Context) (int64, error) {
	return 0, nil
}

func (m *mockSessionStore) UpdateRepairDiagnostics(_ context.Context, params db.UpdateRepairDiagnosticsParams) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[params.SessionID]
	if !ok {
		return nil
	}
	s.LastRepairStartedAt = &params.StartedAt
	s.LastRepairRunnerError = params.RunnerError
	s.LastRepairExitError = params.ExitError
	s.LastRepairAttemptCount++
	return nil
}

func (m *mockSessionStore) UpdateRepairBlocked(_ context.Context, sessionID string, at time.Time, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[sessionID]
	if !ok {
		return nil
	}
	s.LastRepairBlockedReason = reason
	blockedAt := at
	s.LastRepairBlockedAt = &blockedAt
	return nil
}

func (m *mockSessionStore) ListByState(_ context.Context, state int) ([]*models.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []*models.Session
	for _, s := range m.sessions {
		if int(s.State) == state {
			result = append(result, s)
		}
	}
	return result, nil
}

func (m *mockSessionStore) ListByStates(_ context.Context, states []int) ([]*models.Session, error) {
	if len(states) == 0 {
		return nil, nil
	}
	want := make(map[int]struct{}, len(states))
	for _, st := range states {
		want[st] = struct{}{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []*models.Session
	for _, s := range m.sessions {
		if _, ok := want[int(s.State)]; ok {
			result = append(result, s)
		}
	}
	return result, nil
}

func (m *mockSessionStore) UpdateStateConditionalFrom(_ context.Context, id string, newState int, expectedStates []int) (bool, error) {
	if len(expectedStates) == 0 {
		return false, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return false, nil
	}
	for _, st := range expectedStates {
		if int(s.State) == st {
			s.State = machine.State(newState)
			return true, nil
		}
	}
	return false, nil
}

func (m *mockSessionStore) UpdateStateConditional(_ context.Context, id string, newState, expectedState int) (bool, error) {
	// Mirror the SQL `UPDATE ... WHERE state = ?` atomic check-and-set so the
	// FinalizeSession idempotency test (concurrent goroutines) sees the same
	// race-free behavior the real SQLite store provides.
	if m.updateStateConditionalHook != nil {
		m.updateStateConditionalHook(id)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return false, nil
	}
	if int(s.State) != expectedState {
		return false, nil
	}
	s.State = machine.State(newState)
	return true, nil
}

func (m *mockSessionStore) OrphanHeadlessRun(_ context.Context, id, reason string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok || s.State != machine.ImplementingPlan {
		return false, nil
	}
	reasonPtr := &reason
	if m.updateHook != nil {
		if err := m.updateHook(id, db.UpdateSessionParams{BlockedReason: &reasonPtr}); err != nil {
			return false, err
		}
	}
	s.State = machine.Orphaned
	s.BlockedReason = reasonPtr
	return true, nil
}

func (m *mockSessionStore) ClaimUnarchivedOrphan(_ context.Context, id, reason string) (bool, error) {
	if m.updateStateConditionalHook != nil {
		m.updateStateConditionalHook(id)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok || s.State != machine.Orphaned || s.ArchivedAt != nil || s.BlockedReason == nil || *s.BlockedReason != reason {
		return false, nil
	}
	s.State = machine.ImplementingPlan
	return true, nil
}

func (m *mockSessionStore) CommitOrphanResume(_ context.Context, id, reason string, priorAgentSession *string, newAgentSessionID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.orphanResumeCommitErr != nil {
		return false, m.orphanResumeCommitErr
	}
	if m.orphanResumeCommitHook != nil {
		m.orphanResumeCommitHook(id)
	}
	s, ok := m.sessions[id]
	if !ok || s.State != machine.ImplementingPlan || s.ArchivedAt != nil || s.BlockedReason == nil || *s.BlockedReason != reason || !sameAgentSessionID(s.AgentSessionID, priorAgentSession) {
		return false, nil
	}
	s.AgentSessionID = &newAgentSessionID
	s.BlockedReason = nil
	return true, nil
}

func (m *mockSessionStore) ReleaseOrphanResumeClaim(_ context.Context, id, reason string, priorAgentSession *string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok || s.State != machine.ImplementingPlan || s.ArchivedAt != nil || (s.BlockedReason != nil && *s.BlockedReason != reason) || !sameAgentSessionID(s.AgentSessionID, priorAgentSession) {
		return false, nil
	}
	s.State = machine.Orphaned
	return true, nil
}

func (m *mockSessionStore) ReparkOrphanResume(_ context.Context, id, reason string, priorAgentSession *string, newAgentSessionID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.orphanResumeReparkErr != nil {
		return false, m.orphanResumeReparkErr
	}
	s, ok := m.sessions[id]
	if !ok || s.State != machine.ImplementingPlan || s.ArchivedAt != nil || s.BlockedReason != nil || s.AgentSessionID == nil || *s.AgentSessionID != newAgentSessionID {
		return false, nil
	}
	s.State = machine.Orphaned
	s.AgentSessionID = priorAgentSession
	s.BlockedReason = &reason
	return true, nil
}

func sameAgentSessionID(left, right *string) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

// --- Mock RepoStore ---

type mockRepoStore struct {
	repos map[string]*models.Repo
	// getCalls counts Get lookups, so a test can assert that a code path which
	// looks a repo up was (or was not) entered. Atomic because the store is
	// shared with the archive/poll goroutines some tests leave running, not
	// because archiveSessionAfterMergeIfEnabled's own Get is detached (it runs
	// on the caller's goroutine; only ArchiveSession is inside safego.Go).
	getCalls atomic.Int64
}

func newMockRepoStore() *mockRepoStore {
	return &mockRepoStore{repos: make(map[string]*models.Repo)}
}

func (m *mockRepoStore) Create(_ context.Context, params db.CreateRepoParams) (*models.Repo, error) {
	r := &models.Repo{
		ID:                "repo-1",
		DisplayName:       params.DisplayName,
		LocalPath:         params.LocalPath,
		DefaultBaseBranch: params.DefaultBaseBranch,
		WorktreeBaseDir:   params.WorktreeBaseDir,
		SetupScript:       params.SetupScript,
	}
	m.repos[r.ID] = r
	return r, nil
}

func (m *mockRepoStore) Get(_ context.Context, id string) (*models.Repo, error) {
	m.getCalls.Add(1)
	r, ok := m.repos[id]
	if !ok {
		return nil, fmt.Errorf("repo %s not found", id)
	}
	return r, nil
}

func (m *mockRepoStore) GetByPath(_ context.Context, _ string) (*models.Repo, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockRepoStore) GetByOrigin(_ context.Context, originURL string) (*models.Repo, error) {
	_, targetWebURL, targetOK := vcs.RepoWebLink(originURL)
	for _, r := range m.repos {
		if r.OriginURL == originURL {
			return r, nil
		}
		if targetOK {
			_, repoWebURL, repoOK := vcs.RepoWebLink(r.OriginURL)
			if repoOK && repoWebURL == targetWebURL {
				return r, nil
			}
		}
	}
	return nil, fmt.Errorf("repo origin %s not found", originURL)
}

func (m *mockRepoStore) List(_ context.Context) ([]*models.Repo, error) {
	var result []*models.Repo
	for _, r := range m.repos {
		result = append(result, r)
	}
	return result, nil
}

func (m *mockRepoStore) Update(_ context.Context, id string, params db.UpdateRepoParams) (*models.Repo, error) {
	r, ok := m.repos[id]
	if !ok {
		return nil, fmt.Errorf("repo %s not found", id)
	}
	if params.OriginURL != nil {
		r.OriginURL = *params.OriginURL
	}
	if params.DisplayName != nil {
		r.DisplayName = *params.DisplayName
	}
	return r, nil
}

func (m *mockRepoStore) Delete(_ context.Context, id string) error {
	delete(m.repos, id)
	return nil
}

// --- Mock AgentChatStore ---

// mockAgentChatStore satisfies db.AgentChatStore for lifecycle tests. By
// default Create / UpdateTmuxSessionName / DeleteByAgentSessionID succeed and
// record their parameters so tests can assert on them. Setting createErr,
// updateTmuxNameErr, etc. forces the corresponding method to return that
// error instead — used by failure-mode tests for the cron tmux path.
type mockAgentChatStore struct {
	mu                     sync.Mutex
	createCalls            []db.CreateAgentChatParams
	agentSessionIDUpdates  []agentSessionIDUpdate
	tmuxNameUpdates        []tmuxNameUpdate
	accountIDUpdates       []accountIDUpdate
	deletedAgentSessionIDs []string
	markStartFailedCalls   []markStartFailedCall
	chatsBySession         map[string][]*models.AgentChat // returned by ListBySession when set
	chatsWithTmux          []*models.AgentChat            // returned by ListWithTmuxSession when set
	createErr              error
	updateTmuxNameErr      error
	deleteErr              error
	listBySessionErr       error // when non-nil, ListBySession returns it
	listWithTmuxErr        error // when non-nil, ListWithTmuxSession returns it
}

type markStartFailedCall struct {
	agentSessionID string
	reason         string
}

type tmuxNameUpdate struct {
	agentSessionID string
	name           *string
}

type agentSessionIDUpdate struct {
	id                string
	oldAgentSessionID string
	newAgentSessionID string
}

type accountIDUpdate struct {
	agentSessionID string
	accountID      *string
}

func (m *mockAgentChatStore) Create(_ context.Context, params db.CreateAgentChatParams) (*models.AgentChat, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createCalls = append(m.createCalls, params)
	if m.createErr != nil {
		return nil, m.createErr
	}
	return &models.AgentChat{
		ID:                "chat-" + params.AgentSessionID,
		SessionID:         params.SessionID,
		AgentSessionID:    params.AgentSessionID,
		ProviderSessionID: params.ProviderSessionID,
		Title:             params.Title,
	}, nil
}

func (m *mockAgentChatStore) GetByAgentSessionID(_ context.Context, agentSessionID string) (*models.AgentChat, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, chats := range m.chatsBySession {
		for _, chat := range chats {
			if chat.AgentSessionID == agentSessionID {
				return chat, nil
			}
		}
	}
	for _, chat := range m.chatsWithTmux {
		if chat.AgentSessionID == agentSessionID {
			return chat, nil
		}
	}
	return nil, db.ErrAgentChatNotFound
}

func (m *mockAgentChatStore) ListBySession(_ context.Context, sessionID string) ([]*models.AgentChat, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listBySessionErr != nil {
		return nil, m.listBySessionErr
	}
	if m.chatsBySession == nil {
		return nil, nil
	}
	return m.chatsBySession[sessionID], nil
}

func (m *mockAgentChatStore) ListBySessions(_ context.Context, sessionIDs []string) (map[string][]*models.AgentChat, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	chatsBySession := make(map[string][]*models.AgentChat, len(sessionIDs))
	if m.chatsBySession == nil {
		return chatsBySession, nil
	}
	for _, id := range sessionIDs {
		chatsBySession[id] = m.chatsBySession[id]
	}
	return chatsBySession, nil
}

func (m *mockAgentChatStore) UpdateTitle(_ context.Context, _, _ string) error {
	return nil
}

func (m *mockAgentChatStore) UpdateTitleByAgentSessionID(_ context.Context, _, _ string) error {
	return nil
}

func (m *mockAgentChatStore) UpdateAgentSessionID(_ context.Context, id, oldAgentSessionID, newAgentSessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.agentSessionIDUpdates = append(m.agentSessionIDUpdates, agentSessionIDUpdate{id: id, oldAgentSessionID: oldAgentSessionID, newAgentSessionID: newAgentSessionID})
	for _, chats := range m.chatsBySession {
		for _, chat := range chats {
			if chat.ID == id && chat.AgentSessionID == oldAgentSessionID {
				chat.AgentSessionID = newAgentSessionID
			}
		}
	}
	for _, chat := range m.chatsWithTmux {
		if chat.ID == id && chat.AgentSessionID == oldAgentSessionID {
			chat.AgentSessionID = newAgentSessionID
		}
	}
	return nil
}

func (m *mockAgentChatStore) UpdateTmuxSessionName(_ context.Context, agentSessionID string, name *string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tmuxNameUpdates = append(m.tmuxNameUpdates, tmuxNameUpdate{agentSessionID: agentSessionID, name: name})
	for _, chats := range m.chatsBySession {
		for _, chat := range chats {
			if chat.AgentSessionID == agentSessionID {
				chat.TmuxSessionName = name
			}
		}
	}
	for _, chat := range m.chatsWithTmux {
		if chat.AgentSessionID == agentSessionID {
			chat.TmuxSessionName = name
		}
	}
	return m.updateTmuxNameErr
}

func (m *mockAgentChatStore) UpdateProviderSessionID(_ context.Context, _ string, _ *string) error {
	return nil
}

func (m *mockAgentChatStore) UpdateAccountIDByAgentSessionID(_ context.Context, agentSessionID string, accountID *string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.accountIDUpdates = append(m.accountIDUpdates, accountIDUpdate{agentSessionID: agentSessionID, accountID: accountID})
	for _, chats := range m.chatsBySession {
		for _, chat := range chats {
			if chat.AgentSessionID == agentSessionID {
				chat.AccountID = accountID
			}
		}
	}
	for _, chat := range m.chatsWithTmux {
		if chat.AgentSessionID == agentSessionID {
			chat.AccountID = accountID
		}
	}
	return nil
}

func (m *mockAgentChatStore) MarkStartFailed(_ context.Context, agentSessionID, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.markStartFailedCalls = append(m.markStartFailedCalls, markStartFailedCall{agentSessionID: agentSessionID, reason: reason})
	for _, chats := range m.chatsBySession {
		for _, chat := range chats {
			if chat.AgentSessionID == agentSessionID {
				chat.TmuxSessionName = nil
				chat.StartError = &reason
			}
		}
	}
	for _, chat := range m.chatsWithTmux {
		if chat.AgentSessionID == agentSessionID {
			chat.TmuxSessionName = nil
			chat.StartError = &reason
		}
	}
	return nil
}

func (m *mockAgentChatStore) DeleteByAgentSessionID(_ context.Context, agentSessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletedAgentSessionIDs = append(m.deletedAgentSessionIDs, agentSessionID)
	if m.deleteErr != nil {
		return m.deleteErr
	}
	return nil
}

func (m *mockAgentChatStore) ListWithTmuxSession(_ context.Context) ([]*models.AgentChat, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listWithTmuxErr != nil {
		return nil, m.listWithTmuxErr
	}
	return m.chatsWithTmux, nil
}

func (m *mockAgentChatStore) ListRoutableChats(_ context.Context) ([]*models.AgentChat, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.chatsWithTmux, nil
}

// --- Mock WorktreeManager ---

type mockWorktreeManager struct {
	created                     []gitpkg.CreateOpts
	createErr                   error // if set, Create returns this error
	createdFromExisting         []gitpkg.CreateFromExistingBranchOpts
	createFromExistingBranchErr error // if set, CreateFromExistingBranch returns this error
	onSetupScript               func()
	archived                    []string
	archiveErr                  error  // if set, Archive returns this error
	archiveHook                 func() // if set, invoked inside Archive() mid-run (used to observe transient flags)
	archiveCtx                  context.Context
	archiveCtxLive              bool
	resurrected                 []gitpkg.ResurrectOpts
	pushed                      []string
	pushErr                     error    // if set, Push returns this error
	emptyCommits                []string // worktree paths on which EmptyCommit was invoked
	emptyCommitCalls            int
	verifyCurrentBranchErr      error
	// verifyCurrentBranchHook, when set, runs inside VerifyCurrentBranch — the
	// FIRST worktree call createDraftPR makes. The BOS-540 tests use it to park
	// the background create before it has touched anything, so the assertions
	// that follow observe the session exactly as a client would while the PR is
	// still being opened.
	verifyCurrentBranchHook func()
	// verifyCurrentBranchCtxHook is the same park point with the call's context
	// in hand, so a test can stand in for a git/GitHub call that honours
	// cancellation rather than one that ignores it.
	verifyCurrentBranchCtxHook func(context.Context)
	verifiedBranches           []string
	verifyPushedErr            error
	verifyPushedResult         *gitpkg.BranchVerification
	verifyPushedCalls          []verifyPushedCall
	latestCommitSubject        string
	latestCommitSubjectErr     error
	commitSubjects             []string
	commitSubjectsErr          error
	// commitSubjectsFn, when set, overrides commitSubjects/commitSubjectsErr and
	// lets a test return different subjects depending on the base ref passed to
	// CommitSubjects (e.g. local "main" vs "refs/remotes/origin/main").
	commitSubjectsFn       func(baseRef string) ([]string, error)
	commitSubjectsBaseRefs []string // base refs passed to CommitSubjects, in call order
	// hasDiffAgainstBase is what HasDiffAgainstBase reports, but ONLY when
	// hasDiffAgainstBaseOK is set. The extra opt-in flag keeps the zero-value
	// mock answering "has a diff" — the pre-BOS-591 behaviour every existing
	// finalize test assumes — while still letting a test assert the empty-diff
	// refusal, rather than overloading a bool's zero value as "unset".
	hasDiffAgainstBase          bool
	hasDiffAgainstBaseOK        bool
	hasDiffAgainstBaseErr       error
	hasDiffAgainstBaseRefs      []string // base refs passed to HasDiffAgainstBase, in call order
	branchDebugSnapshot         *gitpkg.BranchDebugSnapshot
	branchDebugSnapshotErr      error
	branchDebugSnapshotCalls    []branchDebugSnapshotCall
	originURL                   string // returned by DetectOriginURL
	statusOut                   string // returned by Status
	statusErr                   error  // if set, Status returns this error
	statusCalls                 int
	worktreePath                string // override for Create's returned WorktreePath; empty uses the historical fixed path
	fetchedBases                []string
	fetchBaseErr                error
	isAncestorFn                func(localPath, ref, target string) (bool, error)
	branchSafeToDelete          bool     // returned by BranchSafeToDelete when branchSafeToDeleteErr is nil
	branchSafeToDeleteErr       error    // if set, BranchSafeToDelete returns this error
	deletedLocalBranches        []string // branches passed to DeleteLocalBranch
	deleteLocalBranchErr        error    // if set, DeleteLocalBranch returns this error
	deleteLocalBranchCtx        context.Context
	deleteLocalBranchCtxLive    bool
	injectPRNumbersCalls        []injectPRNumbersCall
	injectPRNumbersErr          error
	retryDeferredBaseSyncsCalls int
}

type injectPRNumbersCall struct {
	branch   string
	prNumber int
	baseRef  string
}

type branchDebugSnapshotCall struct {
	worktreePath string
	branch       string
	baseBranch   string
}

type verifyPushedCall struct {
	worktreePath string
	branch       string
	baseBranch   string
	skipFetch    bool
}

func (m *mockWorktreeManager) Create(_ context.Context, opts gitpkg.CreateOpts) (*gitpkg.CreateResult, error) {
	m.created = append(m.created, opts)
	if opts.SetupScript != nil && strings.TrimSpace(*opts.SetupScript) != "" && m.onSetupScript != nil {
		m.onSetupScript()
	}
	if m.createErr != nil {
		return nil, m.createErr
	}
	path := m.worktreePath
	if path == "" {
		path = "/tmp/worktrees/test-repo/test-session"
	}
	return &gitpkg.CreateResult{
		WorktreePath: path,
		BranchName:   "test-session",
	}, nil
}

func (m *mockWorktreeManager) Archive(ctx context.Context, path string) error {
	m.archiveCtx = ctx
	m.archiveCtxLive = ctx.Err() == nil
	m.archived = append(m.archived, path)
	if m.archiveHook != nil {
		m.archiveHook()
	}
	return m.archiveErr
}

func (m *mockWorktreeManager) PurgeWorktree(_ context.Context, _, _, _, _ string) {}

func (m *mockWorktreeManager) Resurrect(_ context.Context, opts gitpkg.ResurrectOpts) error {
	m.resurrected = append(m.resurrected, opts)
	return nil
}

func (m *mockWorktreeManager) EmptyCommit(_ context.Context, worktreePath, _ string) error {
	m.emptyCommitCalls++
	m.emptyCommits = append(m.emptyCommits, worktreePath)
	return nil
}

func (m *mockWorktreeManager) VerifyCurrentBranch(ctx context.Context, _ string, branch string) error {
	m.verifiedBranches = append(m.verifiedBranches, branch)
	if m.verifyCurrentBranchCtxHook != nil {
		m.verifyCurrentBranchCtxHook(ctx)
	}
	if m.verifyCurrentBranchHook != nil {
		m.verifyCurrentBranchHook()
	}
	return m.verifyCurrentBranchErr
}

func (m *mockWorktreeManager) Push(_ context.Context, _ string, branch string) error {
	m.pushed = append(m.pushed, branch)
	return m.pushErr
}

func (m *mockWorktreeManager) PushWithLease(_ context.Context, _ string, branch, expectedRemoteSHA string) (string, error) {
	if expectedRemoteSHA == "" {
		return "", errors.New("expected remote SHA is required")
	}
	if m.pushErr != nil {
		return "", m.pushErr
	}
	m.pushed = append(m.pushed, branch)
	return "pushed-head-sha", nil
}

func (m *mockWorktreeManager) InjectPRNumbers(_ context.Context, _, branch string, prNumber int, baseRef string) error {
	m.injectPRNumbersCalls = append(m.injectPRNumbersCalls, injectPRNumbersCall{
		branch:   branch,
		prNumber: prNumber,
		baseRef:  baseRef,
	})
	return m.injectPRNumbersErr
}

func (m *mockWorktreeManager) VerifyPushedBranchAheadOfBase(_ context.Context, worktreePath, branch, baseBranch string, opts gitpkg.VerifyPushedBranchAheadOfBaseOpts) (*gitpkg.BranchVerification, error) {
	m.verifyPushedCalls = append(m.verifyPushedCalls, verifyPushedCall{
		worktreePath: worktreePath,
		branch:       branch,
		baseBranch:   baseBranch,
		skipFetch:    opts.SkipFetch,
	})
	if m.verifyPushedErr != nil {
		return nil, m.verifyPushedErr
	}
	if m.verifyPushedResult != nil {
		return m.verifyPushedResult, nil
	}
	return &gitpkg.BranchVerification{
		HeadSHA:       "head-sha",
		BaseSHA:       "base-sha",
		RemoteHeadSHA: "head-sha",
		AheadCount:    1,
	}, nil
}

func (m *mockWorktreeManager) Status(_ context.Context, _ string) (string, error) {
	m.statusCalls++
	return m.statusOut, m.statusErr
}

func (m *mockWorktreeManager) CommitSubjects(_ context.Context, _ string, baseRef string) ([]string, error) {
	m.commitSubjectsBaseRefs = append(m.commitSubjectsBaseRefs, baseRef)
	if m.commitSubjectsFn != nil {
		return m.commitSubjectsFn(baseRef)
	}
	return m.commitSubjects, m.commitSubjectsErr
}

func (m *mockWorktreeManager) HasDiffAgainstBase(_ context.Context, _ string, baseRef string) (bool, error) {
	m.hasDiffAgainstBaseRefs = append(m.hasDiffAgainstBaseRefs, baseRef)
	if m.hasDiffAgainstBaseErr != nil {
		return false, m.hasDiffAgainstBaseErr
	}
	if !m.hasDiffAgainstBaseOK {
		return true, nil
	}
	return m.hasDiffAgainstBase, nil
}

func (m *mockWorktreeManager) LatestCommitSubject(_ context.Context, _ string) (string, error) {
	return m.latestCommitSubject, m.latestCommitSubjectErr
}

func (m *mockWorktreeManager) BranchDebugSnapshot(_ context.Context, worktreePath, branch, baseBranch string) (*gitpkg.BranchDebugSnapshot, error) {
	m.branchDebugSnapshotCalls = append(m.branchDebugSnapshotCalls, branchDebugSnapshotCall{
		worktreePath: worktreePath,
		branch:       branch,
		baseBranch:   baseBranch,
	})
	if m.branchDebugSnapshotErr != nil {
		return nil, m.branchDebugSnapshotErr
	}
	if m.branchDebugSnapshot != nil {
		return m.branchDebugSnapshot, nil
	}
	return &gitpkg.BranchDebugSnapshot{}, nil
}

func (m *mockWorktreeManager) Clone(_ context.Context, _, _ string) error {
	return nil
}

func (m *mockWorktreeManager) EmptyTrash(_ context.Context, _ string, _ []string) error {
	return nil
}

func (m *mockWorktreeManager) DeleteLocalBranch(ctx context.Context, _, branch string) error {
	m.deleteLocalBranchCtx = ctx
	m.deleteLocalBranchCtxLive = ctx.Err() == nil
	m.deletedLocalBranches = append(m.deletedLocalBranches, branch)
	return m.deleteLocalBranchErr
}

func (m *mockWorktreeManager) BranchSafeToDelete(_ context.Context, _, _, _ string) (bool, error) {
	return m.branchSafeToDelete, m.branchSafeToDeleteErr
}

func (m *mockWorktreeManager) DetectOriginURL(_ context.Context, _ string) (string, error) {
	return m.originURL, nil
}

func (m *mockWorktreeManager) IsGitRepo(_ context.Context, _ string) bool {
	return true
}

func (m *mockWorktreeManager) DetectDefaultBranch(_ context.Context, _ string) (string, error) {
	return "main", nil
}

func (m *mockWorktreeManager) RetryDeferredBaseSyncs(_ context.Context) {
	m.retryDeferredBaseSyncsCalls++
}

func (m *mockWorktreeManager) IsAncestor(_ context.Context, localPath, ref, target string) (bool, error) {
	if m.isAncestorFn != nil {
		return m.isAncestorFn(localPath, ref, target)
	}
	return true, nil
}

func (m *mockWorktreeManager) CountMergeCommits(_ context.Context, _, _, _ string) (int, error) {
	return 0, nil
}

func (m *mockWorktreeManager) MergeLocalBranch(_ context.Context, _, _, _, _ string) error {
	return nil
}

func (m *mockWorktreeManager) CountBehindBase(_ context.Context, _, _, _ string) (int, error) {
	return 0, nil
}

func (m *mockWorktreeManager) RebaseOntoBaseAndPush(_ context.Context, _, _, _ string) (*gitpkg.RebaseResult, error) {
	return &gitpkg.RebaseResult{}, nil
}

func (m *mockWorktreeManager) FetchBase(_ context.Context, _, base string) error {
	m.fetchedBases = append(m.fetchedBases, base)
	return m.fetchBaseErr
}

func (m *mockWorktreeManager) SyncBaseBranch(_ context.Context, _, _ string) error {
	return nil
}

func (m *mockWorktreeManager) CreateFromExistingBranch(_ context.Context, opts gitpkg.CreateFromExistingBranchOpts) (*gitpkg.CreateResult, error) {
	m.createdFromExisting = append(m.createdFromExisting, opts)
	if opts.SetupScript != nil && strings.TrimSpace(*opts.SetupScript) != "" && m.onSetupScript != nil {
		m.onSetupScript()
	}
	if m.createFromExistingBranchErr != nil {
		return nil, m.createFromExistingBranchErr
	}
	return &gitpkg.CreateResult{
		WorktreePath: "/tmp/worktrees/" + opts.BranchName,
		BranchName:   opts.BranchName,
	}, nil
}

// --- Mock AgentRunner ---

type mockAgentRunner struct {
	// mu guards running, which the orphan-resume poll goroutine
	// (watchHeadlessRunStatus → IsRunning) reads concurrently with the test
	// goroutine's Start/Stop mutations. The started/stopped slices are only
	// touched synchronously by the test goroutine, so they need no locking.
	mu       sync.Mutex
	started  []mockStartCall
	stopped  []string
	running  map[string]bool
	nextID   string
	startErr error // if set, Start returns this error

	preflightCalls int
	preflightErr   error
	// preflightErrFromCall selects which preflight call preflightErr applies to,
	// 1-based; the zero value means every call, which is what the single-probe
	// tests assume. StartSession runs two probes against the same runner (the
	// post-setup one in StartSession, then the authoritative one in
	// startTmuxChat), so a test that needs the FIRST to pass and the SECOND to
	// reject sets this to 2.
	preflightErrFromCall int
	preflights           []mockPreflightCall
}

type mockPreflightCall struct {
	agentName string
	model     string
	env       map[string]string
	profile   pb.HeadlessCapabilityProfile
}

type mockStartCall struct {
	workDir string
	plan    string
	resume  *string
	model   string
	env     map[string]string
	profile pb.HeadlessCapabilityProfile
}

func newMockAgentRunner() *mockAgentRunner {
	return &mockAgentRunner{
		running: make(map[string]bool),
		nextID:  "claude-123",
	}
}

func (m *mockAgentRunner) Start(_ context.Context, workDir, plan string, resume *string, _, model string, env map[string]string) (string, error) {
	m.started = append(m.started, mockStartCall{workDir: workDir, plan: plan, resume: resume, model: model, env: env})
	if m.startErr != nil {
		return "", m.startErr
	}
	id := m.nextID
	m.mu.Lock()
	m.running[id] = true
	m.mu.Unlock()
	return id, nil
}

func (m *mockAgentRunner) Stop(sessionID string) error {
	m.stopped = append(m.stopped, sessionID)
	m.mu.Lock()
	delete(m.running, sessionID)
	m.mu.Unlock()
	return nil
}

func (m *mockAgentRunner) IsRunning(sessionID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running[sessionID]
}

func (m *mockAgentRunner) ExitError(_ string) error {
	return nil
}

func (m *mockAgentRunner) Subscribe(_ context.Context, _ string) (<-chan agent.OutputLine, error) {
	ch := make(chan agent.OutputLine)
	close(ch)
	return ch, nil
}

func (m *mockAgentRunner) History(_ string) []agent.OutputLine {
	return nil
}

// StartByAgent forwards to Start so existing test assertions still fire.
// The test fakes don't need to inspect the agent name — by-agent routing
// is exercised by the dispatcher tests in services/bossd/internal/agent.
func (m *mockAgentRunner) StartByAgent(ctx context.Context, _, workDir, plan string, resume *string, agentSessionID, model string, extraEnv map[string]string) (string, error) {
	return m.Start(ctx, workDir, plan, resume, agentSessionID, model, extraEnv)
}

func (m *mockAgentRunner) StartByAgentWithHeadlessCapabilityProfile(ctx context.Context, _ string, workDir, plan string, resume *string, agentSessionID, model string, extraEnv map[string]string, profile pb.HeadlessCapabilityProfile) (string, error) {
	m.started = append(m.started, mockStartCall{workDir: workDir, plan: plan, resume: resume, model: model, env: extraEnv, profile: profile})
	if m.startErr != nil {
		return "", m.startErr
	}
	id := m.nextID
	m.mu.Lock()
	m.running[id] = true
	m.mu.Unlock()
	return id, nil
}

func (m *mockAgentRunner) PreflightByAgentWithHeadlessCapabilityProfile(_ context.Context, agentName, model string, extraEnv map[string]string, profile pb.HeadlessCapabilityProfile) error {
	m.preflightCalls++
	m.preflights = append(m.preflights, mockPreflightCall{
		agentName: agentName,
		model:     model,
		env:       extraEnv,
		profile:   profile,
	})
	if m.preflightErrFromCall > 0 && m.preflightCalls < m.preflightErrFromCall {
		return nil
	}
	return m.preflightErr
}

// StopByAgent forwards to Stop, ignoring the agent name (see StartByAgent).
func (m *mockAgentRunner) StopByAgent(_, agentSessionID string) error {
	return m.Stop(agentSessionID)
}

// IsRunningByAgent forwards to IsRunning, ignoring the agent name (see StartByAgent).
func (m *mockAgentRunner) IsRunningByAgent(_, agentSessionID string) bool {
	return m.IsRunning(agentSessionID)
}

// --- Mock VCS Provider ---

type mockVCSProvider struct {
	createPRCalls      []vcs.CreatePROpts
	listOpenPRCalls    []string
	updatePRTitleCalls []updatePRTitleCall
	markReadyCalls     []int
	mergePRCalls       []int
	nextPRInfo         *vcs.PRInfo
	nextPRStatus       *vcs.PRStatus
	nextCheckResults   []vcs.CheckResult
	nextReviewComments []vcs.ReviewComment
	nextOpenPRs        []vcs.PRSummary
	allowedStrategies  []string
	createPRErr        error
	listOpenPRErr      error
	updatePRTitleErr   error
	checkResultsErr    error
	reviewCommentsErr  error
	mergePRErr         error
	markReadyErr       error

	getCheckResultsCalls   int
	getReviewCommentsCalls int
	getPRStatusPRNumbers   []int

	// createPRHook, when set, fully replaces CreateDraftPR's body (including
	// its call recording). Used by the BOS-540 concurrency tests to park the
	// background create while another writer opens the PR.
	createPRHook func(vcs.CreatePROpts) (*vcs.PRInfo, error)
}

type updatePRTitleCall struct {
	repoPath string
	prID     int
	title    string
}

func newMockVCSProvider() *mockVCSProvider {
	return &mockVCSProvider{
		nextPRInfo:   &vcs.PRInfo{Number: 42, URL: "https://github.com/owner/repo/pull/42"},
		nextPRStatus: &vcs.PRStatus{State: vcs.PRStateOpen},
	}
}

func (m *mockVCSProvider) CreateDraftPR(_ context.Context, opts vcs.CreatePROpts) (*vcs.PRInfo, error) {
	if m.createPRHook != nil {
		// The hook owns both the call log and the result, so a test can make
		// concurrent creates (BOS-540's background create racing finalize) return
		// different answers per call under its own lock.
		return m.createPRHook(opts)
	}
	m.createPRCalls = append(m.createPRCalls, opts)
	if m.createPRErr != nil {
		return nil, m.createPRErr
	}
	return m.nextPRInfo, nil
}

func (m *mockVCSProvider) GetPRStatus(_ context.Context, _ string, prNumber int) (*vcs.PRStatus, error) {
	m.getPRStatusPRNumbers = append(m.getPRStatusPRNumbers, prNumber)
	if m.nextPRStatus != nil {
		return m.nextPRStatus, nil
	}
	return &vcs.PRStatus{State: vcs.PRStateOpen}, nil
}

func (m *mockVCSProvider) GetCheckResults(_ context.Context, _ string, _ int) ([]vcs.CheckResult, error) {
	m.getCheckResultsCalls++
	return m.nextCheckResults, m.checkResultsErr
}

func (m *mockVCSProvider) GetFailedCheckLogs(_ context.Context, _ string, _ string) (string, error) {
	return "", nil
}

func (m *mockVCSProvider) MarkReadyForReview(_ context.Context, _ string, prID int) error {
	m.markReadyCalls = append(m.markReadyCalls, prID)
	return m.markReadyErr
}

func (m *mockVCSProvider) GetReviewComments(_ context.Context, _ string, _ int) ([]vcs.ReviewComment, error) {
	m.getReviewCommentsCalls++
	return m.nextReviewComments, m.reviewCommentsErr
}

func (m *mockVCSProvider) ListOpenPRs(_ context.Context, repoPath string) ([]vcs.PRSummary, error) {
	m.listOpenPRCalls = append(m.listOpenPRCalls, repoPath)
	if m.listOpenPRErr != nil {
		return nil, m.listOpenPRErr
	}
	return m.nextOpenPRs, nil
}

func (m *mockVCSProvider) ListClosedPRs(_ context.Context, _ string) ([]vcs.PRSummary, error) {
	return nil, nil
}
func (m *mockVCSProvider) SearchPRsByTitleTag(_ context.Context, _, _ string) ([]vcs.PRSummary, error) {
	return nil, nil
}

func (m *mockVCSProvider) UpdatePRTitle(_ context.Context, repoPath string, prID int, title string) error {
	m.updatePRTitleCalls = append(m.updatePRTitleCalls, updatePRTitleCall{
		repoPath: repoPath,
		prID:     prID,
		title:    title,
	})
	return m.updatePRTitleErr
}

func (m *mockVCSProvider) MergePR(_ context.Context, _ string, prID int, _ string) error {
	m.mergePRCalls = append(m.mergePRCalls, prID)
	return m.mergePRErr
}

func (m *mockVCSProvider) GetPRMergeCommit(_ context.Context, _ string, prID int) (string, error) {
	return fmt.Sprintf("mock-merge-%d", prID), nil
}

func (m *mockVCSProvider) GetAllowedMergeStrategies(_ context.Context, _ string) ([]string, error) {
	if m.allowedStrategies != nil {
		return m.allowedStrategies, nil
	}
	return []string{"merge", "squash", "rebase"}, nil
}

// --- Tests ---

func TestStartSession(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	logger := zerolog.Nop()

	// Set up test data.
	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Test Session",
		Plan:       "Do something",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
	}

	lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), logger)

	// Detach=true is the `boss new --detach` headless path that auto-runs the
	// agent. Interactive sessions (Detach=false) are covered by the
	// AwaitsManualStart tests below.
	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{Detach: true}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	awaitDraftPR(t, lc, "sess-1")

	// Verify worktree was created.
	if len(wt.created) != 1 {
		t.Fatalf("expected 1 worktree created, got %d", len(wt.created))
	}
	if wt.created[0].RepoPath != "/tmp/repo" {
		t.Errorf("worktree repo path = %q, want /tmp/repo", wt.created[0].RepoPath)
	}
	if wt.created[0].BaseBranch != "main" {
		t.Errorf("worktree base branch = %q, want main", wt.created[0].BaseBranch)
	}

	// Verify Claude was started.
	if len(cr.started) != 1 {
		t.Fatalf("expected 1 claude start, got %d", len(cr.started))
	}
	if cr.started[0].workDir != "/tmp/worktrees/test-repo/test-session" {
		t.Errorf("claude workDir = %q, want /tmp/worktrees/test-repo/test-session", cr.started[0].workDir)
	}
	if cr.started[0].plan != "Do something" {
		t.Errorf("claude plan = %q, want 'Do something'", cr.started[0].plan)
	}
	if cr.started[0].resume != nil {
		t.Errorf("claude resume = %v, want nil", cr.started[0].resume)
	}

	// Verify session was updated.
	sess := sessions.sessions["sess-1"]
	if sess.State != machine.ImplementingPlan {
		t.Errorf("session state = %v, want ImplementingPlan", sess.State)
	}
	if sess.WorktreePath != "/tmp/worktrees/test-repo/test-session" {
		t.Errorf("worktree path = %q, want /tmp/worktrees/test-repo/test-session", sess.WorktreePath)
	}
	if sess.BranchName != "test-session" {
		t.Errorf("branch name = %q, want test-session", sess.BranchName)
	}
	if sess.AgentSessionID == nil || *sess.AgentSessionID != "claude-123" {
		t.Errorf("claude session id = %v, want claude-123", sess.AgentSessionID)
	}
}

func TestStartSession_TrackerSession_AwaitsManualStart(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}
	// A Linear/Sentry-sourced session carries a TrackerID and a Plan. It is
	// created idle: the worktree is set up but the agent is NOT auto-started,
	// so the user can review/edit the plan (pre-filled into the agent input on
	// first attach) and start the run manually. Cron sessions still auto-run
	// via the CronJobID branch; only `boss new --detach` (Detach=true) auto-runs
	// headlessly — plain interactive New/Existing PR sessions are now idle too
	// (see the AwaitsManualStart tests below).
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Test Session",
		Plan:       "Implement the ticket",
		TrackerID:  ptr("BOS-123"),
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
	}

	lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), logger)

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	awaitDraftPR(t, lc, "sess-1")

	// Worktree setup still runs.
	if len(wt.created) != 1 {
		t.Fatalf("expected 1 worktree created, got %d", len(wt.created))
	}

	// The agent must NOT be auto-started for a tracker session.
	if len(cr.started) != 0 {
		t.Fatalf("expected 0 agent starts for tracker session, got %d", len(cr.started))
	}

	// The session lands ready (ImplementingPlan) but idle — no agent session.
	sess := sessions.sessions["sess-1"]
	if sess.State != machine.ImplementingPlan {
		t.Errorf("session state = %v, want ImplementingPlan", sess.State)
	}
	if sess.AgentSessionID != nil {
		t.Errorf("agent session id = %v, want nil (idle)", sess.AgentSessionID)
	}
	// The plan is preserved for the prefill-on-attach path.
	if sess.Plan != "Implement the ticket" {
		t.Errorf("plan = %q, want preserved", sess.Plan)
	}
}

// TestStartSession_ForkGovernedByDetach pins the BOS-179 fix: Detach — NOT
// tracker-sourcing — decides headless-vs-idle for a non-cron session. The
// regression this guards is a detached, tracker-sourced session (exactly what
// /boss-epic's headless create_session fan-out produces) being forced into the
// idle "awaiting manual start on first attach" branch and never running. The
// !detach cases must stay byte-identical (idle) regardless of the tracker id.
func TestStartSession_ForkGovernedByDetach(t *testing.T) {
	cases := []struct {
		name         string
		detach       bool
		tracker      bool
		wantHeadless bool
	}{
		{name: "detach+tracker runs headlessly (the fix)", detach: true, tracker: true, wantHeadless: true},
		{name: "detach without tracker runs headlessly", detach: true, tracker: false, wantHeadless: true},
		{name: "no-detach with tracker stays idle", detach: false, tracker: true, wantHeadless: false},
		{name: "no-detach without tracker stays idle", detach: false, tracker: false, wantHeadless: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			sessions := newMockSessionStore()
			repos := newMockRepoStore()
			wt := &mockWorktreeManager{}
			cr := newMockAgentRunner()
			logger := zerolog.Nop()

			repos.repos["repo-1"] = &models.Repo{
				ID:                "repo-1",
				LocalPath:         "/tmp/repo",
				DefaultBaseBranch: "main",
				WorktreeBaseDir:   "/tmp/worktrees",
			}
			sess := &models.Session{
				ID:         "sess-1",
				RepoID:     "repo-1",
				Title:      "Test Session",
				Plan:       "Do the work",
				BaseBranch: "main",
				State:      machine.CreatingWorktree,
			}
			if tc.tracker {
				sess.TrackerID = ptr("BOS-123")
			}
			sessions.sessions["sess-1"] = sess

			lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), logger)

			if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{Detach: tc.detach}); err != nil {
				t.Fatalf("StartSession: %v", err)
			}
			awaitDraftPR(t, lc, "sess-1")

			got := sessions.sessions["sess-1"]
			if tc.wantHeadless {
				if len(cr.started) != 1 {
					t.Fatalf("expected 1 headless agent start, got %d", len(cr.started))
				}
				if got.AgentSessionID == nil || *got.AgentSessionID != "claude-123" {
					t.Errorf("agent session id = %v, want claude-123 (headless run)", got.AgentSessionID)
				}
			} else {
				if len(cr.started) != 0 {
					t.Fatalf("expected 0 agent starts (idle), got %d", len(cr.started))
				}
				if got.AgentSessionID != nil {
					t.Errorf("agent session id = %v, want nil (idle)", got.AgentSessionID)
				}
			}
		})
	}
}

// TestStartSession_HeadlessRunUsesSessionModel pins that a headless (detach) run
// passes the session's opaque Model id through to StartByAgent, so a session
// created with model=<opus id> runs `claude … --model <opus id>`.
func TestStartSession_HeadlessRunUsesSessionModel(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Test Session",
		Plan:       "Do the work",
		BaseBranch: "main",
		Model:      "claude-opus-4-8",
		State:      machine.CreatingWorktree,
	}

	lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), logger)

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{Detach: true}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	awaitDraftPR(t, lc, "sess-1")
	if len(cr.started) != 1 {
		t.Fatalf("expected 1 headless agent start, got %d", len(cr.started))
	}
	if cr.started[0].model != "claude-opus-4-8" {
		t.Errorf("headless run model = %q, want claude-opus-4-8", cr.started[0].model)
	}
}

func TestStartSession_HeadlessRunCarriesExplicitCapabilityProfile(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	runner := newMockAgentRunner()
	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", LocalPath: "/tmp/repo", DefaultBaseBranch: "main", WorktreeBaseDir: "/tmp/worktrees"}
	sessions.sessions["sess-1"] = &models.Session{ID: "sess-1", RepoID: "repo-1", Title: "Profiled", Plan: "not inferred from this text", BaseBranch: "main", AgentName: "codex", Model: "gpt-5-codex", State: machine.CreatingWorktree}
	lc := newTestLifecycle(sessions, repos, nil, nil, wt, runner, nil, newMockVCSProvider(), zerolog.Nop())
	lc.SetAccountEnvResolver(&fakeAccountEnvResolver{env: map[string]string{"CODEX_HOME": "/managed/codex-home"}})

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{
		Detach:                    true,
		HeadlessCapabilityProfile: pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1,
	}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	awaitDraftPR(t, lc, "sess-1")
	if len(runner.started) != 1 {
		t.Fatalf("headless starts = %d, want 1", len(runner.started))
	}
	if got := runner.started[0].profile; got != pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1 {
		t.Fatalf("HeadlessCapabilityProfile = %s, want tracker-plan-attachment-v1", got)
	}
	if runner.preflightCalls != 1 || len(runner.preflights) != 1 {
		t.Fatalf("preflight calls = %d, records = %d; want 1 each", runner.preflightCalls, len(runner.preflights))
	}
	if got := runner.preflights[0]; got.agentName != "codex" ||
		got.model != runner.started[0].model ||
		got.env["CODEX_HOME"] != runner.started[0].env["CODEX_HOME"] ||
		got.model != "gpt-5-codex" ||
		got.env["CODEX_HOME"] != "/managed/codex-home" {
		t.Fatalf("preflight/start target mismatch: preflight=%+v start=%+v", got, runner.started[0])
	}
}

func TestStartSession_ProfilePreflightFailsAfterSetup(t *testing.T) {
	t.Run("required profile rolls back the initialized worktree", func(t *testing.T) {
		ctx := context.Background()
		sessions := newMockSessionStore()
		repos := newMockRepoStore()
		setupCalls := 0
		worktrees := &mockWorktreeManager{onSetupScript: func() { setupCalls++ }}
		runner := newMockAgentRunner()
		runner.preflightErr = status.Error(codes.FailedPrecondition, "tracker-plan-attachment unavailable")
		provider := newMockVCSProvider()
		setupScript := "run setup"
		repos.repos["repo-1"] = &models.Repo{
			ID:                "repo-1",
			LocalPath:         "/tmp/repo",
			DefaultBaseBranch: "main",
			WorktreeBaseDir:   "/tmp/worktrees",
			SetupScript:       &setupScript,
		}
		sessions.sessions["sess-1"] = &models.Session{
			ID:         "sess-1",
			RepoID:     "repo-1",
			Title:      "Profiled",
			Plan:       "attach a tracker plan",
			BaseBranch: "main",
			AgentName:  "codex",
			Model:      "gpt-5-codex",
			State:      machine.CreatingWorktree,
		}
		lifecycle := newTestLifecycle(sessions, repos, nil, nil, worktrees, runner, nil, provider, zerolog.Nop())
		lifecycle.SetAccountEnvResolver(&fakeAccountEnvResolver{env: map[string]string{"CODEX_HOME": "/managed/codex-home"}})

		opts := StartSessionOpts{
			Detach:                    true,
			HeadlessCapabilityProfile: pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1,
		}
		err := lifecycle.StartSession(ctx, "sess-1", opts)

		if err == nil || !strings.Contains(err.Error(), "tracker-plan-attachment unavailable") {
			t.Fatalf("StartSession error = %v, want tracker-plan-attachment unavailable", err)
		}
		if runner.preflightCalls != 1 {
			t.Fatalf("preflight calls = %d, want 1", runner.preflightCalls)
		}
		if len(worktrees.created) != 1 || len(worktrees.createdFromExisting) != 0 {
			t.Fatalf("worktree calls = create %d, existing %d; want create 1", len(worktrees.created), len(worktrees.createdFromExisting))
		}
		if setupCalls != 1 {
			t.Fatalf("setup script calls = %d, want 1", setupCalls)
		}
		if len(worktrees.archived) != 1 {
			t.Fatalf("archived worktrees = %v, want one rollback", worktrees.archived)
		}
		if got := worktrees.deletedLocalBranches; !reflect.DeepEqual(got, []string{"test-session"}) {
			t.Fatalf("deleted local branches = %v, want [test-session]", got)
		}
		if len(runner.started) != 0 {
			t.Fatalf("agent starts = %d, want 0", len(runner.started))
		}
		if len(provider.createPRCalls) != 0 {
			t.Fatalf("draft PR calls = %d, want 0", len(provider.createPRCalls))
		}
		if updates := sessions.updatesFor("sess-1", "worktree_path"); len(updates) != 0 {
			t.Fatalf("worktree_path updates = %d, want 0", len(updates))
		}
		if len(sessions.updates) != 1 {
			t.Fatalf("session updates = %d, want creating state only", len(sessions.updates))
		}
	})

	// An unattended claude launch is the case that stays unprofiled: the launch
	// policy requires a profile only for codex, so claude keeps its historical
	// gate-free path byte-for-byte. (This subtest used to pin AgentName "codex"
	// with no explicit profile as also gate-free — the deliberate behaviour flip
	// moved that case to "policy-derived profile ..." below.)
	t.Run("unprofiled claude preserves normal creation and start", func(t *testing.T) {
		ctx := context.Background()
		sessions := newMockSessionStore()
		repos := newMockRepoStore()
		worktrees := &mockWorktreeManager{}
		runner := newMockAgentRunner()
		repos.repos["repo-1"] = &models.Repo{
			ID:                "repo-1",
			LocalPath:         "/tmp/repo",
			DefaultBaseBranch: "main",
			WorktreeBaseDir:   "/tmp/worktrees",
		}
		sessions.sessions["sess-1"] = &models.Session{
			ID:         "sess-1",
			RepoID:     "repo-1",
			Title:      "Unprofiled",
			Plan:       "normal claude run",
			BaseBranch: "main",
			AgentName:  "claude",
			State:      machine.CreatingWorktree,
		}
		lifecycle := newTestLifecycle(sessions, repos, nil, nil, worktrees, runner, nil, newMockVCSProvider(), zerolog.Nop())

		if err := lifecycle.StartSession(ctx, "sess-1", StartSessionOpts{Detach: true, DeferPR: true}); err != nil {
			t.Fatalf("StartSession: %v", err)
		}
		if runner.preflightCalls != 0 {
			t.Fatalf("preflight calls = %d, want 0", runner.preflightCalls)
		}
		if len(worktrees.created) != 1 || len(runner.started) != 1 {
			t.Fatalf("normal side effects = worktree %d, start %d; want 1 each", len(worktrees.created), len(runner.started))
		}
		if got := runner.started[0].profile; got != pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_UNSPECIFIED {
			t.Fatalf("claude headless start profile = %s, want unspecified", got)
		}
		if updates := sessions.updatesFor("sess-1", "worktree_path"); len(updates) == 0 {
			t.Fatal("worktree_path update missing")
		}
	})

	// The flip: an unattended codex launch with NO explicit profile now derives
	// TRACKER_PLAN_ATTACHMENT_V1 from the launch policy, so the preflight that
	// used to be dead on every production start actually runs.
	t.Run("policy-derived profile gates an unattended codex launch", func(t *testing.T) {
		ctx := context.Background()
		sessions := newMockSessionStore()
		repos := newMockRepoStore()
		worktrees := &mockWorktreeManager{}
		runner := newMockAgentRunner()
		repos.repos["repo-1"] = &models.Repo{
			ID:                "repo-1",
			LocalPath:         "/tmp/repo",
			DefaultBaseBranch: "main",
			WorktreeBaseDir:   "/tmp/worktrees",
		}
		sessions.sessions["sess-1"] = &models.Session{
			ID:         "sess-1",
			RepoID:     "repo-1",
			Title:      "Policy profiled",
			Plan:       "normal codex run",
			BaseBranch: "main",
			AgentName:  "codex",
			Model:      "gpt-5-codex",
			State:      machine.CreatingWorktree,
		}
		lifecycle := newTestLifecycle(sessions, repos, nil, nil, worktrees, runner, nil, newMockVCSProvider(), zerolog.Nop())

		if err := lifecycle.StartSession(ctx, "sess-1", StartSessionOpts{Detach: true, DeferPR: true}); err != nil {
			t.Fatalf("StartSession: %v", err)
		}
		if runner.preflightCalls != 1 || len(runner.preflights) != 1 {
			t.Fatalf("preflight calls = %d, records = %d; want 1 each", runner.preflightCalls, len(runner.preflights))
		}
		if got := runner.preflights[0]; got.agentName != "codex" ||
			got.profile != pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1 {
			t.Fatalf("preflight = %+v, want codex with tracker-plan-attachment-v1", got)
		}
		if len(worktrees.created) != 1 || len(runner.started) != 1 {
			t.Fatalf("normal side effects = worktree %d, start %d; want 1 each", len(worktrees.created), len(runner.started))
		}
		if got := runner.started[0].profile; got != pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1 {
			t.Fatalf("headless start profile = %s, want tracker-plan-attachment-v1", got)
		}
	})

	// Same post-setup rollback as the first subtest, but for a profile the
	// policy derived rather than one a caller passed.
	t.Run("policy-derived preflight failure rolls back initialized worktree", func(t *testing.T) {
		ctx := context.Background()
		sessions := newMockSessionStore()
		repos := newMockRepoStore()
		setupCalls := 0
		worktrees := &mockWorktreeManager{onSetupScript: func() { setupCalls++ }}
		runner := newMockAgentRunner()
		runner.preflightErr = status.Error(codes.FailedPrecondition, "tracker-plan-attachment unavailable")
		provider := newMockVCSProvider()
		setupScript := "run setup"
		repos.repos["repo-1"] = &models.Repo{
			ID:                "repo-1",
			LocalPath:         "/tmp/repo",
			DefaultBaseBranch: "main",
			WorktreeBaseDir:   "/tmp/worktrees",
			SetupScript:       &setupScript,
		}
		sessions.sessions["sess-1"] = &models.Session{
			ID:         "sess-1",
			RepoID:     "repo-1",
			Title:      "Policy profiled",
			Plan:       "attach a tracker plan",
			BaseBranch: "main",
			AgentName:  "codex",
			Model:      "gpt-5-codex",
			State:      machine.CreatingWorktree,
		}
		lifecycle := newTestLifecycle(sessions, repos, nil, nil, worktrees, runner, nil, provider, zerolog.Nop())

		err := lifecycle.StartSession(ctx, "sess-1", StartSessionOpts{Detach: true})

		if err == nil || !strings.Contains(err.Error(), "tracker-plan-attachment unavailable") {
			t.Fatalf("StartSession error = %v, want tracker-plan-attachment unavailable", err)
		}
		// Self-diagnosing failure: the error names the agent it gated and the
		// profile it required.
		if !strings.Contains(err.Error(), "codex") {
			t.Errorf("error %q does not name the agent", err.Error())
		}
		wantProfile := pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1.String()
		if !strings.Contains(err.Error(), wantProfile) {
			t.Errorf("error %q does not name the profile %s", err.Error(), wantProfile)
		}
		if runner.preflightCalls != 1 {
			t.Fatalf("preflight calls = %d, want 1", runner.preflightCalls)
		}
		if len(worktrees.created) != 1 || len(worktrees.createdFromExisting) != 0 {
			t.Fatalf("worktree calls = create %d, existing %d; want create 1", len(worktrees.created), len(worktrees.createdFromExisting))
		}
		if setupCalls != 1 {
			t.Fatalf("setup script calls = %d, want 1", setupCalls)
		}
		if len(worktrees.archived) != 1 {
			t.Fatalf("archived worktrees = %v, want one rollback", worktrees.archived)
		}
		if got := worktrees.deletedLocalBranches; !reflect.DeepEqual(got, []string{"test-session"}) {
			t.Fatalf("deleted local branches = %v, want [test-session]", got)
		}
		if len(runner.started) != 0 {
			t.Fatalf("agent starts = %d, want 0", len(runner.started))
		}
		if len(provider.createPRCalls) != 0 {
			t.Fatalf("draft PR calls = %d, want 0", len(provider.createPRCalls))
		}
		if updates := sessions.updatesFor("sess-1", "worktree_path"); len(updates) != 0 {
			t.Fatalf("worktree_path updates = %d, want 0", len(updates))
		}
		if len(sessions.updates) != 1 {
			t.Fatalf("session updates = %d, want creating state only", len(sessions.updates))
		}
	})

	t.Run("preserves supplied existing branch on rollback", func(t *testing.T) {
		ctx := context.Background()
		sessions := newMockSessionStore()
		repos := newMockRepoStore()
		worktrees := &mockWorktreeManager{}
		runner := newMockAgentRunner()
		runner.preflightErr = status.Error(codes.FailedPrecondition, "tracker-plan-attachment unavailable")
		repos.repos["repo-1"] = &models.Repo{ID: "repo-1", LocalPath: "/tmp/repo", DefaultBaseBranch: "main", WorktreeBaseDir: "/tmp/worktrees"}
		sessions.sessions["sess-1"] = &models.Session{ID: "sess-1", RepoID: "repo-1", Title: "Existing branch", Plan: "do work", BaseBranch: "main", AgentName: "codex", Model: "gpt-5-codex", State: machine.CreatingWorktree}
		lifecycle := newTestLifecycle(sessions, repos, nil, nil, worktrees, runner, nil, newMockVCSProvider(), zerolog.Nop())

		err := lifecycle.StartSession(ctx, "sess-1", StartSessionOpts{Detach: true, DeferPR: true, ExistingBranch: "feature/existing"})
		if err == nil || !strings.Contains(err.Error(), "tracker-plan-attachment unavailable") {
			t.Fatalf("StartSession error = %v, want tracker-plan-attachment unavailable", err)
		}
		if len(worktrees.archived) != 1 {
			t.Fatalf("archived worktrees = %v, want one rollback", worktrees.archived)
		}
		if len(worktrees.deletedLocalBranches) != 0 {
			t.Fatalf("deleted local branches = %v, want none for ExistingBranch", worktrees.deletedLocalBranches)
		}
	})

	t.Run("removes fresh branch created after existing branch fallback", func(t *testing.T) {
		ctx := context.Background()
		sessions := newMockSessionStore()
		repos := newMockRepoStore()
		worktrees := &mockWorktreeManager{createFromExistingBranchErr: errors.New("branch not found on remote")}
		runner := newMockAgentRunner()
		runner.preflightErr = status.Error(codes.FailedPrecondition, "tracker-plan-attachment unavailable")
		repos.repos["repo-1"] = &models.Repo{ID: "repo-1", LocalPath: "/tmp/repo", DefaultBaseBranch: "main", WorktreeBaseDir: "/tmp/worktrees"}
		sessions.sessions["sess-1"] = &models.Session{ID: "sess-1", RepoID: "repo-1", Title: "Fallback branch", Plan: "do work", BaseBranch: "main", AgentName: "codex", Model: "gpt-5-codex", State: machine.CreatingWorktree}
		lifecycle := newTestLifecycle(sessions, repos, nil, nil, worktrees, runner, nil, newMockVCSProvider(), zerolog.Nop())

		err := lifecycle.StartSession(ctx, "sess-1", StartSessionOpts{Detach: true, DeferPR: true, ExistingBranch: "feature/fallback"})
		if err == nil || !strings.Contains(err.Error(), "tracker-plan-attachment unavailable") {
			t.Fatalf("StartSession error = %v, want tracker-plan-attachment unavailable", err)
		}
		if len(worktrees.createdFromExisting) != 1 || len(worktrees.created) != 1 {
			t.Fatalf("worktree calls = existing %d, create %d; want 1 each", len(worktrees.createdFromExisting), len(worktrees.created))
		}
		if len(worktrees.archived) != 1 {
			t.Fatalf("archived worktrees = %v, want one rollback", worktrees.archived)
		}
		if got := worktrees.deletedLocalBranches; !reflect.DeepEqual(got, []string{"test-session"}) {
			t.Fatalf("deleted local branches = %v, want [test-session]", got)
		}
	})
}

// TestStartSession_AppliesHeadlessCapabilityProfilePolicy locks the launch
// policy where it actually matters: inside StartSession, over the real
// preflight seam, for every agent/interactivity combination that reaches the
// headless or idle branch. Only an unattended codex launch may reach the gate.
func TestStartSession_AppliesHeadlessCapabilityProfilePolicy(t *testing.T) {
	tests := []struct {
		name          string
		agentName     string
		opts          StartSessionOpts
		wantPreflight bool
		wantStarts    int
	}{
		{
			name:          "codex detach derives the tracker-plan-attachment profile",
			agentName:     "codex",
			opts:          StartSessionOpts{Detach: true, DeferPR: true},
			wantPreflight: true,
			wantStarts:    1,
		},
		{
			name:       "interactive codex stays unprofiled",
			agentName:  "codex",
			opts:       StartSessionOpts{DeferPR: true},
			wantStarts: 0,
		},
		{
			name:       "claude detach stays unprofiled",
			agentName:  "claude",
			opts:       StartSessionOpts{Detach: true, DeferPR: true},
			wantStarts: 1,
		},
		{
			name:       "interactive claude stays unprofiled",
			agentName:  "claude",
			opts:       StartSessionOpts{DeferPR: true},
			wantStarts: 0,
		},
		{
			// mockAgentRunner is not an agent.AgentNameResolver, so the
			// resolved name is the raw empty string: an unresolved agent must
			// never opt a launch into a gate its runner may not implement.
			name:       "unresolved empty agent name stays unprofiled",
			agentName:  "",
			opts:       StartSessionOpts{Detach: true, DeferPR: true},
			wantStarts: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			sessions := newMockSessionStore()
			repos := newMockRepoStore()
			worktrees := &mockWorktreeManager{}
			runner := newMockAgentRunner()
			repos.repos["repo-1"] = &models.Repo{
				ID:                "repo-1",
				LocalPath:         "/tmp/repo",
				DefaultBaseBranch: "main",
				WorktreeBaseDir:   "/tmp/worktrees",
			}
			sessions.sessions["sess-1"] = &models.Session{
				ID:         "sess-1",
				RepoID:     "repo-1",
				Title:      "Policy",
				Plan:       "do work",
				BaseBranch: "main",
				AgentName:  tc.agentName,
				Model:      "some-model",
				State:      machine.CreatingWorktree,
			}
			lifecycle := newTestLifecycle(sessions, repos, &mockAgentChatStore{}, nil, worktrees, runner, nil, newMockVCSProvider(), zerolog.Nop())

			if err := lifecycle.StartSession(ctx, "sess-1", tc.opts); err != nil {
				t.Fatalf("StartSession: %v", err)
			}

			wantCalls := 0
			if tc.wantPreflight {
				wantCalls = 1
			}
			if runner.preflightCalls != wantCalls || len(runner.preflights) != wantCalls {
				t.Fatalf("preflight calls = %d, records = %d; want %d each", runner.preflightCalls, len(runner.preflights), wantCalls)
			}
			if tc.wantPreflight {
				got := runner.preflights[0]
				if got.agentName != tc.agentName {
					t.Errorf("preflight agent = %q, want %q", got.agentName, tc.agentName)
				}
				if got.profile != pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1 {
					t.Errorf("preflight profile = %s, want tracker-plan-attachment-v1", got.profile)
				}
			}
			if len(runner.started) != tc.wantStarts {
				t.Fatalf("agent starts = %d, want %d", len(runner.started), tc.wantStarts)
			}
			if tc.wantStarts == 1 {
				wantProfile := pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_UNSPECIFIED
				if tc.wantPreflight {
					wantProfile = pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1
				}
				if got := runner.started[0].profile; got != wantProfile {
					t.Errorf("headless start profile = %s, want %s", got, wantProfile)
				}
			}
		})
	}
}

// TestStartSession_AppliesCapabilityProfilePolicyOnTmuxHostedLaunch covers the
// two unattended markers that route away from the headless branch into the
// durable tmux-hosted path (IsTmuxUnattended and CronJobID). Such a launch is
// checked twice against the post-setup worktree environment: once before the
// session records it and again before tmux starts.
func TestStartSession_AppliesCapabilityProfilePolicyOnTmuxHostedLaunch(t *testing.T) {
	tests := []struct {
		name string
		opts StartSessionOpts
	}{
		{name: "tmux unattended", opts: StartSessionOpts{DeferPR: true, IsTmuxUnattended: true, HookToken: "tok-1"}},
		{name: "cron", opts: StartSessionOpts{DeferPR: true, CronJobID: "cron-1", HookToken: "tok-1"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			worktreeDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(worktreeDir, ".env"), []byte("CODEX_HOME=.codex\nHOME=home\n"), 0o600); err != nil {
				t.Fatalf("write worktree .env: %v", err)
			}
			// The registered checkout deliberately disagrees with the worktree;
			// neither check may use it as the launch environment.
			repoDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(repoDir, ".env"), []byte("CODEX_HOME=/repo/codex-home\nHOME=/repo/home\n"), 0o600); err != nil {
				t.Fatalf("write repo .env: %v", err)
			}

			sessions := newMockSessionStore()
			repos := newMockRepoStore()
			chats := &mockAgentChatStore{}
			worktrees := &mockWorktreeManager{worktreePath: worktreeDir}
			runner := newMockAgentRunner()
			tx := tmux.NewClient(tmux.WithCommandFactory(newFakeTmux().factory))

			repos.repos["repo-1"] = &models.Repo{
				ID:                "repo-1",
				LocalPath:         repoDir,
				DefaultBaseBranch: "main",
				WorktreeBaseDir:   "/tmp/worktrees",
				OriginURL:         "owner/repo",
			}
			sessions.sessions["sess-1"] = &models.Session{
				ID:         "sess-1",
				RepoID:     "repo-1",
				Title:      "Unattended codex",
				Plan:       "do work",
				BaseBranch: "main",
				AgentName:  "codex",
				Model:      "gpt-5-codex",
				State:      machine.CreatingWorktree,
			}

			client := newFakeAgent()
			lifecycle := newTestLifecycle(sessions, repos, chats, &stubCronJobStore{}, worktrees, runner, tx, newMockVCSProvider(), zerolog.Nop())
			lifecycle.newTmuxChatAgentSessionID = func() string { return "agent-1" }
			lifecycle.SetHookPort(45678)
			lifecycle.SetAgents(map[string]agent.AgentRunnerClient{"codex": client})
			lifecycle.SetAgentLogsDir(t.TempDir())
			lifecycle.SetPollArmer(&fakePollArmer{})
			lifecycle.SetDaemonCtx(ctx)

			if err := lifecycle.StartSession(ctx, "sess-1", tc.opts); err != nil {
				t.Fatalf("StartSession: %v", err)
			}

			// Both checks use the post-setup worktree environment.
			if runner.preflightCalls != 2 || len(runner.preflights) != 2 {
				t.Fatalf("preflight calls = %d, records = %d; want 2 each", runner.preflightCalls, len(runner.preflights))
			}
			for i, got := range runner.preflights {
				if got.agentName != "codex" {
					t.Errorf("preflight[%d] agent = %q, want codex", i, got.agentName)
				}
				if got.profile != pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1 {
					t.Errorf("preflight[%d] profile = %s, want tracker-plan-attachment-v1", i, got.profile)
				}
				if got.model != "gpt-5-codex" {
					t.Errorf("preflight[%d] model = %q, want gpt-5-codex", i, got.model)
				}
			}
			if got := runner.preflights[0]; got.env["CODEX_HOME"] != filepath.Join(worktreeDir, ".codex") || got.env["HOME"] != filepath.Join(worktreeDir, "home") {
				t.Errorf("post-setup preflight env = %v, want worktree CODEX_HOME and HOME", got.env)
			}
			if got := runner.preflights[1]; got.env["CODEX_HOME"] != filepath.Join(worktreeDir, ".codex") || got.env["HOME"] != filepath.Join(worktreeDir, "home") {
				t.Errorf("tmux preflight env = %v, want worktree CODEX_HOME and HOME", got.env)
			}
		})
	}
}

// TestStartSession_ProfilePreflightUsesWorktreeEnvAfterSetup proves a setup
// script can replace the registered checkout's CODEX_HOME before preflight.
func TestStartSession_ProfilePreflightUsesWorktreeEnvAfterSetup(t *testing.T) {
	ctx := context.Background()
	repoDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoDir, ".env"), []byte("CODEX_HOME=/repo/codex-home\n"), 0o600); err != nil {
		t.Fatalf("write repo .env: %v", err)
	}
	worktreeDir := t.TempDir()
	setupScript := "replace worktree env"
	var setupWriteErr error

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	worktrees := &mockWorktreeManager{
		worktreePath: worktreeDir,
		onSetupScript: func() {
			setupWriteErr = os.WriteFile(filepath.Join(worktreeDir, ".env"), []byte("CODEX_HOME=.codex\nHOME=home\n"), 0o600)
		},
	}
	runner := newMockAgentRunner()
	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         repoDir,
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
		SetupScript:       &setupScript,
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Unmanaged codex",
		Plan:       "do work",
		BaseBranch: "main",
		AgentName:  "codex",
		Model:      "gpt-5-codex",
		State:      machine.CreatingWorktree,
	}
	lifecycle := newTestLifecycle(sessions, repos, nil, nil, worktrees, runner, nil, newMockVCSProvider(), zerolog.Nop())

	if err := lifecycle.StartSession(ctx, "sess-1", StartSessionOpts{Detach: true, DeferPR: true}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if setupWriteErr != nil {
		t.Fatalf("write setup .env: %v", setupWriteErr)
	}
	if runner.preflightCalls != 1 || len(runner.preflights) != 1 {
		t.Fatalf("preflight calls = %d, records = %d; want 1 each", runner.preflightCalls, len(runner.preflights))
	}
	if got := runner.preflights[0]; got.env["CODEX_HOME"] != filepath.Join(worktreeDir, ".codex") || got.env["HOME"] != filepath.Join(worktreeDir, "home") {
		t.Errorf("preflight env = %v, want relative homes resolved from the setup worktree", got.env)
	}
	if got := runner.started[0]; got.env["CODEX_HOME"] != filepath.Join(worktreeDir, ".codex") || got.env["HOME"] != filepath.Join(worktreeDir, "home") {
		t.Errorf("headless start env = %v, want relative homes resolved from the setup worktree", got.env)
	}
}

func TestStartSession_ProfilePreflightRollbackUsesLiveCleanupContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	worktrees := &mockWorktreeManager{onSetupScript: cancel}
	runner := newMockAgentRunner()
	runner.preflightErr = status.Error(codes.FailedPrecondition, "tracker-plan-attachment unavailable")
	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", LocalPath: "/tmp/repo", DefaultBaseBranch: "main", WorktreeBaseDir: "/tmp/worktrees"}
	sessions.sessions["sess-1"] = &models.Session{
		ID: "sess-1", RepoID: "repo-1", Title: "Profiled", Plan: "do work", BaseBranch: "main",
		AgentName: "codex", Model: "gpt-5-codex", State: machine.CreatingWorktree,
	}
	lifecycle := newTestLifecycle(sessions, repos, nil, nil, worktrees, runner, nil, newMockVCSProvider(), zerolog.Nop())

	err := lifecycle.StartSession(ctx, "sess-1", StartSessionOpts{
		Detach:                    true,
		DeferPR:                   true,
		HeadlessCapabilityProfile: pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1,
	})
	if err == nil {
		t.Fatal("StartSession succeeded, want profile preflight failure")
	}
	if !worktrees.archiveCtxLive {
		t.Fatalf("Archive context = %v, want live cleanup context", worktrees.archiveCtx)
	}
	if !worktrees.deleteLocalBranchCtxLive {
		t.Fatalf("DeleteLocalBranch context = %v, want live cleanup context", worktrees.deleteLocalBranchCtx)
	}
}

// TestStartSession_TmuxHostedProfilePreflightRollsBackWorktree pins that a
// tmux-hosted launch validates post-setup and cleans up on failure.
func TestStartSession_TmuxHostedProfilePreflightRollsBackWorktree(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	setupCalls := 0
	worktrees := &mockWorktreeManager{onSetupScript: func() { setupCalls++ }}
	runner := newMockAgentRunner()
	runner.preflightErr = status.Error(codes.FailedPrecondition, "tracker-plan-attachment unavailable")
	provider := newMockVCSProvider()
	setupScript := "run setup"
	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
		SetupScript:       &setupScript,
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Cron codex",
		Plan:       "do work",
		BaseBranch: "main",
		AgentName:  "codex",
		Model:      "gpt-5-codex",
		State:      machine.CreatingWorktree,
	}
	lifecycle := newTestLifecycle(sessions, repos, nil, nil, worktrees, runner, nil, provider, zerolog.Nop())

	// CronJobID alone makes this tmux-hosted, independent of tmux availability.
	err := lifecycle.StartSession(ctx, "sess-1", StartSessionOpts{CronJobID: "cron-1", DeferPR: true, HookToken: "tok-1"})

	if err == nil || !strings.Contains(err.Error(), "tracker-plan-attachment unavailable") {
		t.Fatalf("StartSession error = %v, want tracker-plan-attachment unavailable", err)
	}
	if runner.preflightCalls != 1 {
		t.Fatalf("preflight calls = %d, want 1", runner.preflightCalls)
	}
	if len(worktrees.created) != 1 || len(worktrees.createdFromExisting) != 0 {
		t.Fatalf("worktree calls = create %d, existing %d; want create 1", len(worktrees.created), len(worktrees.createdFromExisting))
	}
	if setupCalls != 1 {
		t.Fatalf("setup script calls = %d, want 1", setupCalls)
	}
	if len(worktrees.archived) != 1 {
		t.Fatalf("archived worktrees = %v, want one rollback", worktrees.archived)
	}
	if len(provider.createPRCalls) != 0 {
		t.Fatalf("draft PR calls = %d, want 0", len(provider.createPRCalls))
	}
	if len(sessions.updates) != 1 {
		t.Fatalf("session updates = %d, want creating state only", len(sessions.updates))
	}
}

// TestStartSession_TmuxChatProfilePreflightRollsBackWorktree pins the second,
// authoritative probe. StartSession runs the capability preflight twice: once
// post-setup (covered above) and again inside startTmuxChat, which is the one
// that decides the launch when the two can disagree. That second probe runs
// after the worktree and branch have been persisted, and the cron caller drops
// only the session row on error, so a rejection there must roll both back too —
// otherwise the next fire collides with the stranded branch.
//
// preflightErrFromCall = 2 is what makes this distinct from the test above: the
// post-setup probe passes, so the launch only fails once it reaches the probe
// in startTmuxChat.
func TestStartSession_TmuxChatProfilePreflightRollsBackWorktree(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	worktreeDir := t.TempDir()
	worktrees := &mockWorktreeManager{worktreePath: worktreeDir}
	runner := newMockAgentRunner()
	runner.preflightErr = status.Error(codes.FailedPrecondition, "tracker-plan-attachment unavailable")
	runner.preflightErrFromCall = 2
	provider := newMockVCSProvider()
	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Cron codex",
		Plan:       "do work",
		BaseBranch: "main",
		AgentName:  "codex",
		Model:      "gpt-5-codex",
		State:      machine.CreatingWorktree,
	}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(newFakeTmux().factory))
	lifecycle := newTestLifecycle(sessions, repos, &mockAgentChatStore{}, nil, worktrees, runner, tmuxClient, provider, zerolog.Nop())
	lifecycle.SetAgents(map[string]agent.AgentRunnerClient{"codex": newFakeAgent()})
	lifecycle.SetAgentLogsDir(t.TempDir())

	// HookToken is left empty so the Stop-hook configuration step is skipped;
	// this test is about the preflight, not hook wiring.
	err := lifecycle.StartSession(ctx, "sess-1", StartSessionOpts{CronJobID: "cron-1", DeferPR: true})

	if err == nil || !strings.Contains(err.Error(), "tracker-plan-attachment unavailable") {
		t.Fatalf("StartSession error = %v, want tracker-plan-attachment unavailable", err)
	}
	if runner.preflightCalls != 2 {
		t.Fatalf("preflight calls = %d, want 2 (post-setup then startTmuxChat)", runner.preflightCalls)
	}
	if len(worktrees.archived) != 1 || worktrees.archived[0] != worktreeDir {
		t.Fatalf("archived worktrees = %v, want one rollback of %q", worktrees.archived, worktreeDir)
	}
	if !slices.Contains(worktrees.deletedLocalBranches, "test-session") {
		t.Fatalf("deleted local branches = %v, want the created branch rolled back", worktrees.deletedLocalBranches)
	}
	if len(provider.createPRCalls) != 0 {
		t.Fatalf("draft PR calls = %d, want 0", len(provider.createPRCalls))
	}
}

// TestStartSession_ExplicitCapabilityProfileSurvivesPolicy pins that an
// explicitly-passed profile wins over the policy's verdict: claude + detach
// derives UNSPECIFIED, yet an explicit V1 still reaches the preflight and the
// profiled start untouched.
func TestStartSession_ExplicitCapabilityProfileSurvivesPolicy(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	worktrees := &mockWorktreeManager{}
	runner := newMockAgentRunner()
	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Explicit override",
		Plan:       "do work",
		BaseBranch: "main",
		AgentName:  "claude",
		Model:      "claude-opus-4-8",
		State:      machine.CreatingWorktree,
	}
	lifecycle := newTestLifecycle(sessions, repos, nil, nil, worktrees, runner, nil, newMockVCSProvider(), zerolog.Nop())

	if err := lifecycle.StartSession(ctx, "sess-1", StartSessionOpts{
		Detach:                    true,
		DeferPR:                   true,
		HeadlessCapabilityProfile: pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1,
	}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	if runner.preflightCalls != 1 || len(runner.preflights) != 1 {
		t.Fatalf("preflight calls = %d, records = %d; want 1 each", runner.preflightCalls, len(runner.preflights))
	}
	if got := runner.preflights[0]; got.agentName != "claude" ||
		got.profile != pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1 {
		t.Fatalf("preflight = %+v, want claude with tracker-plan-attachment-v1", got)
	}
	if len(runner.started) != 1 {
		t.Fatalf("agent starts = %d, want 1", len(runner.started))
	}
	if got := runner.started[0].profile; got != pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1 {
		t.Fatalf("headless start profile = %s, want tracker-plan-attachment-v1", got)
	}
}

// TestStartSession_NewPR_AwaitsManualStart pins the fix for the "headless agent
// run failed" bug: a plain interactive new-PR session (no TrackerID, no
// CronJobID, Detach=false) must NOT auto-fire a headless `claude --print` run.
// It lands idle in ImplementingPlan and starts the agent on first attach.
func TestStartSession_NewPR_AwaitsManualStart(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Test Session",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
	}

	lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), logger)

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	awaitDraftPR(t, lc, "sess-1")

	if len(cr.started) != 0 {
		t.Fatalf("expected 0 agent starts for interactive new-PR session, got %d", len(cr.started))
	}
	sess := sessions.sessions["sess-1"]
	if sess.State != machine.ImplementingPlan {
		t.Errorf("session state = %v, want ImplementingPlan", sess.State)
	}
	if sess.AgentSessionID != nil {
		t.Errorf("agent session id = %v, want nil (idle)", sess.AgentSessionID)
	}
}

// TestStartSession_ExistingPR_AwaitsManualStart is the same as the new-PR case
// but for the "Work on an existing PR" flow (PRNumber set). It must also stay
// idle rather than firing the doomed empty-prompt headless run.
func TestStartSession_ExistingPR_AwaitsManualStart(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}
	prNumber := 42
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Work on existing PR",
		BaseBranch: "main",
		PRNumber:   &prNumber,
		State:      machine.CreatingWorktree,
	}

	lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), logger)

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{ExistingBranch: "feature/pr-42"}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	awaitDraftPR(t, lc, "sess-1")

	if len(cr.started) != 0 {
		t.Fatalf("expected 0 agent starts for interactive existing-PR session, got %d", len(cr.started))
	}
	sess := sessions.sessions["sess-1"]
	if sess.State != machine.ImplementingPlan {
		t.Errorf("session state = %v, want ImplementingPlan", sess.State)
	}
	if sess.AgentSessionID != nil {
		t.Errorf("agent session id = %v, want nil (idle)", sess.AgentSessionID)
	}
}

func TestStartSession_DraftPRNotAttemptedWhenBranchHasNoCommitsOverBase(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{
		verifyPushedErr: fmt.Errorf("head branch test-session has no commits over base origin/main (base SHA base123, head SHA head123)"),
	}
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		OriginURL:         "owner/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Test Session",
		Plan:       "Do something",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
	}

	lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, vp, logger)

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	awaitDraftPR(t, lc, "sess-1")

	if len(vp.createPRCalls) != 0 {
		t.Fatalf("CreateDraftPR calls = %d, want 0", len(vp.createPRCalls))
	}
	if len(wt.verifyPushedCalls) != 1 {
		t.Fatalf("VerifyPushedBranchAheadOfBase calls = %d, want 1", len(wt.verifyPushedCalls))
	}

	reason := sessions.sessions["sess-1"].BlockedReason
	if reason == nil {
		t.Fatal("BlockedReason = nil, want branch invariant reason")
	}
	for _, want := range []string{
		"head branch test-session has no commits over base origin/main",
		"base SHA base123",
		"head SHA head123",
	} {
		if !strings.Contains(*reason, want) {
			t.Fatalf("BlockedReason = %q, want %q", *reason, want)
		}
	}
}

func TestStartSession_NoCommitsBetweenProviderErrorIncludesBranchSHAs(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{
		verifyPushedResult: &gitpkg.BranchVerification{
			HeadSHA:       "head456",
			BaseSHA:       "base123",
			RemoteHeadSHA: "head456",
			AheadCount:    1,
		},
	}
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	vp.createPRErr = fmt.Errorf("gh pr create: GraphQL: No commits between main and test-session (createPullRequest)")
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		OriginURL:         "owner/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Test Session",
		Plan:       "Do something",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
	}

	lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, vp, logger)

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	awaitDraftPR(t, lc, "sess-1")

	if len(vp.createPRCalls) != 1 {
		t.Fatalf("CreateDraftPR calls = %d, want 1", len(vp.createPRCalls))
	}
	reason := sessions.sessions["sess-1"].BlockedReason
	if reason == nil {
		t.Fatal("BlockedReason = nil, want No commits between reason")
	}
	for _, want := range []string{
		"head branch test-session has no commits over base origin/main",
		"base SHA base123",
		"head SHA head456",
		"No commits between main and test-session",
	} {
		if !strings.Contains(*reason, want) {
			t.Fatalf("BlockedReason = %q, want %q", *reason, want)
		}
	}
}

func TestStartSession_DuplicatePRErrorAttachesExistingPRAndClearsBlockedReason(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		OriginURL:         "git@github.com:owner/repo.git",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}
	reason := "draft PR creation failed: create draft PR: " + vcs.ErrPRAlreadyExists.Error()
	sessions.sessions["sess-1"] = &models.Session{
		ID:            "sess-1",
		RepoID:        "repo-1",
		Title:         "Test Session",
		Plan:          "Do something",
		BaseBranch:    "main",
		State:         machine.CreatingWorktree,
		BlockedReason: &reason,
	}
	vp.createPRErr = vcs.ErrPRAlreadyExists
	vp.nextOpenPRs = []vcs.PRSummary{
		{
			Number:     77,
			Title:      "Test Session",
			HeadBranch: "test-session",
			State:      vcs.PRStateOpen,
		},
	}

	lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, vp, logger)

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	awaitDraftPR(t, lc, "sess-1")

	sess := sessions.sessions["sess-1"]
	if sess.PRNumber == nil || *sess.PRNumber != 77 {
		t.Fatalf("PRNumber = %v, want 77", sess.PRNumber)
	}
	if sess.PRURL == nil || *sess.PRURL != "https://github.com/owner/repo/pull/77" {
		t.Fatalf("PRURL = %v, want https://github.com/owner/repo/pull/77", sess.PRURL)
	}
	if sess.BlockedReason != nil {
		t.Fatalf("BlockedReason = %q, want nil", *sess.BlockedReason)
	}
	if len(vp.createPRCalls) != 1 {
		t.Fatalf("CreateDraftPR calls = %d, want 1", len(vp.createPRCalls))
	}
	if len(vp.listOpenPRCalls) != 1 || vp.listOpenPRCalls[0] != "git@github.com:owner/repo.git" {
		t.Fatalf("ListOpenPRs calls = %v, want [git@github.com:owner/repo.git]", vp.listOpenPRCalls)
	}
}

func TestStartSession_CronPRTitleNormalizesSessionTitleWhenLatestCommitSubjectFails(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	chats := &mockAgentChatStore{}
	wt := new(mockWorktreeManager)
	wt.latestCommitSubjectErr = errors.New("git log failed")
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	tx := tmux.NewClient(tmux.WithCommandFactory(newFakeTmux().factory))

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		OriginURL:         "git@github.com:owner/repo.git",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "test(githubapp): [#493] cover malformed Link header without brackets",
		Plan:       "Do something",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
		AgentName:  "claude",
	}

	lc := newTestLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, tx, vp, logger)
	lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": newFakeAgent()})
	lc.SetAgentLogsDir(t.TempDir())

	cronJobID := "cron-42"
	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{CronJobID: cronJobID}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	awaitDraftPR(t, lc, "sess-1")

	if len(vp.createPRCalls) != 1 {
		t.Fatalf("CreateDraftPR calls = %d, want 1", len(vp.createPRCalls))
	}
	if got, want := vp.createPRCalls[0].Title, "Cover malformed Link header without brackets"; got != want {
		t.Fatalf("PR title = %q, want %q", got, want)
	}
}

func TestStartSession_ExistingBranchNotOnRemote_FallsBackToCreate(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{
		createFromExistingBranchErr: fmt.Errorf("fetch branch: git fetch origin dave/fre-1176: exit status 128: fatal: couldn't find remote ref dave/fre-1176"),
	}
	cr := newMockAgentRunner()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "FRE-1176 Fix login bug",
		Plan:       "Fix the bug",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
	}

	lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), logger)

	// Pass a branch name that doesn't exist on the remote.
	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{ExistingBranch: "dave/fre-1176"}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	awaitDraftPR(t, lc, "sess-1")

	// Should have tried CreateFromExistingBranch first.
	if len(wt.createdFromExisting) != 1 {
		t.Fatalf("expected 1 CreateFromExistingBranch call, got %d", len(wt.createdFromExisting))
	}

	// Should have fallen back to Create with the branch name.
	if len(wt.created) != 1 {
		t.Fatalf("expected 1 Create call (fallback), got %d", len(wt.created))
	}
	if wt.created[0].BranchName != "dave/fre-1176" {
		t.Errorf("Create BranchName = %q, want dave/fre-1176", wt.created[0].BranchName)
	}
	if wt.created[0].BaseBranch != "main" {
		t.Errorf("Create BaseBranch = %q, want main", wt.created[0].BaseBranch)
	}
}

func TestStopSession(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	logger := zerolog.Nop()

	agentSessionID := "claude-123"
	cr.running[agentSessionID] = true

	sessions.sessions["sess-1"] = &models.Session{
		ID:             "sess-1",
		RepoID:         "repo-1",
		State:          machine.ImplementingPlan,
		AgentSessionID: &agentSessionID,
	}

	lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), logger)

	if err := lc.StopSession(ctx, "sess-1"); err != nil {
		t.Fatalf("StopSession: %v", err)
	}

	// Verify Claude was stopped.
	if len(cr.stopped) != 1 || cr.stopped[0] != "claude-123" {
		t.Errorf("expected claude-123 stopped, got %v", cr.stopped)
	}

	// Verify state is Closed.
	if sessions.sessions["sess-1"].State != machine.Closed {
		t.Errorf("state = %v, want Closed", sessions.sessions["sess-1"].State)
	}
}

func TestArchiveSession(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo",
	}

	agentSessionID := "claude-123"
	cr.running[agentSessionID] = true

	sessions.sessions["sess-1"] = &models.Session{
		ID:             "sess-1",
		RepoID:         "repo-1",
		State:          machine.ImplementingPlan,
		WorktreePath:   "/tmp/worktrees/test-repo/test",
		AgentSessionID: &agentSessionID,
	}

	lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), logger)

	if err := lc.ArchiveSession(ctx, "sess-1"); err != nil {
		t.Fatalf("ArchiveSession: %v", err)
	}

	// Verify Claude was stopped.
	if len(cr.stopped) != 1 {
		t.Errorf("expected 1 claude stop, got %d", len(cr.stopped))
	}

	// Verify worktree was archived.
	if len(wt.archived) != 1 || wt.archived[0] != "/tmp/worktrees/test-repo/test" {
		t.Errorf("expected worktree archived at /tmp/worktrees/test-repo/test, got %v", wt.archived)
	}
}

func TestArchiveSessionDeletesLocalBranch(t *testing.T) {
	// buildLifecycle wires a lifecycle around a session on branch "feature" whose
	// worktree differs from the repo local path (the non-quick-chat path).
	buildLifecycle := func(t *testing.T, canAutoDelete, safe bool, deleteErr error, worktreePath string) (*Lifecycle, *mockWorktreeManager) {
		t.Helper()
		sessions := newMockSessionStore()
		repos := newMockRepoStore()
		wt := &mockWorktreeManager{branchSafeToDelete: safe, deleteLocalBranchErr: deleteErr}
		cr := newMockAgentRunner()

		repos.repos["repo-1"] = &models.Repo{
			ID:                    "repo-1",
			LocalPath:             "/tmp/repo",
			CanAutoDeleteBranches: canAutoDelete,
		}
		sessions.sessions["sess-1"] = &models.Session{
			ID:           "sess-1",
			RepoID:       "repo-1",
			State:        machine.ImplementingPlan,
			WorktreePath: worktreePath,
			BranchName:   "feature",
			BaseBranch:   "main",
		}

		lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), zerolog.Nop())
		return lc, wt
	}

	t.Run("flag on and branch safe deletes the branch", func(t *testing.T) {
		lc, wt := buildLifecycle(t, true, true, nil, "/tmp/worktrees/test-repo/test")
		if err := lc.ArchiveSession(context.Background(), "sess-1"); err != nil {
			t.Fatalf("ArchiveSession: %v", err)
		}
		if len(wt.deletedLocalBranches) != 1 || wt.deletedLocalBranches[0] != "feature" {
			t.Errorf("expected DeleteLocalBranch once with %q, got %v", "feature", wt.deletedLocalBranches)
		}
	})

	t.Run("flag off never deletes", func(t *testing.T) {
		lc, wt := buildLifecycle(t, false, true, nil, "/tmp/worktrees/test-repo/test")
		if err := lc.ArchiveSession(context.Background(), "sess-1"); err != nil {
			t.Fatalf("ArchiveSession: %v", err)
		}
		if len(wt.deletedLocalBranches) != 0 {
			t.Errorf("expected no DeleteLocalBranch, got %v", wt.deletedLocalBranches)
		}
	})

	t.Run("branch unsafe never deletes", func(t *testing.T) {
		lc, wt := buildLifecycle(t, true, false, nil, "/tmp/worktrees/test-repo/test")
		if err := lc.ArchiveSession(context.Background(), "sess-1"); err != nil {
			t.Fatalf("ArchiveSession: %v", err)
		}
		if len(wt.deletedLocalBranches) != 0 {
			t.Errorf("expected no DeleteLocalBranch for unsafe branch, got %v", wt.deletedLocalBranches)
		}
	})

	t.Run("quick chat session never deletes", func(t *testing.T) {
		// Quick chat: worktree path equals the repo local path, so the whole
		// archive+delete block is skipped.
		lc, wt := buildLifecycle(t, true, true, nil, "/tmp/repo")
		if err := lc.ArchiveSession(context.Background(), "sess-1"); err != nil {
			t.Fatalf("ArchiveSession: %v", err)
		}
		if len(wt.deletedLocalBranches) != 0 {
			t.Errorf("expected no DeleteLocalBranch for quick chat, got %v", wt.deletedLocalBranches)
		}
		if len(wt.archived) != 0 {
			t.Errorf("expected no worktree archive for quick chat, got %v", wt.archived)
		}
	})

	t.Run("delete failure does not fail archive", func(t *testing.T) {
		lc, wt := buildLifecycle(t, true, true, errors.New("boom"), "/tmp/worktrees/test-repo/test")
		if err := lc.ArchiveSession(context.Background(), "sess-1"); err != nil {
			t.Fatalf("ArchiveSession should swallow delete errors, got: %v", err)
		}
		if len(wt.deletedLocalBranches) != 1 {
			t.Errorf("expected DeleteLocalBranch attempted once, got %v", wt.deletedLocalBranches)
		}
	})

	t.Run("safe-check error keeps branch and does not fail archive", func(t *testing.T) {
		// BranchSafeToDelete itself erroring is a best-effort no-op: the archive
		// has already succeeded, so the branch is kept and no error propagates.
		lc, wt := buildLifecycle(t, true, true, nil, "/tmp/worktrees/test-repo/test")
		wt.branchSafeToDeleteErr = errors.New("git blew up")
		if err := lc.ArchiveSession(context.Background(), "sess-1"); err != nil {
			t.Fatalf("ArchiveSession should swallow predicate errors, got: %v", err)
		}
		if len(wt.deletedLocalBranches) != 0 {
			t.Errorf("expected no DeleteLocalBranch when safe-check errors, got %v", wt.deletedLocalBranches)
		}
	})

	t.Run("base branch is never deleted", func(t *testing.T) {
		// A session whose BranchName equals its BaseBranch would pass the
		// self-ancestor safe check; only the base-branch guard prevents the
		// destructive `git branch -D <base>`. BranchSafeToDelete must never even
		// be consulted here.
		sessions := newMockSessionStore()
		repos := newMockRepoStore()
		wt := &mockWorktreeManager{branchSafeToDelete: true}
		cr := newMockAgentRunner()
		repos.repos["repo-1"] = &models.Repo{
			ID:                    "repo-1",
			LocalPath:             "/tmp/repo",
			DefaultBaseBranch:     "main",
			CanAutoDeleteBranches: true,
		}
		sessions.sessions["sess-1"] = &models.Session{
			ID:           "sess-1",
			RepoID:       "repo-1",
			State:        machine.ImplementingPlan,
			WorktreePath: "/tmp/worktrees/test-repo/test",
			BranchName:   "main",
			BaseBranch:   "main",
		}
		lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), zerolog.Nop())
		if err := lc.ArchiveSession(context.Background(), "sess-1"); err != nil {
			t.Fatalf("ArchiveSession: %v", err)
		}
		if len(wt.deletedLocalBranches) != 0 {
			t.Errorf("expected the base branch to be kept, got deletes %v", wt.deletedLocalBranches)
		}
	})
}

func TestResurrectSession(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo",
	}

	now := func() *models.Session {
		return sessions.sessions["sess-1"]
	}

	archivedAt := func() *models.Session {
		s := &models.Session{
			ID:           "sess-1",
			RepoID:       "repo-1",
			Title:        "Test Session",
			Plan:         "Do something",
			WorktreePath: "/tmp/worktrees/test-repo/test",
			BranchName:   "test",
			BaseBranch:   "main",
			State:        machine.ImplementingPlan,
		}
		// Set ArchivedAt to mark as archived.
		t := now // just need a non-nil value
		_ = t
		return s
	}

	// Create an archived session.
	archivedTime := new(struct{}) // placeholder
	_ = archivedTime

	sess := archivedAt()
	// Actually set ArchivedAt.
	archTime := sess.CreatedAt // zero time, but non-nil pointer
	sess.ArchivedAt = &archTime
	sessions.sessions["sess-1"] = sess

	oldClaudeID := "claude-old"
	sess.AgentSessionID = &oldClaudeID

	lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), logger)

	if err := lc.ResurrectSession(ctx, "sess-1"); err != nil {
		t.Fatalf("ResurrectSession: %v", err)
	}

	// Verify worktree was resurrected.
	if len(wt.resurrected) != 1 {
		t.Fatalf("expected 1 resurrect call, got %d", len(wt.resurrected))
	}
	if wt.resurrected[0].BranchName != "test" {
		t.Errorf("resurrect branch = %q, want test", wt.resurrected[0].BranchName)
	}
	if wt.resurrected[0].BaseBranch != "main" {
		t.Errorf("resurrect base branch = %q, want main", wt.resurrected[0].BaseBranch)
	}

	// Verify Claude was started with resume.
	if len(cr.started) != 1 {
		t.Fatalf("expected 1 claude start, got %d", len(cr.started))
	}
	if cr.started[0].resume == nil || *cr.started[0].resume != "claude-old" {
		t.Errorf("expected claude resume with 'claude-old', got %v", cr.started[0].resume)
	}

	// Verify session state is ImplementingPlan.
	if sessions.sessions["sess-1"].State != machine.ImplementingPlan {
		t.Errorf("state = %v, want ImplementingPlan", sessions.sessions["sess-1"].State)
	}
}

func TestResurrectSessionNotArchived(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	logger := zerolog.Nop()

	sessions.sessions["sess-1"] = &models.Session{
		ID:     "sess-1",
		RepoID: "repo-1",
		State:  machine.ImplementingPlan,
		// ArchivedAt is nil — not archived.
	}

	lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), logger)

	err := lc.ResurrectSession(ctx, "sess-1")
	if err == nil {
		t.Fatal("expected error for non-archived session")
	}
	if err.Error() != "session sess-1 is not archived" {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestResurrectSessionClearsTerminalStateBeforeAgentStart pins the ordering the
// BOS-697 merged-but-unarchived sweep depends on: ResurrectSession must leave
// the terminal state in the same breath as clearing archived_at, before the
// slow agent start.
//
// {archived_at NULL, state Merged} is exactly archiveMergedButUnarchived's
// predicate, so a resurrect that left that shape standing across StartByAgent
// would let the reconcile tick archive the session back out from under the
// agent it is starting — and a StartByAgent failure would leave it wearing that
// shape forever, so the sweep would undo the resurrect on the very next tick.
//
// The agent start is made to fail on purpose: that is what proves the state
// write happened BEFORE it. With the write back in its old position (after the
// start) this assertion goes red, because the row would still read Merged.
func TestResurrectSessionClearsTerminalStateBeforeAgentStart(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	cr.startErr = errors.New("agent start failed")

	repos.repos["repo-1"] = &models.Repo{
		ID:                              "repo-1",
		LocalPath:                       "/tmp/repo",
		ShouldArchiveSessionsAfterMerge: true,
	}

	archivedAt := time.Now()
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		WorktreePath: "/tmp/worktrees/test-repo/test",
		BranchName:   "test",
		BaseBranch:   "main",
		State:        machine.Merged,
		ArchivedAt:   &archivedAt,
	}

	lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), zerolog.Nop())

	if err := lc.ResurrectSession(ctx, "sess-1"); err == nil {
		t.Fatal("expected ResurrectSession to fail when the agent cannot start")
	}

	got := sessions.sessions["sess-1"]
	if got.ArchivedAt != nil {
		t.Fatalf("archived_at = %v, want nil (the un-archive already landed)", got.ArchivedAt)
	}
	if got.State != machine.ImplementingPlan {
		t.Fatalf("state = %v, want ImplementingPlan; a failed resurrect must not leave the "+
			"merged-but-unarchived shape the reconcile sweep archives", got.State)
	}
}

func TestStopSessionNoClaudeProcess(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	logger := zerolog.Nop()

	sessions.sessions["sess-1"] = &models.Session{
		ID:     "sess-1",
		RepoID: "repo-1",
		State:  machine.ImplementingPlan,
		// No ClaudeSessionID.
	}

	lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), logger)

	if err := lc.StopSession(ctx, "sess-1"); err != nil {
		t.Fatalf("StopSession: %v", err)
	}

	// No Claude stop calls.
	if len(cr.stopped) != 0 {
		t.Errorf("expected 0 claude stops, got %d", len(cr.stopped))
	}

	// State should still be Closed.
	if sessions.sessions["sess-1"].State != machine.Closed {
		t.Errorf("state = %v, want Closed", sessions.sessions["sess-1"].State)
	}
}

func TestSubmitPR(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo",
		OriginURL: "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		Title:        "Test Session",
		Plan:         "Do something",
		WorktreePath: "/tmp/worktrees/test-repo/test-session",
		BranchName:   "test-session",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
	}

	lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, vp, logger)

	if err := lc.SubmitPR(ctx, "sess-1"); err != nil {
		t.Fatalf("SubmitPR: %v", err)
	}

	// Verify branch was pushed.
	if len(wt.pushed) != 1 || wt.pushed[0] != "test-session" {
		t.Errorf("expected push of test-session, got %v", wt.pushed)
	}

	// Verify draft PR was created.
	if len(vp.createPRCalls) != 1 {
		t.Fatalf("expected 1 createPR call, got %d", len(vp.createPRCalls))
	}
	call := vp.createPRCalls[0]
	if call.RepoPath != "owner/repo" {
		t.Errorf("PR repo = %q, want owner/repo", call.RepoPath)
	}
	if call.HeadBranch != "test-session" {
		t.Errorf("PR head = %q, want test-session", call.HeadBranch)
	}
	if call.BaseBranch != "main" {
		t.Errorf("PR base = %q, want main", call.BaseBranch)
	}
	if call.Title != "Test Session" {
		t.Errorf("PR title = %q, want 'Test Session'", call.Title)
	}
	if !call.Draft {
		t.Error("expected draft PR")
	}

	// Verify session was updated with PR info and state.
	sess := sessions.sessions["sess-1"]
	if sess.State != machine.AwaitingChecks {
		t.Errorf("state = %v, want AwaitingChecks", sess.State)
	}
	if sess.PRNumber == nil || *sess.PRNumber != 42 {
		t.Errorf("PR number = %v, want 42", sess.PRNumber)
	}
	if sess.PRURL == nil || *sess.PRURL != "https://github.com/owner/repo/pull/42" {
		t.Errorf("PR URL = %v, want https://github.com/owner/repo/pull/42", sess.PRURL)
	}
}

func TestSubmitPR_ClearsOnlyDraftPRBlockedReasonAfterCreate(t *testing.T) {
	t.Run("draft PR blocked reason is cleared", func(t *testing.T) {
		ctx := context.Background()
		sessions := newMockSessionStore()
		repos := newMockRepoStore()
		wt := &mockWorktreeManager{}
		cr := newMockAgentRunner()
		vp := newMockVCSProvider()
		logger := zerolog.Nop()

		repos.repos["repo-1"] = &models.Repo{
			ID:        "repo-1",
			LocalPath: "/tmp/repo",
			OriginURL: "owner/repo",
		}
		reason := "draft PR creation failed: create draft PR: GraphQL: Head sha can't be blank"
		sessions.sessions["sess-1"] = &models.Session{
			ID:            "sess-1",
			RepoID:        "repo-1",
			Title:         "Test Session",
			Plan:          "Do something",
			WorktreePath:  "/tmp/worktrees/test-repo/test-session",
			BranchName:    "test-session",
			BaseBranch:    "main",
			State:         machine.ImplementingPlan,
			BlockedReason: &reason,
		}

		lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, vp, logger)

		if err := lc.SubmitPR(ctx, "sess-1"); err != nil {
			t.Fatalf("SubmitPR: %v", err)
		}

		sess := sessions.sessions["sess-1"]
		if sess.PRNumber == nil || *sess.PRNumber != 42 {
			t.Fatalf("PRNumber = %v, want 42", sess.PRNumber)
		}
		if sess.PRURL == nil || *sess.PRURL != "https://github.com/owner/repo/pull/42" {
			t.Fatalf("PRURL = %v, want https://github.com/owner/repo/pull/42", sess.PRURL)
		}
		if sess.BlockedReason != nil {
			t.Fatalf("BlockedReason = %q, want nil", *sess.BlockedReason)
		}
	})

	t.Run("non-draft blocked reason is preserved", func(t *testing.T) {
		ctx := context.Background()
		sessions := newMockSessionStore()
		repos := newMockRepoStore()
		wt := &mockWorktreeManager{}
		cr := newMockAgentRunner()
		vp := newMockVCSProvider()
		logger := zerolog.Nop()

		repos.repos["repo-1"] = &models.Repo{
			ID:        "repo-1",
			LocalPath: "/tmp/repo",
			OriginURL: "owner/repo",
		}
		reason := "manual hold"
		sessions.sessions["sess-1"] = &models.Session{
			ID:            "sess-1",
			RepoID:        "repo-1",
			Title:         "Test Session",
			Plan:          "Do something",
			WorktreePath:  "/tmp/worktrees/test-repo/test-session",
			BranchName:    "test-session",
			BaseBranch:    "main",
			State:         machine.ImplementingPlan,
			BlockedReason: &reason,
		}

		lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, vp, logger)

		if err := lc.SubmitPR(ctx, "sess-1"); err != nil {
			t.Fatalf("SubmitPR: %v", err)
		}

		sess := sessions.sessions["sess-1"]
		if sess.BlockedReason == nil || *sess.BlockedReason != reason {
			t.Fatalf("BlockedReason = %v, want %q", sess.BlockedReason, reason)
		}
	})
}

func TestSubmitPR_ExistingPRStillPushesImplementationCommits(t *testing.T) {
	// When a draft PR was already created up-front (e.g. via createDraftPR
	// during StartSession), SubmitPR must still push so that any commits
	// Claude made on top of the placeholder empty commit reach the remote.
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo",
		OriginURL: "owner/repo",
	}
	existingPR := 7
	existingURL := "https://github.com/owner/repo/pull/7"
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		Title:        "Test Session",
		Plan:         "Do something",
		WorktreePath: "/tmp/worktrees/test-repo/test-session",
		BranchName:   "test-session",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
		PRNumber:     &existingPR,
		PRURL:        &existingURL,
	}

	lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, vp, logger)

	if err := lc.SubmitPR(ctx, "sess-1"); err != nil {
		t.Fatalf("SubmitPR: %v", err)
	}

	// Verify branch was pushed even though a PR already existed.
	if len(wt.pushed) != 1 || wt.pushed[0] != "test-session" {
		t.Errorf("expected push of test-session, got %v", wt.pushed)
	}

	// Verify no new PR was created.
	if len(vp.createPRCalls) != 0 {
		t.Errorf("expected 0 createPR calls (PR already exists), got %d", len(vp.createPRCalls))
	}

	// Verify session was advanced to AwaitingChecks with PR info preserved.
	sess := sessions.sessions["sess-1"]
	if sess.State != machine.AwaitingChecks {
		t.Errorf("state = %v, want AwaitingChecks", sess.State)
	}
	if sess.PRNumber == nil || *sess.PRNumber != 7 {
		t.Errorf("PR number = %v, want 7", sess.PRNumber)
	}
}

func TestSubmitPRWrongState(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo",
		OriginURL: "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:     "sess-1",
		RepoID: "repo-1",
		State:  machine.CreatingWorktree, // wrong state for SubmitPR
	}

	lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, vp, logger)

	err := lc.SubmitPR(ctx, "sess-1")
	if err == nil {
		t.Fatal("expected error for wrong state")
	}
}

func TestStartSession_NoPlan_CreateDraftPRFailsRepoNotReady(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
		OriginURL:         "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Test Session",
		Plan:       "", // no plan → triggers immediate PR creation
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
	}

	// Make CreateDraftPR return ErrRepoNotReady.
	vp.nextPRInfo = nil
	vp.createPRErr = vcs.ErrRepoNotReady

	lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, vp, logger)

	err := lc.StartSession(ctx, "sess-1", StartSessionOpts{})
	awaitDraftPR(t, lc, "sess-1")
	if err != nil {
		t.Fatalf("expected session to start successfully despite PR failure, got: %v", err)
	}

	// Session should be in ImplementingPlan state with no PR.
	sess := sessions.sessions["sess-1"]
	if sess.State != machine.ImplementingPlan {
		t.Errorf("expected state ImplementingPlan, got: %v", sess.State)
	}
	if sess.PRNumber != nil {
		t.Errorf("expected PRNumber to be nil, got: %v", *sess.PRNumber)
	}
	if sess.PRURL != nil {
		t.Errorf("expected PRURL to be nil, got: %v", *sess.PRURL)
	}
}

func TestStartSession_SkipSetupScript_NilsSetupScript(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	logger := zerolog.Nop()

	setupCmd := "npm install"
	repos.repos["repo-1"] = &models.Repo{
		ID:              "repo-1",
		LocalPath:       "/tmp/repo",
		WorktreeBaseDir: "/tmp/worktrees",
		DisplayName:     "test-repo",
		SetupScript:     &setupCmd,
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Bump lodash",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
	}

	lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), logger)

	// skipSetupScript = true with an existing branch (dependabot PR path).
	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{
		ExistingBranch:  "dependabot/npm/lodash-4.17.21",
		SkipSetupScript: true,
	}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	awaitDraftPR(t, lc, "sess-1")

	// Verify CreateFromExistingBranch was called with nil SetupScript.
	if len(wt.createdFromExisting) != 1 {
		t.Fatalf("expected 1 CreateFromExistingBranch call, got %d", len(wt.createdFromExisting))
	}
	if wt.createdFromExisting[0].SetupScript != nil {
		t.Errorf("expected nil SetupScript when skipSetupScript=true, got %q", *wt.createdFromExisting[0].SetupScript)
	}
}

func TestStartSession_SkipSetupScript_NewBranch(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	logger := zerolog.Nop()

	setupCmd := "npm install"
	repos.repos["repo-1"] = &models.Repo{
		ID:              "repo-1",
		LocalPath:       "/tmp/repo",
		WorktreeBaseDir: "/tmp/worktrees",
		DisplayName:     "test-repo",
		SetupScript:     &setupCmd,
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Bump lodash",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
	}

	lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), logger)

	// skipSetupScript = true with no existing branch (new branch path).
	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{SkipSetupScript: true}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	awaitDraftPR(t, lc, "sess-1")

	// Verify Create was called with nil SetupScript.
	if len(wt.created) != 1 {
		t.Fatalf("expected 1 Create call, got %d", len(wt.created))
	}
	if wt.created[0].SetupScript != nil {
		t.Errorf("expected nil SetupScript when skipSetupScript=true, got %q", *wt.created[0].SetupScript)
	}
}

// fakeSettingUp records SetSettingUp and SetArchiving calls.
type fakeSettingUp struct {
	mu             sync.Mutex
	calls          []bool
	archivingCalls []bool
	archiving      bool // current SetArchiving state (last value set)
}

func (f *fakeSettingUp) SetSettingUp(_ string, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, on)
}

func (f *fakeSettingUp) SetArchiving(_ string, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.archivingCalls = append(f.archivingCalls, on)
	f.archiving = on
}

func (f *fakeSettingUp) snapshot() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]bool, len(f.calls))
	copy(out, f.calls)
	return out
}

func (f *fakeSettingUp) archivingSnapshot() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]bool, len(f.archivingCalls))
	copy(out, f.archivingCalls)
	return out
}

func (f *fakeSettingUp) isArchiving() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.archiving
}

func TestArchiveSession_SetsAndClearsArchivingFlag(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:              "repo-1",
		LocalPath:       "/tmp/repo",
		WorktreeBaseDir: "/tmp/worktrees",
		DisplayName:     "test-repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		Title:        "Test Session",
		BaseBranch:   "main",
		WorktreePath: "/tmp/worktrees/test-repo/test-session",
		State:        machine.ImplementingPlan,
	}

	lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), logger)
	f := &fakeSettingUp{}
	lc.SetDisplayTracker(f)

	// Observe the flag mid-archive: the worktree Archive runs inside
	// ArchiveSession, between the set-true and the deferred clear.
	var midRunArchiving bool
	wt.archiveHook = func() { midRunArchiving = f.isArchiving() }

	if err := lc.ArchiveSession(ctx, "sess-1"); err != nil {
		t.Fatalf("ArchiveSession: %v", err)
	}

	if !midRunArchiving {
		t.Fatalf("expected Archiving=true during ArchiveSession run")
	}
	if want := []bool{true, false}; !reflect.DeepEqual(f.archivingSnapshot(), want) {
		t.Fatalf("archiving calls = %v, want %v", f.archivingSnapshot(), want)
	}
	if f.isArchiving() {
		t.Fatalf("expected Archiving cleared after ArchiveSession")
	}
}

func TestArchiveSession_ClearsArchivingFlagOnError(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	logger := zerolog.Nop()

	// No session row → l.sessions.Get fails, so ArchiveSession returns an error
	// after the set-true. The deferred clear must still run (error path).
	lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), logger)
	f := &fakeSettingUp{}
	lc.SetDisplayTracker(f)

	if err := lc.ArchiveSession(ctx, "missing"); err == nil {
		t.Fatalf("expected error for missing session")
	}

	if want := []bool{true, false}; !reflect.DeepEqual(f.archivingSnapshot(), want) {
		t.Fatalf("archiving calls = %v, want %v (flag not cleared on error)", f.archivingSnapshot(), want)
	}
	if f.isArchiving() {
		t.Fatalf("expected Archiving cleared on error path")
	}
}

func TestStartSession_SetsInitializingWhenSetupScriptPresent(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	logger := zerolog.Nop()

	setupCmd := "npm install"
	repos.repos["repo-1"] = &models.Repo{
		ID:              "repo-1",
		LocalPath:       "/tmp/repo",
		WorktreeBaseDir: "/tmp/worktrees",
		DisplayName:     "test-repo",
		SetupScript:     &setupCmd,
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Test Session",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
	}

	lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), logger)
	f := &fakeSettingUp{}
	lc.SetDisplayTracker(f)

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	awaitDraftPR(t, lc, "sess-1")

	if want := []bool{true, false}; !reflect.DeepEqual(f.snapshot(), want) {
		t.Fatalf("calls = %v, want %v", f.snapshot(), want)
	}
}

func TestStartSession_ClearsInitializingOnCreateError(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{createErr: errors.New("create failed")}
	cr := newMockAgentRunner()
	logger := zerolog.Nop()

	setupCmd := "npm install"
	repos.repos["repo-1"] = &models.Repo{
		ID:              "repo-1",
		LocalPath:       "/tmp/repo",
		WorktreeBaseDir: "/tmp/worktrees",
		DisplayName:     "test-repo",
		SetupScript:     &setupCmd,
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Test Session",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
	}

	lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), logger)
	f := &fakeSettingUp{}
	lc.SetDisplayTracker(f)

	_ = lc.StartSession(ctx, "sess-1", StartSessionOpts{})
	awaitDraftPR(t, lc, "sess-1")

	calls := f.snapshot()
	if len(calls) == 0 || calls[len(calls)-1] != false {
		t.Fatalf("flag not cleared on error: %v", calls)
	}
}

func TestStartSession_NoInitializingWithoutSetupScript(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	logger := zerolog.Nop()

	// Repo with no setup script.
	repos.repos["repo-1"] = &models.Repo{
		ID:              "repo-1",
		LocalPath:       "/tmp/repo",
		WorktreeBaseDir: "/tmp/worktrees",
		DisplayName:     "test-repo",
		SetupScript:     nil,
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Test Session",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
	}

	lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), logger)
	f := &fakeSettingUp{}
	lc.SetDisplayTracker(f)

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	awaitDraftPR(t, lc, "sess-1")

	if calls := f.snapshot(); len(calls) != 0 {
		t.Fatalf("expected no SetSettingUp calls when no setup script, got: %v", calls)
	}
}

func TestStartQuickChatSession(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Quick chat",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
	}

	lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), logger)

	if err := lc.StartQuickChatSession(ctx, "sess-1"); err != nil {
		t.Fatalf("StartQuickChatSession: %v", err)
	}

	// Verify NO worktree was created.
	if len(wt.created) != 0 {
		t.Errorf("expected 0 worktrees created, got %d", len(wt.created))
	}
	if len(wt.createdFromExisting) != 0 {
		t.Errorf("expected 0 existing branch worktrees, got %d", len(wt.createdFromExisting))
	}

	// Verify Claude was NOT started — chat launch happens on-demand from
	// the boss CLI's PTY manager, not from StartSession.
	if len(cr.started) != 0 {
		t.Fatalf("expected 0 claude starts, got %d", len(cr.started))
	}

	// Verify session was updated correctly.
	sess := sessions.sessions["sess-1"]
	if sess.State != machine.ImplementingPlan {
		t.Errorf("session state = %v, want ImplementingPlan", sess.State)
	}
	if sess.WorktreePath != "/tmp/repo" {
		t.Errorf("worktree path = %q, want /tmp/repo", sess.WorktreePath)
	}
	if sess.BranchName != "" {
		t.Errorf("branch name = %q, want empty", sess.BranchName)
	}
}

func TestArchiveQuickChatSession(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo",
	}

	agentSessionID := "claude-123"
	cr.running[agentSessionID] = true

	sessions.sessions["sess-1"] = &models.Session{
		ID:             "sess-1",
		RepoID:         "repo-1",
		State:          machine.ImplementingPlan,
		WorktreePath:   "/tmp/repo", // same as repo.LocalPath → quick chat
		AgentSessionID: &agentSessionID,
	}

	lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), logger)

	if err := lc.ArchiveSession(ctx, "sess-1"); err != nil {
		t.Fatalf("ArchiveSession: %v", err)
	}

	// Verify Claude was stopped.
	if len(cr.stopped) != 1 {
		t.Errorf("expected 1 claude stop, got %d", len(cr.stopped))
	}

	// Verify worktree was NOT archived (would destroy base repo).
	if len(wt.archived) != 0 {
		t.Errorf("expected 0 worktree archives for quick chat, got %d", len(wt.archived))
	}
}

func TestResurrectQuickChatSession(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo",
	}

	archTime := models.Session{}.CreatedAt
	oldClaudeID := "claude-old"
	sessions.sessions["sess-1"] = &models.Session{
		ID:             "sess-1",
		RepoID:         "repo-1",
		Title:          "Quick chat",
		WorktreePath:   "/tmp/repo", // same as repo.LocalPath → quick chat
		BranchName:     "",
		State:          machine.ImplementingPlan,
		ArchivedAt:     &archTime,
		AgentSessionID: &oldClaudeID,
	}

	lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), logger)

	if err := lc.ResurrectSession(ctx, "sess-1"); err != nil {
		t.Fatalf("ResurrectSession: %v", err)
	}

	// Verify worktree was NOT resurrected (no worktree to recreate).
	if len(wt.resurrected) != 0 {
		t.Errorf("expected 0 resurrect calls for quick chat, got %d", len(wt.resurrected))
	}

	// Verify Claude was started with resume.
	if len(cr.started) != 1 {
		t.Fatalf("expected 1 claude start, got %d", len(cr.started))
	}
	if cr.started[0].resume == nil || *cr.started[0].resume != "claude-old" {
		t.Errorf("expected claude resume with 'claude-old', got %v", cr.started[0].resume)
	}

	// Verify session state is ImplementingPlan.
	if sessions.sessions["sess-1"].State != machine.ImplementingPlan {
		t.Errorf("state = %v, want ImplementingPlan", sessions.sessions["sess-1"].State)
	}
}

func TestResolveOriginURL_AlreadySet(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo",
		OriginURL: "git@github.com:owner/repo.git",
	}

	lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), logger)

	if err := lc.resolveOriginURL(ctx, repos.repos["repo-1"]); err != nil {
		t.Fatalf("resolveOriginURL: %v", err)
	}
	if got := repos.repos["repo-1"].OriginURL; got != "git@github.com:owner/repo.git" {
		t.Errorf("repo.OriginURL = %q, want git@github.com:owner/repo.git", got)
	}
}

func TestResolveOriginURL_EmptyReDetected(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{originURL: "git@github.com:owner/repo.git"}
	cr := newMockAgentRunner()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo",
		OriginURL: "", // empty — needs re-detection
	}

	lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), logger)

	if err := lc.resolveOriginURL(ctx, repos.repos["repo-1"]); err != nil {
		t.Fatalf("resolveOriginURL: %v", err)
	}
	// Verify it was re-detected and persisted to the repo.
	if repos.repos["repo-1"].OriginURL != "git@github.com:owner/repo.git" {
		t.Errorf("repo.OriginURL = %q, want git@github.com:owner/repo.git", repos.repos["repo-1"].OriginURL)
	}
}

func TestResolveOriginURL_EmptyNoRemote(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{originURL: ""} // no remote configured
	cr := newMockAgentRunner()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:          "repo-1",
		DisplayName: "test-repo",
		LocalPath:   "/tmp/repo",
		OriginURL:   "",
	}

	lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), logger)

	err := lc.resolveOriginURL(ctx, repos.repos["repo-1"])
	if err == nil {
		t.Fatal("expected error when no origin remote is configured")
	}
	if !strings.Contains(err.Error(), "no origin remote configured") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestStartSession_NoPlan_EmptyOriginURL_ReDetected(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{originURL: "git@github.com:owner/repo.git"}
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
		OriginURL:         "", // empty — should be re-detected
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Test Session",
		Plan:       "", // no plan → triggers immediate PR creation
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
	}

	lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, vp, logger)

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	awaitDraftPR(t, lc, "sess-1")

	// Verify origin URL was re-detected and persisted.
	if repos.repos["repo-1"].OriginURL != "git@github.com:owner/repo.git" {
		t.Errorf("repo.OriginURL = %q, want git@github.com:owner/repo.git", repos.repos["repo-1"].OriginURL)
	}

	// Verify PR was created with the re-detected URL.
	if len(vp.createPRCalls) != 1 {
		t.Fatalf("expected 1 createPR call, got %d", len(vp.createPRCalls))
	}
	if vp.createPRCalls[0].RepoPath != "git@github.com:owner/repo.git" {
		t.Errorf("PR repo = %q, want git@github.com:owner/repo.git", vp.createPRCalls[0].RepoPath)
	}
}

func TestStartSession_NoSkipSetupScript_PassesSetupScript(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	logger := zerolog.Nop()

	setupCmd := "npm install"
	repos.repos["repo-1"] = &models.Repo{
		ID:              "repo-1",
		LocalPath:       "/tmp/repo",
		WorktreeBaseDir: "/tmp/worktrees",
		DisplayName:     "test-repo",
		SetupScript:     &setupCmd,
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Bump lodash",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
	}

	lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), logger)

	// skipSetupScript = false with existing branch.
	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{
		ExistingBranch: "dependabot/npm/lodash-4.17.21",
	}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	awaitDraftPR(t, lc, "sess-1")

	// Verify CreateFromExistingBranch was called WITH SetupScript.
	if len(wt.createdFromExisting) != 1 {
		t.Fatalf("expected 1 CreateFromExistingBranch call, got %d", len(wt.createdFromExisting))
	}
	if wt.createdFromExisting[0].SetupScript == nil {
		t.Error("expected non-nil SetupScript when skipSetupScript=false")
	} else if *wt.createdFromExisting[0].SetupScript != "npm install" {
		t.Errorf("expected SetupScript 'npm install', got %q", *wt.createdFromExisting[0].SetupScript)
	}
}

// TestStartSession_DeferPRFalse_CreatesDraftPR pins the pre-FL3 behavior: a
// session with no PR and DeferPR=false (the zero value) must still get an
// up-front draft PR during StartSession.
func TestStartSession_DeferPRFalse_CreatesDraftPR(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
		OriginURL:         "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Test Session",
		Plan:       "Do something",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
	}

	lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, vp, logger)

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	awaitDraftPR(t, lc, "sess-1")

	if len(vp.createPRCalls) != 1 {
		t.Fatalf("expected 1 createPR call with DeferPR=false, got %d", len(vp.createPRCalls))
	}
	sess := sessions.sessions["sess-1"]
	if sess.PRNumber == nil {
		t.Error("expected PRNumber to be populated after up-front draft PR")
	}
	// Worktree creation already fetched this base earlier in the same
	// StartSession call, so the draft-PR verification must not fetch it again.
	if len(wt.verifyPushedCalls) != 1 {
		t.Fatalf("VerifyPushedBranchAheadOfBase calls = %d, want 1", len(wt.verifyPushedCalls))
	}
	if !wt.verifyPushedCalls[0].skipFetch {
		t.Error("skipFetch = false, want true so session create fetches the base only once")
	}
}

func TestStartSession_CreateDraftPRFailureStoresBlockedReason(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	agentChats := &mockAgentChatStore{}
	worktrees := &mockWorktreeManager{}
	agentRunner := newMockAgentRunner()
	provider := newMockVCSProvider()
	lifecycle := newTestLifecycle(sessions, repos, agentChats, nil, worktrees, agentRunner, nil, provider, zerolog.Nop())

	repo := &models.Repo{
		ID:              "repo-1",
		DisplayName:     "repo",
		LocalPath:       "/tmp/repo",
		OriginURL:       "git@github.com:owner/repo.git",
		WorktreeBaseDir: "/tmp/worktrees",
	}
	repos.repos[repo.ID] = repo

	session := &models.Session{
		ID:         "sess-1",
		RepoID:     repo.ID,
		Title:      "Open a PR",
		Plan:       "Do useful work",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
	}
	sessions.sessions[session.ID] = session

	provider.createPRErr = errors.New("gh pr create: GraphQL: Head sha can't be blank")

	if err := lifecycle.StartSession(ctx, session.ID, StartSessionOpts{}); err != nil {
		t.Fatalf("StartSession returned error: %v", err)
	}
	awaitDraftPR(t, lifecycle, session.ID)

	updated := sessions.sessions[session.ID]
	if updated.PRNumber != nil {
		t.Fatalf("PRNumber = %v, want nil", *updated.PRNumber)
	}
	if updated.PRURL != nil {
		t.Fatalf("PRURL = %v, want nil", *updated.PRURL)
	}
	if updated.BlockedReason == nil {
		t.Fatal("BlockedReason = nil, want draft PR failure")
	}
	wantReason := "draft PR creation failed: create draft PR: gh pr create: GraphQL: Head sha can't be blank"
	if !strings.Contains(*updated.BlockedReason, wantReason) {
		t.Fatalf("BlockedReason = %q, want to contain %q", *updated.BlockedReason, wantReason)
	}
	if updated.State != machine.ImplementingPlan {
		t.Fatalf("State = %v, want ImplementingPlan", updated.State)
	}
}

func TestStartSession_CreateDraftPRFailureLogsBranchDiagnostics(t *testing.T) {
	const prFailure = "gh pr create: GraphQL: Head sha can't be blank"

	findLogEvent := func(t *testing.T, logs string, message string) map[string]any {
		t.Helper()
		for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var event map[string]any
			if err := json.Unmarshal([]byte(line), &event); err != nil {
				t.Fatalf("decode log line %q: %v", line, err)
			}
			if event["message"] == message {
				return event
			}
		}
		t.Fatalf("log event %q not found in:\n%s", message, logs)
		return nil
	}

	startWithDraftPRError := func(t *testing.T, worktrees *mockWorktreeManager, logger zerolog.Logger) (*mockSessionStore, error) {
		t.Helper()
		ctx := context.Background()
		sessions := newMockSessionStore()
		repos := newMockRepoStore()
		agentChats := &mockAgentChatStore{}
		agentRunner := newMockAgentRunner()
		provider := newMockVCSProvider()
		lifecycle := newTestLifecycle(sessions, repos, agentChats, nil, worktrees, agentRunner, nil, provider, logger)

		repo := &models.Repo{
			ID:              "repo-1",
			DisplayName:     "repo",
			LocalPath:       "/tmp/repo",
			OriginURL:       "git@github.com:owner/repo.git",
			WorktreeBaseDir: "/tmp/worktrees",
		}
		repos.repos[repo.ID] = repo
		sessions.sessions["sess-1"] = &models.Session{
			ID:         "sess-1",
			RepoID:     repo.ID,
			Title:      "Open a PR",
			Plan:       "Do useful work",
			BaseBranch: "main",
			State:      machine.CreatingWorktree,
		}
		provider.createPRErr = errors.New(prFailure)

		err := lifecycle.StartSession(ctx, "sess-1", StartSessionOpts{})
		awaitDraftPR(t, lifecycle, "sess-1")
		return sessions, err
	}

	assertBlockedReason := func(t *testing.T, sessions *mockSessionStore) {
		t.Helper()
		updated := sessions.sessions["sess-1"]
		if updated.BlockedReason == nil {
			t.Fatal("BlockedReason = nil, want draft PR failure")
		}
		wantReason := "draft PR creation failed: create draft PR: " + prFailure
		if !strings.Contains(*updated.BlockedReason, wantReason) {
			t.Fatalf("BlockedReason = %q, want to contain %q", *updated.BlockedReason, wantReason)
		}
	}

	assertSnapshotCall := func(t *testing.T, worktrees *mockWorktreeManager, wantPath, wantBranch, wantBase string) {
		t.Helper()
		if len(worktrees.branchDebugSnapshotCalls) != 1 {
			t.Fatalf("BranchDebugSnapshot calls = %d, want 1", len(worktrees.branchDebugSnapshotCalls))
		}
		call := worktrees.branchDebugSnapshotCalls[0]
		if call.worktreePath != wantPath || call.branch != wantBranch || call.baseBranch != wantBase {
			t.Fatalf("BranchDebugSnapshot call = %+v, want path=%q branch=%q base=%q", call, wantPath, wantBranch, wantBase)
		}
	}

	t.Run("logs snapshot fields", func(t *testing.T) {
		var logs bytes.Buffer
		worktrees := &mockWorktreeManager{
			worktreePath: "/tmp/worktrees/repo/test-session",
			branchDebugSnapshot: &gitpkg.BranchDebugSnapshot{
				CurrentBranch: "test-session",
				HeadSHA:       "abc123",
				RemoteHeadSHA: "def456",
				AheadBehind:   "0\t2",
			},
		}

		// SyncWriter: StartSession and its background draft-PR step (BOS-540)
		// now both log, so an unsynchronized bytes.Buffer has two writers.
		sessions, err := startWithDraftPRError(t, worktrees, zerolog.New(zerolog.SyncWriter(&logs)))
		if err != nil {
			t.Fatalf("StartSession returned error: %v", err)
		}
		assertBlockedReason(t, sessions)
		assertSnapshotCall(t, worktrees, "/tmp/worktrees/repo/test-session", "test-session", "main")

		event := findLogEvent(t, logs.String(), "draft PR creation failed with branch debug snapshot")
		for field, want := range map[string]string{
			"session":         "sess-1",
			"branch":          "test-session",
			"base":            "main",
			"current_branch":  "test-session",
			"head_sha":        "abc123",
			"remote_head_sha": "def456",
			"ahead_behind":    "0\t2",
		} {
			if got := event[field]; got != want {
				t.Fatalf("log field %s = %v, want %q", field, got, want)
			}
		}
		if got := fmt.Sprint(event["error"]); !strings.Contains(got, "create draft PR: "+prFailure) {
			t.Fatalf("log error = %q, want original draft PR error", got)
		}
	})

	t.Run("logs original draft PR error when snapshot collection fails", func(t *testing.T) {
		var logs bytes.Buffer
		worktrees := &mockWorktreeManager{
			worktreePath:           "/tmp/worktrees/repo/test-session",
			branchDebugSnapshotErr: errors.New("snapshot failed"),
		}

		// SyncWriter: StartSession and its background draft-PR step (BOS-540)
		// now both log, so an unsynchronized bytes.Buffer has two writers.
		sessions, err := startWithDraftPRError(t, worktrees, zerolog.New(zerolog.SyncWriter(&logs)))
		if err != nil {
			t.Fatalf("StartSession returned error: %v", err)
		}
		assertBlockedReason(t, sessions)
		assertSnapshotCall(t, worktrees, "/tmp/worktrees/repo/test-session", "test-session", "main")

		event := findLogEvent(t, logs.String(), "failed to collect branch debug snapshot after draft PR failure")
		for field, want := range map[string]string{
			"session":        "sess-1",
			"branch":         "test-session",
			"draft_pr_error": "create draft PR: " + prFailure,
		} {
			if got := event[field]; got != want {
				t.Fatalf("log field %s = %v, want %q", field, got, want)
			}
		}
		if got := fmt.Sprint(event["error"]); got != "snapshot failed" {
			t.Fatalf("log error = %q, want snapshot failure", got)
		}
	})
}

func TestStartSession_BlocksDraftPRWhenWorktreeBranchMismatches(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{
		verifyCurrentBranchErr: errors.New(`worktree is on branch "production", expected "fix-camera-crash"`),
	}
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
		OriginURL:         "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Fix camera crash",
		BranchName: "fix-camera-crash",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
	}

	lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, vp, logger)
	err := lc.StartSession(ctx, "sess-1", StartSessionOpts{})
	awaitDraftPR(t, lc, "sess-1")
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	if wt.emptyCommitCalls != 0 {
		t.Fatalf("EmptyCommit calls = %d, want 0", wt.emptyCommitCalls)
	}
	if len(vp.createPRCalls) != 0 {
		t.Fatalf("CreateDraftPR calls = %d, want 0", len(vp.createPRCalls))
	}
	sess := sessions.sessions["sess-1"]
	if sess.BlockedReason == nil {
		t.Fatal("BlockedReason = nil, want branch mismatch reason")
	}
	if !strings.Contains(*sess.BlockedReason, `worktree is on branch "production", expected "fix-camera-crash"`) {
		t.Fatalf("BlockedReason = %q, want branch mismatch", *sess.BlockedReason)
	}
}

// TestStartSession_DeferPRTrue_SkipsDraftPR verifies the cron-session path:
// DeferPR=true must suppress the up-front PR creation so the finalize path
// can later call EnsurePR based on the session's outcome.
func TestStartSession_DeferPRTrue_SkipsDraftPR(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
		OriginURL:         "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Nightly audit",
		Plan:       "Run the audit",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
	}

	lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, vp, logger)

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{DeferPR: true}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	awaitDraftPR(t, lc, "sess-1")

	if len(vp.createPRCalls) != 0 {
		t.Errorf("expected 0 createPR calls with DeferPR=true, got %d", len(vp.createPRCalls))
	}
	if len(wt.emptyCommits) != 0 {
		t.Errorf("expected 0 empty commits with DeferPR=true, got %d", len(wt.emptyCommits))
	}
	sess := sessions.sessions["sess-1"]
	if sess.PRNumber != nil {
		t.Errorf("expected PRNumber to remain nil, got %v", sess.PRNumber)
	}
	// BOS-540: DeferPR must not merely skip the create, it must start no
	// background step at all — no goroutine, and no in-flight marker claiming a
	// PR is coming when none is.
	if sess.BlockedReason != nil {
		t.Errorf("BlockedReason = %q, want nil: DeferPR advertises no pending PR", *sess.BlockedReason)
	}
	if lc.backgroundDraftPRIsTracked("sess-1") {
		t.Error("DeferPR session registered a background draft-PR step, want none")
	}
}

// newBackgroundDraftPRFixture builds the shared setup for the BOS-540 tests: a
// repo, one PR-less session in CreatingWorktree, and a Lifecycle wired to
// controllable mocks.
func newBackgroundDraftPRFixture(t *testing.T) (*mockSessionStore, *mockWorktreeManager, *mockVCSProvider, *Lifecycle) {
	t.Helper()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	vp := newMockVCSProvider()

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
		OriginURL:         "git@github.com:owner/repo.git",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Test Session",
		Plan:       "Do something",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
	}

	lc := newTestLifecycle(sessions, repos, nil, nil, wt, newMockAgentRunner(), nil, vp, zerolog.Nop())
	return sessions, wt, vp, lc
}

// TestStartSession_DraftPRCreatedInBackgroundAfterReturn is the core BOS-540
// assertion: StartSession returns — session usable, state ImplementingPlan —
// while the draft PR has not been created yet, and the PR number is persisted to
// the session row once the background step finishes.
//
// The create is parked inside VerifyCurrentBranch, the first thing createDraftPR
// does, so the window the test inspects is exactly what a client sees between
// "session created" and "PR opened".
func TestStartSession_DraftPRCreatedInBackgroundAfterReturn(t *testing.T) {
	ctx := context.Background()
	sessions, wt, vp, lc := newBackgroundDraftPRFixture(t)

	parked := make(chan struct{})
	release := make(chan struct{})
	wt.verifyCurrentBranchHook = func() {
		close(parked)
		<-release
	}

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	// StartSession has returned while the create is still parked: everything
	// read below is ordered behind the parked goroutine's last write by the
	// channel handshake, so these are stable observations, not a snapshot race.
	<-parked

	sess := sessions.sessions["sess-1"]
	if sess.State != machine.ImplementingPlan {
		t.Fatalf("State = %v, want ImplementingPlan before the PR exists", sess.State)
	}
	if sess.PRNumber != nil {
		t.Fatalf("PRNumber = %d, want nil: StartSession must not wait for the PR", *sess.PRNumber)
	}
	if len(vp.createPRCalls) != 0 {
		t.Fatalf("CreateDraftPR calls = %d, want 0 while the background step is still running", len(vp.createPRCalls))
	}
	// Observability: the session says a PR is coming, and says it in a form that
	// is NOT a failure — otherwise the TUI would render "? PR failed" for every
	// healthy session start.
	if !sessionreason.IsDraftPRCreationInFlight(sess.BlockedReason) {
		t.Fatalf("BlockedReason = %v, want the draft-PR in-flight marker", sess.BlockedReason)
	}
	if sessionreason.IsDraftPRCreationFailure(sess.BlockedReason) {
		t.Fatalf("BlockedReason = %q reads as a failure, want in-flight", *sess.BlockedReason)
	}

	close(release)
	awaitDraftPR(t, lc, "sess-1")

	sess = sessions.sessions["sess-1"]
	if sess.PRNumber == nil || *sess.PRNumber != 42 {
		t.Fatalf("PRNumber = %v, want 42 once the background create finished", sess.PRNumber)
	}
	if sess.PRURL == nil || *sess.PRURL != "https://github.com/owner/repo/pull/42" {
		t.Fatalf("PRURL = %v, want the created PR's URL", sess.PRURL)
	}
	if sess.BlockedReason != nil {
		t.Fatalf("BlockedReason = %q, want nil once the PR landed", *sess.BlockedReason)
	}
	if len(vp.createPRCalls) != 1 {
		t.Fatalf("CreateDraftPR calls = %d, want 1", len(vp.createPRCalls))
	}
}

// TestStartSession_BackgroundDraftPRFailureBlocksAndPreservesBranch pins that
// moving the create into the background did not move its failure handling: the
// blocked reason still lands (replacing the in-flight marker, so the session
// reads as failed rather than perpetually pending), the branch is still pushed
// and preserved for a retry, and StartSession itself still succeeds.
func TestStartSession_BackgroundDraftPRFailureBlocksAndPreservesBranch(t *testing.T) {
	ctx := context.Background()
	sessions, wt, vp, lc := newBackgroundDraftPRFixture(t)
	vp.createPRErr = errors.New("gh pr create: GraphQL: Head sha can't be blank")

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	awaitDraftPR(t, lc, "sess-1")

	sess := sessions.sessions["sess-1"]
	if sess.PRNumber != nil {
		t.Fatalf("PRNumber = %d, want nil after a failed create", *sess.PRNumber)
	}
	if !sessionreason.IsDraftPRCreationFailure(sess.BlockedReason) {
		t.Fatalf("BlockedReason = %v, want a draft PR creation failure", sess.BlockedReason)
	}
	if sessionreason.IsDraftPRCreationInFlight(sess.BlockedReason) {
		t.Fatal("BlockedReason still reads as in-flight after the create failed")
	}
	if sess.State != machine.ImplementingPlan {
		t.Fatalf("State = %v, want ImplementingPlan: a failed PR must not derail the session", sess.State)
	}
	// Retryable: the branch reached the remote, so EnsurePR/SubmitPR can open
	// the PR later without redoing the session.
	if len(wt.pushed) != 1 || wt.pushed[0] != sess.BranchName {
		t.Fatalf("pushed branches = %v, want [%s] preserved for retry", wt.pushed, sess.BranchName)
	}
}

// TestStartSession_FinalizeRacingBackgroundDraftPRYieldsOnePR covers the
// ordering constraint the background step introduces: a run short enough to
// finalize before the create completes has two writers trying to open a PR for
// the same branch. GitHub rejects the second with ErrPRAlreadyExists, and the
// existing attach path must turn that into "attach the PR that exists" — so the
// session ends with exactly one PR, not two and not a blocked session.
func TestStartSession_FinalizeRacingBackgroundDraftPRYieldsOnePR(t *testing.T) {
	ctx := context.Background()
	sessions, _, vp, lc := newBackgroundDraftPRFixture(t)

	parked := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	attempts := 0
	vp.createPRHook = func(opts vcs.CreatePROpts) (*vcs.PRInfo, error) {
		mu.Lock()
		attempts++
		n := attempts
		vp.createPRCalls = append(vp.createPRCalls, opts)
		mu.Unlock()
		if n == 1 {
			// The background create, parked mid-flight so finalize can overtake it.
			close(parked)
			<-release
			return nil, vcs.ErrPRAlreadyExists
		}
		return &vcs.PRInfo{Number: 91, URL: "https://github.com/owner/repo/pull/91"}, nil
	}

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	<-parked

	// Finalize's EnsurePR wins the race and opens PR 91.
	branch := sessions.sessions["sess-1"].BranchName
	vp.nextOpenPRs = []vcs.PRSummary{
		{Number: 91, Title: "Test Session", HeadBranch: branch, State: vcs.PRStateOpen},
	}
	if err := lc.EnsurePR(ctx, "sess-1"); err != nil {
		t.Fatalf("EnsurePR: %v", err)
	}

	close(release)
	awaitDraftPR(t, lc, "sess-1")

	sess := sessions.sessions["sess-1"]
	if sess.PRNumber == nil || *sess.PRNumber != 91 {
		t.Fatalf("PRNumber = %v, want 91 (the single PR both writers converged on)", sess.PRNumber)
	}
	if sess.PRURL == nil || *sess.PRURL != "https://github.com/owner/repo/pull/91" {
		t.Fatalf("PRURL = %v, want PR 91's URL", sess.PRURL)
	}
	if sess.BlockedReason != nil {
		t.Fatalf("BlockedReason = %q, want nil: losing the race is not a failure", *sess.BlockedReason)
	}
	mu.Lock()
	gotAttempts := attempts
	mu.Unlock()
	if gotAttempts != 2 {
		t.Fatalf("CreateDraftPR attempts = %d, want 2 (both writers tried)", gotAttempts)
	}
	// Exactly one of the two attempts produced a PR; the loser attached to it
	// rather than opening a second, which is what the ListOpenPRs lookup proves.
	if len(vp.listOpenPRCalls) != 1 {
		t.Fatalf("ListOpenPRs calls = %d, want 1 (the duplicate-create attach path)", len(vp.listOpenPRCalls))
	}
}

// TestStartSession_BackgroundDraftPRSkippedWhenPRAlreadyAttached covers the
// cheap half of the interlock: when another writer attaches a PR before the
// background step gets to run, the step must bow out without touching GitHub —
// and must clear the in-flight marker it wrote, so the session is not left
// advertising a PR that is never coming.
func TestStartSession_BackgroundDraftPRSkippedWhenPRAlreadyAttached(t *testing.T) {
	ctx := context.Background()
	sessions, _, vp, lc := newBackgroundDraftPRFixture(t)

	// The update hook fires on the in-flight marker write, which happens on the
	// calling goroutine BEFORE the background step is spawned — so the PR is
	// guaranteed to be attached by the time the step re-reads the row.
	existing := 55
	existingURL := "https://github.com/owner/repo/pull/55"
	sessions.updateHook = func(id string, params db.UpdateSessionParams) error {
		if params.BlockedReason != nil && *params.BlockedReason != nil &&
			sessionreason.IsDraftPRCreationInFlight(*params.BlockedReason) {
			sessions.sessions[id].PRNumber = &existing
			sessions.sessions[id].PRURL = &existingURL
		}
		return nil
	}

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	awaitDraftPR(t, lc, "sess-1")

	sess := sessions.sessions["sess-1"]
	if sess.PRNumber == nil || *sess.PRNumber != existing {
		t.Fatalf("PRNumber = %v, want the already-attached 55", sess.PRNumber)
	}
	if len(vp.createPRCalls) != 0 {
		t.Fatalf("CreateDraftPR calls = %d, want 0 when a PR is already attached", len(vp.createPRCalls))
	}
	if sess.BlockedReason != nil {
		t.Fatalf("BlockedReason = %q, want nil: the in-flight marker must not outlive the step", *sess.BlockedReason)
	}
}

// TestWaitForBackgroundDraftPRsNamesAbandonedSessions pins the shutdown
// contract: a create still in flight when the daemon gives up is REPORTED, not
// silently dropped, so the operator knows which sessions may have a PR opened
// remotely after the daemon exits.
func TestWaitForBackgroundDraftPRsNamesAbandonedSessions(t *testing.T) {
	ctx := context.Background()
	_, wt, _, lc := newBackgroundDraftPRFixture(t)

	parked := make(chan struct{})
	release := make(chan struct{})
	wt.verifyCurrentBranchHook = func() {
		close(parked)
		<-release
	}

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	<-parked

	giveUp, cancel := context.WithCancel(context.Background())
	cancel()
	abandoned := lc.WaitForBackgroundDraftPRs(giveUp)
	if len(abandoned) != 1 || abandoned[0] != "sess-1" {
		t.Fatalf("abandoned = %v, want [sess-1]", abandoned)
	}

	close(release)
	awaitDraftPR(t, lc, "sess-1")

	// Drained: a later shutdown reports nothing outstanding.
	if abandoned := lc.WaitForBackgroundDraftPRs(context.Background()); len(abandoned) != 0 {
		t.Fatalf("abandoned after drain = %v, want none", abandoned)
	}
}

// TestBackgroundDraftPRFailureDoesNotOverwriteAnotherOwnersState pins the
// failure-path twin of the clearDraftPRBlockedReason re-read. The step's failure
// is decided up to a full push + `gh pr create` after it started, so by the time
// it lands the blocked reason may no longer be ours: finalize's EnsurePR may
// have opened the PR (making our failure moot — and "? PR failed" would then
// render forever on a session that HAS its PR), or the session's own run may
// have recorded a real block (whose diagnostic we would swap for a PR one).
func TestBackgroundDraftPRFailureDoesNotOverwriteAnotherOwnersState(t *testing.T) {
	inFlight := sessionreason.DraftPRCreationInFlight()
	createErr := errors.New("push branch: index.lock exists")

	tests := []struct {
		name    string
		session *models.Session
		// wantReason is the blocked reason expected after the failed create;
		// empty means "expect a draft-PR failure reason".
		wantReason string
	}{
		{
			name:    "no owner: the failure is recorded",
			session: &models.Session{ID: "sess-1", RepoID: "repo-1", BlockedReason: &inFlight},
		},
		{
			name: "a PR landed first: nothing is recorded",
			session: &models.Session{
				ID: "sess-1", RepoID: "repo-1",
				PRNumber: intPtr(42),
			},
			wantReason: "\x00none",
		},
		{
			name: "another owner blocked the session: its reason survives",
			session: &models.Session{
				ID: "sess-1", RepoID: "repo-1",
				BlockedReason: strptr("blocked: max fix attempts reached"),
			},
			wantReason: "blocked: max fix attempts reached",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sessions := newMockSessionStore()
			sessions.sessions["sess-1"] = tt.session
			lc := newTestLifecycle(
				sessions, newMockRepoStore(), nil, nil,
				&mockWorktreeManager{}, newMockAgentRunner(), nil, newMockVCSProvider(), zerolog.Nop(),
			)

			lc.failInFlightDraftPR(context.Background(), "sess-1", createErr)

			got := sessions.sessions["sess-1"].BlockedReason
			switch tt.wantReason {
			case "":
				if !sessionreason.IsDraftPRCreationFailure(got) {
					t.Fatalf("BlockedReason = %v, want a draft PR creation failure", got)
				}
			case "\x00none":
				if got != nil {
					t.Fatalf("BlockedReason = %q, want nil: the PR exists, so the failure is moot", *got)
				}
			default:
				if got == nil || *got != tt.wantReason {
					t.Fatalf("BlockedReason = %v, want %q left in place", got, tt.wantReason)
				}
			}
		})
	}
}

// TestBackgroundDraftPRPanicStillRecordsAFailure pins the marker's last escape
// hatch. safego.Go RECOVERS panics instead of crashing the process, so a panic
// under the step would otherwise unwind straight past every failure handler and
// leave the session advertising a PR that is never coming — until the next
// daemon boot, since the stale-marker sweep only runs at startup.
func TestBackgroundDraftPRPanicStillRecordsAFailure(t *testing.T) {
	ctx := context.Background()
	sessions, wt, _, lc := newBackgroundDraftPRFixture(t)
	wt.verifyCurrentBranchHook = func() { panic("boom from the provider") }

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	awaitDraftPR(t, lc, "sess-1")

	got := sessions.sessions["sess-1"].BlockedReason
	if sessionreason.IsDraftPRCreationInFlight(got) {
		t.Fatal("BlockedReason is still the in-flight marker after a panic; the session would advertise a PR forever")
	}
	if !sessionreason.IsDraftPRCreationFailure(got) {
		t.Fatalf("BlockedReason = %v, want a draft PR creation failure", got)
	}
}

// TestBackgroundDraftPRCancellationClearsRatherThanFails pins the teardown
// paths that PRESERVE the row. ArchiveSession cancels the create, and a cancel
// is a deliberate teardown, not a failure: recording one would leave the
// archived session reading "draft PR creation failed: … context canceled",
// which drives the "? PR failed" label for a session nobody asked to have a PR.
func TestBackgroundDraftPRCancellationClearsRatherThanFails(t *testing.T) {
	ctx := context.Background()
	sessions, wt, _, lc := newBackgroundDraftPRFixture(t)

	parked := make(chan struct{})
	wt.verifyCurrentBranchCtxHook = func(stepCtx context.Context) {
		close(parked)
		<-stepCtx.Done()
	}
	// Faithful to what a cancelled create actually surfaces. exec.CommandContext
	// KILLS the git/gh subprocess, so the error is an *exec.ExitError — "signal:
	// killed" — which does NOT satisfy errors.Is(err, context.Canceled). A
	// cancel-detection keyed on the error value alone passes a test that returns
	// a wrapped context error and fails in production; this returns the real
	// shape, so only a context-keyed detection passes.
	wt.verifyCurrentBranchErr = errors.New("signal: killed")

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	<-parked
	if !sessionreason.IsDraftPRCreationInFlight(sessions.sessions["sess-1"].BlockedReason) {
		t.Fatal("precondition: the in-flight marker should be set while the create is parked")
	}

	lc.StopBackgroundDraftPR(ctx, "sess-1")

	got := sessions.sessions["sess-1"].BlockedReason
	if got != nil {
		t.Fatalf("BlockedReason = %q, want nil: a cancelled create is a teardown, not a failure", *got)
	}
}

// TestOpenDraftPRForBranchRefreshesTitleAndPRBeforeCreating pins the two row
// fields the background step decides from and then writes back, both of which
// can move under it during the ~12-60s create.
func TestOpenDraftPRForBranchRefreshesTitleAndPRBeforeCreating(t *testing.T) {
	t.Run("a rename during the create reaches the PR and survives", func(t *testing.T) {
		sessions := newMockSessionStore()
		vp := newMockVCSProvider()
		sessions.sessions["sess-1"] = &models.Session{
			ID: "sess-1", RepoID: "repo-1", BaseBranch: "main",
			BranchName: "feat-x", Title: "renamed by the user",
		}
		lc := newTestLifecycle(
			sessions, newMockRepoStore(), nil, nil,
			&mockWorktreeManager{}, newMockAgentRunner(), nil, vp, zerolog.Nop(),
		)

		// The step's copy, taken before the rename landed.
		stale := &models.Session{
			ID: "sess-1", RepoID: "repo-1", BaseBranch: "main",
			BranchName: "feat-x", Title: "original title",
		}
		if err := lc.openDraftPRForBranch(context.Background(), "sess-1", stale, &models.Repo{ID: "repo-1", OriginURL: "git@github.com:owner/repo.git"}); err != nil {
			t.Fatalf("openDraftPRForBranch: %v", err)
		}

		if len(vp.createPRCalls) != 1 {
			t.Fatalf("CreateDraftPR calls = %d, want 1", len(vp.createPRCalls))
		}
		if got := vp.createPRCalls[0].Title; got != "renamed by the user" {
			t.Fatalf("PR title = %q, want the renamed title: UpdateSession skipped its own PR sync because pr_number was still nil", got)
		}
		if got := sessions.sessions["sess-1"].Title; got != "renamed by the user" {
			t.Fatalf("stored Title = %q, want the rename preserved, not reverted", got)
		}
	})

	t.Run("a PR attached during the create is adopted, not duplicated", func(t *testing.T) {
		sessions := newMockSessionStore()
		vp := newMockVCSProvider()
		inFlight := sessionreason.DraftPRCreationInFlight()
		sessions.sessions["sess-1"] = &models.Session{
			ID: "sess-1", RepoID: "repo-1", BaseBranch: "main", BranchName: "feat-x",
			PRNumber: intPtr(99), PRURL: strptr("https://github.com/owner/repo/pull/99"),
			BlockedReason: &inFlight,
		}
		lc := newTestLifecycle(
			sessions, newMockRepoStore(), nil, nil,
			&mockWorktreeManager{}, newMockAgentRunner(), nil, vp, zerolog.Nop(),
		)

		stale := &models.Session{ID: "sess-1", RepoID: "repo-1", BaseBranch: "main", BranchName: "feat-x"}
		if err := lc.openDraftPRForBranch(context.Background(), "sess-1", stale, &models.Repo{ID: "repo-1", OriginURL: "git@github.com:owner/repo.git"}); err != nil {
			t.Fatalf("openDraftPRForBranch: %v", err)
		}

		if len(vp.createPRCalls) != 0 {
			t.Fatalf("CreateDraftPR calls = %d, want 0: a PR is already attached", len(vp.createPRCalls))
		}
		if got := sessions.sessions["sess-1"].PRNumber; got == nil || *got != 99 {
			t.Fatalf("stored PRNumber = %v, want the attached 99 untouched", got)
		}
		if stale.PRNumber == nil || *stale.PRNumber != 99 {
			t.Fatalf("caller copy PRNumber = %v, want 99 adopted", stale.PRNumber)
		}
		// The in-flight marker still has to go: no create is coming.
		if got := sessions.sessions["sess-1"].BlockedReason; got != nil {
			t.Fatalf("BlockedReason = %q, want nil once the PR is adopted", *got)
		}
	})
}

// ctxAwareSessionStore rejects writes on a Done context, the way SQLite's
// ExecContext does. The plain mock ignores ctx entirely, so without this the
// bug pinned below — terminal state writes issued on the create's own expired
// context — would pass silently in tests and fail only in production.
type ctxAwareSessionStore struct {
	db.SessionStore
}

func (s ctxAwareSessionStore) Update(ctx context.Context, id string, params db.UpdateSessionParams) (*models.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.SessionStore.Update(ctx, id, params)
}

// TestBackgroundDraftPRFailureIsRecordedWhenTheCreateContextIsDone pins the
// worst state the in-flight marker can get into. The background step bounds
// itself with backgroundDraftPRTimeout, so on the wedged-create path — the one
// where the marker MUST be replaced by a failure — the create's own context is
// already Done. Writing the failure through that context makes the UPDATE fail,
// and the session then advertises "draft PR creation in progress" forever: no
// PR, no failure, nothing to retry. The terminal writes therefore run on a
// detached context.
//
// The context is EXPIRED rather than cancelled, which is the real distinction:
// a deadline means the create wedged and the failure is genuine, while a cancel
// means a teardown stopped it deliberately and the marker is cleared instead
// (see TestBackgroundDraftPRCancellationClearsRatherThanFails).
func TestBackgroundDraftPRFailureIsRecordedWhenTheCreateContextIsDone(t *testing.T) {
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	inFlight := sessionreason.DraftPRCreationInFlight()
	sessions.sessions["sess-1"] = &models.Session{
		ID:            "sess-1",
		RepoID:        "repo-1",
		BaseBranch:    "main",
		BlockedReason: &inFlight,
	}
	lc := newTestLifecycle(
		ctxAwareSessionStore{sessions}, repos, nil, nil,
		&mockWorktreeManager{}, newMockAgentRunner(), nil, newMockVCSProvider(), zerolog.Nop(),
	)

	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if !errors.Is(expired.Err(), context.DeadlineExceeded) {
		t.Fatalf("precondition: ctx.Err() = %v, want DeadlineExceeded", expired.Err())
	}

	lc.failInFlightDraftPR(expired, "sess-1", errors.New("push branch: signal: killed"))

	sess := sessions.sessions["sess-1"]
	if sessionreason.IsDraftPRCreationInFlight(sess.BlockedReason) {
		t.Fatal("BlockedReason is still the in-flight marker; the session would advertise a PR that is never coming")
	}
	if !sessionreason.IsDraftPRCreationFailure(sess.BlockedReason) {
		t.Fatalf("BlockedReason = %v, want a draft PR creation failure", sess.BlockedReason)
	}
}

// TestClearDraftPRBlockedReasonDoesNotEraseAForeignReason pins the widened
// check-to-write window. clearDraftPRBlockedReason issues an unconditional
// `blocked_reason = NULL`, and the *models.Session it decides from is read up to
// a full push + fetch + `gh pr create` before that write once the create runs in
// the background. In that window the session's own headless run can fail and
// record a real block; the PR landing afterwards must not silently erase the
// diagnostic, leaving a Blocked session with no reason.
//
// This drives the helper directly with an explicitly stale snapshot rather than
// going through StartSession, because mockSessionStore.Get hands back the live
// map entry — the caller's "snapshot" is aliased to the row, so a full-flow test
// can never make it disagree and would pass against the unfixed code. A real
// SQLite Get scans into a fresh struct, which is the shape reproduced here.
func TestClearDraftPRBlockedReasonDoesNotEraseAForeignReason(t *testing.T) {
	sessions := newMockSessionStore()
	inFlight := sessionreason.DraftPRCreationInFlight()
	sessions.sessions["sess-1"] = &models.Session{
		ID:            "sess-1",
		RepoID:        "repo-1",
		BlockedReason: &inFlight,
	}
	lc := newTestLifecycle(
		sessions, newMockRepoStore(), nil, nil,
		&mockWorktreeManager{}, newMockAgentRunner(), nil, newMockVCSProvider(), zerolog.Nop(),
	)

	// What the background step carries: the row as it looked when the create
	// began, still showing the in-flight marker it had just written.
	stale := &models.Session{ID: "sess-1", RepoID: "repo-1", BlockedReason: &inFlight}

	// Meanwhile a different owner blocks the session for real.
	const foreign = "headless run failed: agent exited 1"
	reason := foreign
	reasonPtr := &reason
	if _, err := sessions.Update(context.Background(), "sess-1", db.UpdateSessionParams{BlockedReason: &reasonPtr}); err != nil {
		t.Fatalf("write foreign blocked reason: %v", err)
	}

	if err := lc.clearDraftPRBlockedReason(context.Background(), "sess-1", stale); err != nil {
		t.Fatalf("clearDraftPRBlockedReason: %v", err)
	}

	if got := sessions.sessions["sess-1"].BlockedReason; got == nil || *got != foreign {
		t.Fatalf("stored BlockedReason = %v, want the foreign reason %q preserved", got, foreign)
	}
	// The caller's copy adopts the stored value rather than staying stale.
	if stale.BlockedReason == nil || *stale.BlockedReason != foreign {
		t.Fatalf("caller copy BlockedReason = %v, want %q adopted from the row", stale.BlockedReason, foreign)
	}
}

// TestClearDraftPRBlockedReasonStillClearsItsOwnMarker is the positive half:
// the re-read must not turn the helper into a no-op for the case it exists to
// serve — the marker this step itself wrote is still there, so it goes.
func TestClearDraftPRBlockedReasonStillClearsItsOwnMarker(t *testing.T) {
	sessions := newMockSessionStore()
	inFlight := sessionreason.DraftPRCreationInFlight()
	sessions.sessions["sess-1"] = &models.Session{
		ID:            "sess-1",
		RepoID:        "repo-1",
		BlockedReason: &inFlight,
	}
	lc := newTestLifecycle(
		sessions, newMockRepoStore(), nil, nil,
		&mockWorktreeManager{}, newMockAgentRunner(), nil, newMockVCSProvider(), zerolog.Nop(),
	)

	stale := &models.Session{ID: "sess-1", RepoID: "repo-1", BlockedReason: &inFlight}
	if err := lc.clearDraftPRBlockedReason(context.Background(), "sess-1", stale); err != nil {
		t.Fatalf("clearDraftPRBlockedReason: %v", err)
	}
	if got := sessions.sessions["sess-1"].BlockedReason; got != nil {
		t.Fatalf("stored BlockedReason = %q, want nil", *got)
	}
}

// TestCancelBackgroundDraftPRUnblocksTheJoin pins the interlock
// cleanupFailedCreateSession depends on. That path runs under the process-global
// start mutex, so it cannot afford to wait out a push it does not want the
// result of — it cancels the step first, and the join then only covers the
// unwind.
func TestCancelBackgroundDraftPRUnblocksTheJoin(t *testing.T) {
	ctx := context.Background()
	_, wt, _, lc := newBackgroundDraftPRFixture(t)

	parked := make(chan struct{})
	wt.verifyCurrentBranchCtxHook = func(stepCtx context.Context) {
		close(parked)
		// Stand in for a git/GitHub call that honours cancellation.
		<-stepCtx.Done()
	}

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	<-parked

	lc.CancelBackgroundDraftPR("sess-1")

	joinCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := lc.WaitForBackgroundDraftPR(joinCtx, "sess-1"); err != nil {
		t.Fatalf("join after cancel: %v, want the cancelled step to unwind promptly", err)
	}
}

// TestStopBackgroundDraftPRIsCalledBeforeTeardownTouchesTheWorktree pins the
// interlock every session-teardown path needs. Before the create moved into the
// background, CreateSession did not return until the PR existed, so nothing
// could tear a session down mid-create. Now the RPC hands the client a session
// handle while the goroutine is still about to `git commit`/`git push` inside
// the worktree — so an archive that removes that directory first corrupts the
// create and can leave a remote branch and PR behind a session that is gone.
func TestStopBackgroundDraftPRIsCalledBeforeTeardownTouchesTheWorktree(t *testing.T) {
	ctx := context.Background()
	sessions, wt, _, lc := newBackgroundDraftPRFixture(t)

	// Record which happened first. Asserting only that the archive completed
	// would still pass with the stop moved AFTER worktrees.Archive — i.e.
	// against the very regression this test names.
	var orderMu sync.Mutex
	var order []string
	note := func(event string) {
		orderMu.Lock()
		order = append(order, event)
		orderMu.Unlock()
	}

	parked := make(chan struct{})
	wt.verifyCurrentBranchCtxHook = func(stepCtx context.Context) {
		close(parked)
		<-stepCtx.Done()
		note("step-cancelled")
	}
	wt.archiveHook = func() { note("worktree-archived") }

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	<-parked

	sess := sessions.sessions["sess-1"]
	sess.WorktreePath = "/tmp/worktrees/repo/sess-1"

	done := make(chan error, 1)
	go func() { done <- lc.ArchiveSession(ctx, "sess-1") }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ArchiveSession: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("ArchiveSession did not complete: it must cancel the in-flight draft PR create, not wait it out")
	}

	// Cancelled and unwound: nothing is left to race the worktree removal.
	joinCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := lc.WaitForBackgroundDraftPR(joinCtx, "sess-1"); err != nil {
		t.Fatalf("background draft PR still in flight after archive: %v", err)
	}

	orderMu.Lock()
	defer orderMu.Unlock()
	want := []string{"step-cancelled", "worktree-archived"}
	if len(order) != len(want) || order[0] != want[0] || order[1] != want[1] {
		t.Fatalf("event order = %v, want %v: the create must be stopped before its worktree is removed", order, want)
	}
}

// TestClearStaleDraftPRInFlightReasons pins the daemon-restart sweep. The
// in-flight marker means "a goroutine in THIS process is opening a PR", so a
// marker that survives a restart is provably stale — and without the sweep it
// survives forever, because every in-process exit of the step clears it itself.
// The sweep must be surgical: only the in-flight marker goes.
func TestClearStaleDraftPRInFlightReasons(t *testing.T) {
	sessions := newMockSessionStore()
	inFlight := sessionreason.DraftPRCreationInFlight()
	realBlock := "blocked: max fix attempts reached"
	failure := sessionreason.DraftPRCreationFailure(errors.New("boom"))
	sessions.sessions["stale"] = &models.Session{ID: "stale", RepoID: "repo-1", BlockedReason: &inFlight}
	sessions.sessions["blocked"] = &models.Session{ID: "blocked", RepoID: "repo-1", BlockedReason: &realBlock}
	sessions.sessions["failed"] = &models.Session{ID: "failed", RepoID: "repo-1", BlockedReason: &failure}
	sessions.sessions["clean"] = &models.Session{ID: "clean", RepoID: "repo-1"}

	lc := newTestLifecycle(
		sessions, newMockRepoStore(), nil, nil,
		&mockWorktreeManager{}, newMockAgentRunner(), nil, newMockVCSProvider(), zerolog.Nop(),
	)

	cleared, err := lc.ClearStaleDraftPRInFlightReasons(context.Background())
	if err != nil {
		t.Fatalf("ClearStaleDraftPRInFlightReasons: %v", err)
	}
	if len(cleared) != 1 || cleared[0] != "stale" {
		t.Fatalf("cleared = %v, want [stale]", cleared)
	}
	if got := sessions.sessions["stale"].BlockedReason; got != nil {
		t.Fatalf("stale session BlockedReason = %q, want nil", *got)
	}
	// Everything else keeps its reason: a real block and a recorded draft-PR
	// failure are both still true after a restart.
	if got := sessions.sessions["blocked"].BlockedReason; got == nil || *got != realBlock {
		t.Fatalf("blocked session BlockedReason = %v, want %q untouched", got, realBlock)
	}
	if got := sessions.sessions["failed"].BlockedReason; got == nil || *got != failure {
		t.Fatalf("failed session BlockedReason = %v, want %q untouched", got, failure)
	}
}

// TestStartSession_CronJobID_Persisted verifies that opts.CronJobID is
// written onto the session row so the finalize path and cron UI can
// identify which cron job produced a given session.
func TestStartSession_CronJobID_Persisted(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	chats := &mockAgentChatStore{}
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	tx := tmux.NewClient(tmux.WithCommandFactory(newFakeTmux().factory))
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
		OriginURL:         "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Nightly audit",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
		AgentName:  "claude",
	}

	lc := newTestLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, tx, newMockVCSProvider(), logger)
	lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": newFakeAgent()})
	lc.SetAgentLogsDir(t.TempDir())

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{
		DeferPR:   true,
		CronJobID: "cron-42",
	}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	awaitDraftPR(t, lc, "sess-1")

	sess := sessions.sessions["sess-1"]
	if sess.CronJobID == nil {
		t.Fatal("expected CronJobID to be persisted, got nil")
	}
	if *sess.CronJobID != "cron-42" {
		t.Errorf("CronJobID = %q, want %q", *sess.CronJobID, "cron-42")
	}
}

func TestStartSession_ZeroOutputUsesRepoCheckoutWithoutHookOrSetup(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	chats := &mockAgentChatStore{}
	wt := &mockWorktreeManager{}
	agentRunner := newMockAgentRunner()
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(newFakeTmux().factory))
	setup := "pnpm install"
	repos.repos["repo-1"] = &models.Repo{
		ID: "repo-1", LocalPath: "/tmp/repo", SetupScript: &setup,
		DefaultBaseBranch: "main", WorktreeBaseDir: "/tmp/worktrees",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID: "sess-1", RepoID: "repo-1", Title: "Zero output", Plan: "inspect status",
		BaseBranch: "main", State: machine.CreatingWorktree, AgentName: "claude",
	}
	lc := newTestLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, agentRunner, tmuxClient, newMockVCSProvider(), zerolog.Nop())
	fakeAgent := newFakeAgent()
	lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": fakeAgent})
	lc.SetAgentLogsDir(t.TempDir())

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{CronJobID: "cron-1", HookToken: "secret", DeferPR: true, ZeroOutput: true}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if len(wt.created) != 0 || len(wt.createdFromExisting) != 0 {
		t.Fatalf("zero-output created worktrees: fresh=%d existing=%d", len(wt.created), len(wt.createdFromExisting))
	}
	if fakeAgent.LastConfigureHookReq != nil {
		t.Fatal("ConfigureFinalizeHook called for zero-output session")
	}
	if lc.backgroundDraftPRIsTracked("sess-1") {
		t.Fatal("zero-output session started a draft PR")
	}
	sess := sessions.sessions["sess-1"]
	if sess.WorktreePath != "/tmp/repo" || sess.BranchName != "" || !sess.IsQuickChat {
		t.Fatalf("session = path %q branch %q quick=%v, want repo checkout, empty branch, quick chat", sess.WorktreePath, sess.BranchName, sess.IsQuickChat)
	}
	if sess.State != machine.ImplementingPlan || sess.AgentSessionID == nil {
		t.Fatalf("session did not start tmux agent: state=%s agent=%v", sess.State, sess.AgentSessionID)
	}
}

// TestStartSession_HookToken_CallsConfigureFinalizeHook verifies that when
// StartSessionOpts.HookToken is set, StartSession delegates Stop-hook
// configuration to the agent plugin via ConfigureFinalizeHook RPC,
// passing the worktree path, session ID, token, and port. The actual file
// write happens inside the plugin (tested there); lifecycle only verifies
// the RPC contract. Non-cron sessions (empty HookToken) skip this path.
func TestStartSession_HookToken_CallsConfigureFinalizeHook(t *testing.T) {
	ctx := context.Background()

	worktreeDir := t.TempDir()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	chats := &mockAgentChatStore{}
	wt := &mockWorktreeManager{worktreePath: worktreeDir}
	cr := newMockAgentRunner()
	tmuxFake := newFakeTmux()
	tx := tmux.NewClient(tmux.WithCommandFactory(tmuxFake.factory))
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
		OriginURL:         "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Nightly audit",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
		AgentName:  "claude",
	}

	fa := newFakeAgent()
	lc := newTestLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, tx, newMockVCSProvider(), logger)
	lc.SetHookPort(45678)
	lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": fa})
	lc.SetAgentLogsDir(t.TempDir())

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{
		DeferPR:   true,
		CronJobID: "cron-42",
		HookToken: "secret-token-123",
	}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	awaitDraftPR(t, lc, "sess-1")

	// Verify ConfigureFinalizeHook RPC was called with the correct args.
	req := fa.LastConfigureHookReq
	if req == nil {
		t.Fatal("ConfigureFinalizeHook was not called")
	}
	if req.WorkDir != worktreeDir {
		t.Errorf("WorkDir = %q, want %q", req.WorkDir, worktreeDir)
	}
	if req.SessionId != "sess-1" {
		t.Errorf("SessionId = %q, want %q", req.SessionId, "sess-1")
	}
	if req.HookToken != "secret-token-123" {
		t.Errorf("HookToken = %q, want %q", req.HookToken, "secret-token-123")
	}
	if req.HookPort != 45678 {
		t.Errorf("HookPort = %d, want %d", req.HookPort, 45678)
	}
}

// TestEnsurePR_Idempotent verifies EnsurePR is a no-op when the session
// already has a PR, and creates one when it doesn't — without an extra
// empty commit (unlike the StartSession up-front path, which needs one to
// make a PR-less branch diverge from base).
func TestEnsurePR_Idempotent(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	t.Run("PRAlreadyExists_NoOp", func(t *testing.T) {
		sessions := newMockSessionStore()
		repos := newMockRepoStore()
		wt := &mockWorktreeManager{}
		cr := newMockAgentRunner()
		vp := newMockVCSProvider()

		repos.repos["repo-1"] = &models.Repo{
			ID:        "repo-1",
			LocalPath: "/tmp/repo",
			OriginURL: "owner/repo",
		}
		existingPR := 7
		existingURL := "https://github.com/owner/repo/pull/7"
		sessions.sessions["sess-1"] = &models.Session{
			ID:           "sess-1",
			RepoID:       "repo-1",
			Title:        "Has PR",
			WorktreePath: "/tmp/wt",
			BranchName:   "br-1",
			BaseBranch:   "main",
			PRNumber:     &existingPR,
			PRURL:        &existingURL,
		}

		lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, vp, logger)

		if err := lc.EnsurePR(ctx, "sess-1"); err != nil {
			t.Fatalf("EnsurePR: %v", err)
		}

		if len(vp.createPRCalls) != 0 {
			t.Errorf("expected 0 createPR calls when PR exists, got %d", len(vp.createPRCalls))
		}
		if len(wt.pushed) != 0 {
			t.Errorf("expected 0 pushes when PR exists, got %d", len(wt.pushed))
		}
	})

	t.Run("NoPR_PushesAndCreates", func(t *testing.T) {
		sessions := newMockSessionStore()
		repos := newMockRepoStore()
		wt := &mockWorktreeManager{}
		cr := newMockAgentRunner()
		vp := newMockVCSProvider()

		repos.repos["repo-1"] = &models.Repo{
			ID:        "repo-1",
			LocalPath: "/tmp/repo",
			OriginURL: "owner/repo",
		}
		sessions.sessions["sess-1"] = &models.Session{
			ID:           "sess-1",
			RepoID:       "repo-1",
			Title:        "Deferred PR",
			Plan:         "Do thing",
			WorktreePath: "/tmp/wt",
			BranchName:   "br-1",
			BaseBranch:   "main",
		}

		lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, vp, logger)

		if err := lc.EnsurePR(ctx, "sess-1"); err != nil {
			t.Fatalf("EnsurePR: %v", err)
		}

		if len(wt.pushed) != 1 || wt.pushed[0] != "br-1" {
			t.Errorf("expected push of br-1, got %v", wt.pushed)
		}
		// EnsurePR must not produce an empty placeholder commit — Claude
		// already made real commits by the time finalize calls in.
		if len(wt.emptyCommits) != 0 {
			t.Errorf("EnsurePR should not make empty commits, got %d", len(wt.emptyCommits))
		}
		if len(vp.createPRCalls) != 1 {
			t.Fatalf("expected 1 createPR call, got %d", len(vp.createPRCalls))
		}
		sess := sessions.sessions["sess-1"]
		if sess.PRNumber == nil || *sess.PRNumber != 42 {
			t.Errorf("expected PRNumber = 42, got %v", sess.PRNumber)
		}

		// Second call must be a no-op (idempotency).
		prevPushes := len(wt.pushed)
		prevPRCalls := len(vp.createPRCalls)
		if err := lc.EnsurePR(ctx, "sess-1"); err != nil {
			t.Fatalf("EnsurePR second call: %v", err)
		}
		if len(wt.pushed) != prevPushes {
			t.Errorf("second EnsurePR call pushed again: got %d pushes, want %d", len(wt.pushed), prevPushes)
		}
		if len(vp.createPRCalls) != prevPRCalls {
			t.Errorf("second EnsurePR call re-created PR: got %d calls, want %d", len(vp.createPRCalls), prevPRCalls)
		}
	})
}

func TestEnsurePR_CreateDraftPRFailureLogsBranchDiagnostics(t *testing.T) {
	const prFailure = "gh pr create: GraphQL: No commits between main and br-1"

	ctx := context.Background()
	var logs bytes.Buffer
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{
		branchDebugSnapshot: &gitpkg.BranchDebugSnapshot{
			CurrentBranch: "br-1",
			HeadSHA:       "head-sha",
			RemoteHeadSHA: "remote-sha",
			AheadBehind:   "0\t1",
		},
	}
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	vp.createPRErr = errors.New(prFailure)

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo",
		OriginURL: "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		Title:        "Deferred PR",
		Plan:         "Do thing",
		WorktreePath: "/tmp/wt",
		BranchName:   "br-1",
		BaseBranch:   "main",
	}

	lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, vp, zerolog.New(&logs))

	err := lc.EnsurePR(ctx, "sess-1")
	if err == nil {
		t.Fatal("EnsurePR returned nil, want draft PR failure")
	}
	if !strings.Contains(err.Error(), "create draft PR: "+prFailure) {
		t.Fatalf("EnsurePR error = %q, want draft PR failure", err)
	}

	if len(wt.branchDebugSnapshotCalls) != 1 {
		t.Fatalf("BranchDebugSnapshot calls = %d, want 1", len(wt.branchDebugSnapshotCalls))
	}
	call := wt.branchDebugSnapshotCalls[0]
	if call.worktreePath != "/tmp/wt" || call.branch != "br-1" || call.baseBranch != "main" {
		t.Fatalf("BranchDebugSnapshot call = %+v, want path=/tmp/wt branch=br-1 base=main", call)
	}

	updated := sessions.sessions["sess-1"]
	if updated.BlockedReason == nil {
		t.Fatal("BlockedReason = nil, want draft PR failure")
	}
	if !strings.Contains(*updated.BlockedReason, "draft PR creation failed: create draft PR: "+prFailure) {
		t.Fatalf("BlockedReason = %q, want draft PR failure", *updated.BlockedReason)
	}

	var event map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var candidate map[string]any
		if err := json.Unmarshal([]byte(line), &candidate); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		if candidate["message"] == "draft PR creation failed with branch debug snapshot" {
			event = candidate
			break
		}
	}
	if event == nil {
		t.Fatalf("branch diagnostic log not found in:\n%s", logs.String())
	}
	for field, want := range map[string]string{
		"session":         "sess-1",
		"branch":          "br-1",
		"base":            "main",
		"current_branch":  "br-1",
		"head_sha":        "head-sha",
		"remote_head_sha": "remote-sha",
		"ahead_behind":    "0\t1",
	} {
		if got := event[field]; got != want {
			t.Fatalf("log field %s = %v, want %q", field, got, want)
		}
	}
	if got := fmt.Sprint(event["error"]); !strings.Contains(got, "create draft PR: "+prFailure) {
		t.Fatalf("log error = %q, want original draft PR error", got)
	}
}

func TestEnsurePR_RetriesDraftPRWithoutEmptyCommitAndClearsBlockedReason(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	agentChats := &mockAgentChatStore{}
	worktrees := &mockWorktreeManager{}
	agentRunner := newMockAgentRunner()
	provider := newMockVCSProvider()
	lifecycle := newTestLifecycle(sessions, repos, agentChats, nil, worktrees, agentRunner, nil, provider, zerolog.Nop())

	repo := &models.Repo{
		ID:              "repo-1",
		DisplayName:     "repo",
		LocalPath:       "/tmp/repo",
		OriginURL:       "git@github.com:owner/repo.git",
		WorktreeBaseDir: "/tmp/worktrees",
	}
	repos.repos[repo.ID] = repo

	reason := "draft PR creation failed: create PR: GraphQL: Head sha can't be blank"
	session := &models.Session{
		ID:            "sess-1",
		RepoID:        repo.ID,
		Title:         "Open a PR",
		Plan:          "Do useful work",
		WorktreePath:  "/tmp/worktrees/repo/open-a-pr",
		BranchName:    "open-a-pr",
		BaseBranch:    "main",
		State:         machine.ImplementingPlan,
		BlockedReason: &reason,
	}
	sessions.sessions[session.ID] = session

	provider.nextPRInfo = &vcs.PRInfo{
		Number: 77,
		URL:    "https://github.com/owner/repo/pull/77",
	}

	if err := lifecycle.EnsurePR(ctx, session.ID); err != nil {
		t.Fatalf("EnsurePR returned error: %v", err)
	}

	updated := sessions.sessions[session.ID]
	if updated.PRNumber == nil || *updated.PRNumber != 77 {
		t.Fatalf("PRNumber = %v, want 77", updated.PRNumber)
	}
	if updated.PRURL == nil || *updated.PRURL != "https://github.com/owner/repo/pull/77" {
		t.Fatalf("PRURL = %v, want expected PR URL", updated.PRURL)
	}
	if updated.BlockedReason != nil {
		t.Fatalf("BlockedReason = %q, want nil", *updated.BlockedReason)
	}
	if worktrees.emptyCommitCalls != 0 {
		t.Fatalf("emptyCommitCalls = %d, want 0", worktrees.emptyCommitCalls)
	}
	if len(provider.createPRCalls) != 1 {
		t.Fatalf("CreateDraftPR calls = %d, want 1", len(provider.createPRCalls))
	}
}

func TestEnsurePR_ClearBlockedReasonFailureKeepsAttachedPR(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	agentChats := &mockAgentChatStore{}
	worktrees := &mockWorktreeManager{}
	agentRunner := newMockAgentRunner()
	provider := newMockVCSProvider()
	lifecycle := newTestLifecycle(sessions, repos, agentChats, nil, worktrees, agentRunner, nil, provider, zerolog.Nop())

	repo := &models.Repo{
		ID:              "repo-1",
		DisplayName:     "repo",
		LocalPath:       "/tmp/repo",
		OriginURL:       "git@github.com:owner/repo.git",
		WorktreeBaseDir: "/tmp/worktrees",
	}
	repos.repos[repo.ID] = repo

	reason := "draft PR creation failed: create PR: GraphQL: Head sha can't be blank"
	session := &models.Session{
		ID:            "sess-1",
		RepoID:        repo.ID,
		Title:         "Open a PR",
		Plan:          "Do useful work",
		WorktreePath:  "/tmp/worktrees/repo/open-a-pr",
		BranchName:    "open-a-pr",
		BaseBranch:    "main",
		State:         machine.ImplementingPlan,
		BlockedReason: &reason,
	}
	sessions.sessions[session.ID] = session

	provider.nextPRInfo = &vcs.PRInfo{
		Number: 77,
		URL:    "https://github.com/owner/repo/pull/77",
	}

	clearAttempts := 0
	sessions.updateHook = func(_ string, params db.UpdateSessionParams) error {
		if params.BlockedReason != nil && *params.BlockedReason == nil {
			clearAttempts++
			return errors.New("clear failed")
		}
		return nil
	}

	if err := lifecycle.EnsurePR(ctx, session.ID); err != nil {
		t.Fatalf("EnsurePR returned error: %v", err)
	}

	updated := sessions.sessions[session.ID]
	if updated.PRNumber == nil || *updated.PRNumber != 77 {
		t.Fatalf("PRNumber = %v, want 77", updated.PRNumber)
	}
	if updated.PRURL == nil || *updated.PRURL != "https://github.com/owner/repo/pull/77" {
		t.Fatalf("PRURL = %v, want expected PR URL", updated.PRURL)
	}
	if updated.BlockedReason == nil || *updated.BlockedReason != reason {
		t.Fatalf("BlockedReason = %v, want original reason", updated.BlockedReason)
	}
	if clearAttempts != 1 {
		t.Fatalf("clear attempts = %d, want 1", clearAttempts)
	}
	if len(provider.createPRCalls) != 1 {
		t.Fatalf("CreateDraftPR calls = %d, want 1", len(provider.createPRCalls))
	}
}

func TestEnsurePR_AttachesExistingPROnDuplicateError(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	agentChats := &mockAgentChatStore{}
	worktrees := &mockWorktreeManager{}
	agentRunner := newMockAgentRunner()
	provider := newMockVCSProvider()
	lifecycle := newTestLifecycle(sessions, repos, agentChats, nil, worktrees, agentRunner, nil, provider, zerolog.Nop())

	repo := &models.Repo{
		ID:              "repo-1",
		DisplayName:     "repo",
		LocalPath:       "/tmp/repo",
		OriginURL:       "git@github.com:owner/repo.git",
		WorktreeBaseDir: "/tmp/worktrees",
	}
	repos.repos[repo.ID] = repo

	reason := "draft PR creation failed: create PR: GraphQL: Head sha can't be blank"
	session := &models.Session{
		ID:            "sess-1",
		RepoID:        repo.ID,
		Title:         "Open a PR",
		Plan:          "Do useful work",
		WorktreePath:  "/tmp/worktrees/repo/open-a-pr",
		BranchName:    "open-a-pr",
		BaseBranch:    "main",
		State:         machine.ImplementingPlan,
		BlockedReason: &reason,
	}
	sessions.sessions[session.ID] = session

	provider.createPRErr = vcs.ErrPRAlreadyExists
	provider.nextOpenPRs = []vcs.PRSummary{
		{
			Number:     42,
			Title:      "Open a PR",
			HeadBranch: "open-a-pr",
			State:      vcs.PRStateOpen,
		},
	}

	if err := lifecycle.EnsurePR(ctx, session.ID); err != nil {
		t.Fatalf("EnsurePR returned error: %v", err)
	}

	updated := sessions.sessions[session.ID]
	if updated.PRNumber == nil || *updated.PRNumber != 42 {
		t.Fatalf("PRNumber = %v, want 42", updated.PRNumber)
	}
	if updated.PRURL == nil || *updated.PRURL != "https://github.com/owner/repo/pull/42" {
		t.Fatalf("PRURL = %v, want https://github.com/owner/repo/pull/42", updated.PRURL)
	}
	if updated.BlockedReason != nil {
		t.Fatalf("BlockedReason = %q, want nil", *updated.BlockedReason)
	}
	if len(provider.createPRCalls) != 1 {
		t.Fatalf("CreateDraftPR calls = %d, want 1", len(provider.createPRCalls))
	}
}

func TestAttachOpenPRForBranch_MatchesLiveBranchAndPersists(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	agentChats := &mockAgentChatStore{}
	worktrees := &mockWorktreeManager{}
	agentRunner := newMockAgentRunner()
	provider := newMockVCSProvider()
	lifecycle := newTestLifecycle(sessions, repos, agentChats, nil, worktrees, agentRunner, nil, provider, zerolog.Nop())

	branches := &reconcileMockBranchResolver{
		branches: map[string]string{"/wt/sess-1": "dave/won-1208-foo"},
		errs:     map[string]error{},
	}
	lifecycle.SetBranchResolver(branches)

	session := &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		Title:        "WonderCanvas auto-implement",
		BranchName:   "cron-stale",
		WorktreePath: "/wt/sess-1",
	}
	repo := &models.Repo{ID: "repo-1", OriginURL: "https://github.com/owner/repo"}
	sessions.sessions[session.ID] = session
	provider.nextOpenPRs = []vcs.PRSummary{
		{
			Number:     354,
			Title:      "[WON-1208] Fix the thing",
			HeadBranch: "dave/won-1208-foo",
			State:      vcs.PRStateOpen,
		},
	}

	found, err := lifecycle.attachOpenPRForBranch(ctx, session.ID, session, repo)
	if err != nil {
		t.Fatalf("attachOpenPRForBranch: %v", err)
	}
	if !found {
		t.Fatal("expected PR to be attached via live branch")
	}
	got, err := sessions.Get(ctx, session.ID)
	if err != nil {
		t.Fatalf("Get session: %v", err)
	}
	if got.PRNumber == nil || *got.PRNumber != 354 {
		t.Fatalf("PRNumber = %v, want 354", got.PRNumber)
	}
	if got.BranchName != "dave/won-1208-foo" {
		t.Fatalf("BranchName = %q, want corrected", got.BranchName)
	}
	if got.Title != "[WON-1208] Fix the thing" {
		t.Fatalf("Title = %q, want PR title", got.Title)
	}
}

func TestAttachOpenPRForBranch_PrefersStoredBranchOverLiveBranch(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	agentChats := &mockAgentChatStore{}
	worktrees := &mockWorktreeManager{}
	agentRunner := newMockAgentRunner()
	provider := newMockVCSProvider()
	lifecycle := newTestLifecycle(sessions, repos, agentChats, nil, worktrees, agentRunner, nil, provider, zerolog.Nop())

	branches := &reconcileMockBranchResolver{
		branches: map[string]string{"/wt/sess-1": "live-branch"},
		errs:     map[string]error{},
	}
	lifecycle.SetBranchResolver(branches)

	session := &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		Title:        "Stored branch task",
		BranchName:   "stored-branch",
		WorktreePath: "/wt/sess-1",
	}
	repo := &models.Repo{ID: "repo-1", OriginURL: "https://github.com/owner/repo"}
	sessions.sessions[session.ID] = session
	provider.nextOpenPRs = []vcs.PRSummary{
		{
			Number:     200,
			Title:      "Live branch PR",
			HeadBranch: "live-branch",
			State:      vcs.PRStateOpen,
		},
		{
			Number:     100,
			Title:      "Stored branch PR",
			HeadBranch: "stored-branch",
			State:      vcs.PRStateOpen,
		},
	}

	found, err := lifecycle.attachOpenPRForBranch(ctx, session.ID, session, repo)
	if err != nil {
		t.Fatalf("attachOpenPRForBranch: %v", err)
	}
	if !found {
		t.Fatal("expected PR to be attached")
	}
	got, err := sessions.Get(ctx, session.ID)
	if err != nil {
		t.Fatalf("Get session: %v", err)
	}
	if got.PRNumber == nil || *got.PRNumber != 100 {
		t.Fatalf("PRNumber = %v, want stored-branch PR 100", got.PRNumber)
	}
	if got.BranchName != "stored-branch" {
		t.Fatalf("BranchName = %q, want stored-branch", got.BranchName)
	}
	if got.Title != "Stored branch PR" {
		t.Fatalf("Title = %q, want stored-branch PR title", got.Title)
	}
}

// TestAttachOpenPRForBranch_NonCronAdoptsDivergentPRTitle documents the
// intended behaviour on the non-cron stored-branch path: when the matched PR's
// title differs from the session's stored title, the attach adopts the PR
// title (no cron UpdatePRTitle round-trip). The PRNumber==nil precondition all
// callers enforce is what keeps this from clobbering a user edit.
func TestAttachOpenPRForBranch_NonCronAdoptsDivergentPRTitle(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	agentChats := &mockAgentChatStore{}
	worktrees := &mockWorktreeManager{}
	agentRunner := newMockAgentRunner()
	provider := newMockVCSProvider()
	lifecycle := newTestLifecycle(sessions, repos, agentChats, nil, worktrees, agentRunner, nil, provider, zerolog.Nop())

	session := &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		Title:        "original stored title",
		BranchName:   "stored-branch",
		WorktreePath: "/wt/sess-1",
	}
	repo := &models.Repo{ID: "repo-1", OriginURL: "https://github.com/owner/repo"}
	sessions.sessions[session.ID] = session
	provider.nextOpenPRs = []vcs.PRSummary{
		{
			Number:     100,
			Title:      "PR-supplied title",
			HeadBranch: "stored-branch",
			State:      vcs.PRStateOpen,
		},
	}

	found, err := lifecycle.attachOpenPRForBranch(ctx, session.ID, session, repo)
	if err != nil {
		t.Fatalf("attachOpenPRForBranch: %v", err)
	}
	if !found {
		t.Fatal("expected PR to be attached")
	}
	if len(provider.updatePRTitleCalls) != 0 {
		t.Fatalf("non-cron attach must not call UpdatePRTitle, got %v", provider.updatePRTitleCalls)
	}
	got, err := sessions.Get(ctx, session.ID)
	if err != nil {
		t.Fatalf("Get session: %v", err)
	}
	if got.BranchName != "stored-branch" {
		t.Fatalf("BranchName = %q, want stored-branch (unchanged)", got.BranchName)
	}
	if got.Title != "PR-supplied title" {
		t.Fatalf("Title = %q, want adopted PR title", got.Title)
	}
}

func TestEnsurePR_AttachesExistingCronPRAndUpdatesMessyTitle(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	agentChats := &mockAgentChatStore{}
	worktrees := &mockWorktreeManager{
		latestCommitSubject: "test(githubapp): [#493] cover malformed Link header without brackets",
	}
	agentRunner := newMockAgentRunner()
	provider := newMockVCSProvider()
	lifecycle := newTestLifecycle(sessions, repos, agentChats, nil, worktrees, agentRunner, nil, provider, zerolog.Nop())

	repo := &models.Repo{
		ID:              "repo-1",
		DisplayName:     "repo",
		LocalPath:       "/tmp/repo",
		OriginURL:       "git@github.com:owner/repo.git",
		WorktreeBaseDir: "/tmp/worktrees",
	}
	repos.repos[repo.ID] = repo

	cronJobID := "cron-1"
	session := &models.Session{
		ID:           "sess-1",
		RepoID:       repo.ID,
		Title:        "test(githubapp): [#493] cover malformed Link header without brackets",
		Plan:         "Do useful work",
		WorktreePath: "/tmp/worktrees/repo/open-a-pr",
		BranchName:   "cron-link-header-123",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
		CronJobID:    &cronJobID,
	}
	sessions.sessions[session.ID] = session

	provider.createPRErr = vcs.ErrPRAlreadyExists
	provider.nextOpenPRs = []vcs.PRSummary{
		{
			Number:     493,
			Title:      "test(githubapp): [#493] cover malformed Link header without brackets",
			HeadBranch: "cron-link-header-123",
			State:      vcs.PRStateOpen,
		},
	}

	if err := lifecycle.EnsurePR(ctx, session.ID); err != nil {
		t.Fatalf("EnsurePR returned error: %v", err)
	}

	if len(provider.updatePRTitleCalls) != 1 {
		t.Fatalf("UpdatePRTitle calls = %d, want 1", len(provider.updatePRTitleCalls))
	}
	if got, want := provider.updatePRTitleCalls[0].repoPath, "git@github.com:owner/repo.git"; got != want {
		t.Fatalf("UpdatePRTitle repoPath = %q, want %q", got, want)
	}
	if got, want := provider.updatePRTitleCalls[0].prID, 493; got != want {
		t.Fatalf("UpdatePRTitle prID = %d, want %d", got, want)
	}
	if got, want := provider.updatePRTitleCalls[0].title, "Cover malformed Link header without brackets"; got != want {
		t.Fatalf("UpdatePRTitle title = %q, want %q", got, want)
	}

	updated := sessions.sessions[session.ID]
	if updated.PRNumber == nil || *updated.PRNumber != 493 {
		t.Fatalf("PRNumber = %v, want 493", updated.PRNumber)
	}
	if got, want := updated.Title, "Cover malformed Link header without brackets"; got != want {
		t.Fatalf("Title = %q, want updated PR title %q", got, want)
	}
}

func TestEnsurePR_AttachesExistingNonCronPRWithoutUpdatingTitle(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	agentChats := &mockAgentChatStore{}
	worktrees := &mockWorktreeManager{
		latestCommitSubject: "test(githubapp): [#493] cover malformed Link header without brackets",
	}
	agentRunner := newMockAgentRunner()
	provider := newMockVCSProvider()
	lifecycle := newTestLifecycle(sessions, repos, agentChats, nil, worktrees, agentRunner, nil, provider, zerolog.Nop())

	repo := &models.Repo{
		ID:              "repo-1",
		DisplayName:     "repo",
		LocalPath:       "/tmp/repo",
		OriginURL:       "git@github.com:owner/repo.git",
		WorktreeBaseDir: "/tmp/worktrees",
	}
	repos.repos[repo.ID] = repo

	session := &models.Session{
		ID:           "sess-1",
		RepoID:       repo.ID,
		Title:        "test(githubapp): [#493] cover malformed Link header without brackets",
		Plan:         "Do useful work",
		WorktreePath: "/tmp/worktrees/repo/open-a-pr",
		BranchName:   "cron-link-header-123",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
	}
	sessions.sessions[session.ID] = session

	provider.createPRErr = vcs.ErrPRAlreadyExists
	provider.nextOpenPRs = []vcs.PRSummary{
		{
			Number:     493,
			Title:      "test(githubapp): [#493] cover malformed Link header without brackets",
			HeadBranch: "cron-link-header-123",
			State:      vcs.PRStateOpen,
		},
	}

	if err := lifecycle.EnsurePR(ctx, session.ID); err != nil {
		t.Fatalf("EnsurePR returned error: %v", err)
	}

	if len(provider.updatePRTitleCalls) != 0 {
		t.Fatalf("UpdatePRTitle calls = %d, want 0", len(provider.updatePRTitleCalls))
	}
}

func TestEnsurePR_AttachExistingCronPRTitleUpdateFailureReturnsError(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	agentChats := &mockAgentChatStore{}
	worktrees := &mockWorktreeManager{
		latestCommitSubject: "test(githubapp): [#493] cover malformed Link header without brackets",
	}
	agentRunner := newMockAgentRunner()
	provider := newMockVCSProvider()
	provider.updatePRTitleErr = errors.New("gh pr edit failed")
	lifecycle := newTestLifecycle(sessions, repos, agentChats, nil, worktrees, agentRunner, nil, provider, zerolog.Nop())

	repo := &models.Repo{
		ID:              "repo-1",
		DisplayName:     "repo",
		LocalPath:       "/tmp/repo",
		OriginURL:       "git@github.com:owner/repo.git",
		WorktreeBaseDir: "/tmp/worktrees",
	}
	repos.repos[repo.ID] = repo

	cronJobID := "cron-1"
	session := &models.Session{
		ID:           "sess-1",
		RepoID:       repo.ID,
		Title:        "test(githubapp): [#493] cover malformed Link header without brackets",
		Plan:         "Do useful work",
		WorktreePath: "/tmp/worktrees/repo/open-a-pr",
		BranchName:   "cron-link-header-123",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
		CronJobID:    &cronJobID,
	}
	sessions.sessions[session.ID] = session

	provider.createPRErr = vcs.ErrPRAlreadyExists
	provider.nextOpenPRs = []vcs.PRSummary{
		{
			Number:     493,
			Title:      "test(githubapp): [#493] cover malformed Link header without brackets",
			HeadBranch: "cron-link-header-123",
			State:      vcs.PRStateOpen,
		},
	}

	err := lifecycle.EnsurePR(ctx, session.ID)
	if err == nil || !strings.Contains(err.Error(), "update attached PR title") {
		t.Fatalf("EnsurePR error = %v, want update attached PR title failure", err)
	}

	updated := sessions.sessions[session.ID]
	if updated.PRNumber != nil {
		t.Fatalf("PRNumber = %v, want nil", updated.PRNumber)
	}
}

func TestEnsurePR_AttachExistingPRReturnsClearBlockedReasonFailure(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	agentChats := &mockAgentChatStore{}
	worktrees := &mockWorktreeManager{}
	agentRunner := newMockAgentRunner()
	provider := newMockVCSProvider()
	lifecycle := newTestLifecycle(sessions, repos, agentChats, nil, worktrees, agentRunner, nil, provider, zerolog.Nop())

	repo := &models.Repo{
		ID:              "repo-1",
		DisplayName:     "repo",
		LocalPath:       "/tmp/repo",
		OriginURL:       "git@github.com:owner/repo.git",
		WorktreeBaseDir: "/tmp/worktrees",
	}
	repos.repos[repo.ID] = repo

	reason := "draft PR creation failed: create PR: GraphQL: Head sha can't be blank"
	session := &models.Session{
		ID:            "sess-1",
		RepoID:        repo.ID,
		Title:         "Open a PR",
		Plan:          "Do useful work",
		WorktreePath:  "/tmp/worktrees/repo/open-a-pr",
		BranchName:    "open-a-pr",
		BaseBranch:    "main",
		State:         machine.ImplementingPlan,
		BlockedReason: &reason,
	}
	sessions.sessions[session.ID] = session

	provider.createPRErr = vcs.ErrPRAlreadyExists
	provider.nextOpenPRs = []vcs.PRSummary{
		{
			Number:     42,
			Title:      "Open a PR",
			HeadBranch: "open-a-pr",
			State:      vcs.PRStateOpen,
		},
	}

	clearErr := errors.New("clear failed")
	clearAttempts := 0
	sessions.updateHook = func(_ string, params db.UpdateSessionParams) error {
		if params.BlockedReason != nil && *params.BlockedReason == nil {
			clearAttempts++
			return clearErr
		}
		return nil
	}

	err := lifecycle.EnsurePR(ctx, session.ID)
	if !errors.Is(err, clearErr) {
		t.Fatalf("EnsurePR error = %v, want clear failure", err)
	}

	updated := sessions.sessions[session.ID]
	if updated.PRNumber == nil || *updated.PRNumber != 42 {
		t.Fatalf("PRNumber = %v, want 42", updated.PRNumber)
	}
	if updated.PRURL == nil || *updated.PRURL != "https://github.com/owner/repo/pull/42" {
		t.Fatalf("PRURL = %v, want https://github.com/owner/repo/pull/42", updated.PRURL)
	}
	if clearAttempts != 1 {
		t.Fatalf("clear attempts = %d, want 1", clearAttempts)
	}
	if len(provider.createPRCalls) != 1 {
		t.Fatalf("CreateDraftPR calls = %d, want 1", len(provider.createPRCalls))
	}
}

func TestNormalizeCronPRTitle_StripsConventionalCommitAndPRNumberPrefixes(t *testing.T) {
	tests := []struct {
		name    string
		subject string
		want    string
	}{
		{
			name:    "conventional commit with scope and PR number",
			subject: "test(githubapp): [#493] cover malformed Link header without brackets",
			want:    "Cover malformed Link header without brackets",
		},
		{
			name:    "conventional commit without PR number",
			subject: "fix: bump lodash",
			want:    "Bump lodash",
		},
		{
			name:    "PR number without conventional commit prefix",
			subject: "[#493] cover malformed Link header without brackets",
			want:    "Cover malformed Link header without brackets",
		},
		{
			name:    "already natural language",
			subject: "Cover malformed Link header without brackets",
			want:    "Cover malformed Link header without brackets",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeCronPRTitle(tt.subject); got != tt.want {
				t.Fatalf("normalizeCronPRTitle(%q) = %q, want %q", tt.subject, got, tt.want)
			}
		})
	}
}

func TestLinkPR_NumberUpdatesSessionAndCronLastRun(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	cron := &recordingCronJobStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "git@github.com:owner/repo.git",
	}
	cronJobID := "cron-1"
	blockedReason := "draft PR creation failed"
	sessions.sessions["sess-1"] = &models.Session{
		ID:            "sess-1",
		RepoID:        "repo-1",
		BranchName:    "cron-br-1",
		State:         machine.Blocked,
		CronJobID:     &cronJobID,
		BlockedReason: &blockedReason,
	}

	lifecycle := newTestLifecycle(sessions, repos, nil, cron, wt, cr, nil, vp, logger)
	updated, err := lifecycle.LinkPR(ctx, "sess-1", "42")
	if err != nil {
		t.Fatalf("LinkPR: %v", err)
	}

	if updated.PRNumber == nil || *updated.PRNumber != 42 {
		t.Fatalf("PRNumber = %v, want 42", updated.PRNumber)
	}
	if updated.PRURL == nil || *updated.PRURL != "https://github.com/owner/repo/pull/42" {
		t.Fatalf("PRURL = %v, want generated URL", updated.PRURL)
	}
	if updated.State != machine.AwaitingChecks {
		t.Fatalf("state = %s, want awaiting_checks", updated.State)
	}
	if updated.BlockedReason != nil {
		t.Fatalf("BlockedReason = %q, want nil", *updated.BlockedReason)
	}
	if len(vp.getPRStatusPRNumbers) != 1 || vp.getPRStatusPRNumbers[0] != 42 {
		t.Fatalf("GetPRStatus calls = %v, want [42]", vp.getPRStatusPRNumbers)
	}
	if len(cron.lastRunCalls) != 1 {
		t.Fatalf("UpdateLastRun calls = %d, want 1", len(cron.lastRunCalls))
	}
	if cron.lastRunCalls[0].sessionID == nil || *cron.lastRunCalls[0].sessionID != "sess-1" {
		t.Fatalf("last run session ID = %v, want sess-1", cron.lastRunCalls[0].sessionID)
	}
	if cron.lastRunCalls[0].outcome != models.CronJobOutcomePRCreated {
		t.Fatalf("last run outcome = %q, want pr_created", cron.lastRunCalls[0].outcome)
	}
}

func TestLinkPR_AdoptsPRTitleOnFirstAssociation(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	repos := newMockRepoStore()
	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "git@github.com:owner/repo.git",
	}

	t.Run("first association adopts the PR title", func(t *testing.T) {
		sessions := newMockSessionStore()
		vp := newMockVCSProvider()
		vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen, Title: "[BOS-12] Real ticket title"}
		sessions.sessions["sess-1"] = &models.Session{
			ID:         "sess-1",
			RepoID:     "repo-1",
			Title:      "Bossanova auto-implement",
			BranchName: "cron-br-1",
			State:      machine.AwaitingChecks,
		}

		lifecycle := newTestLifecycle(sessions, repos, nil, &recordingCronJobStore{}, &mockWorktreeManager{}, newMockAgentRunner(), nil, vp, logger)
		updated, err := lifecycle.LinkPR(ctx, "sess-1", "42")
		if err != nil {
			t.Fatalf("LinkPR: %v", err)
		}
		if updated.Title != "[BOS-12] Real ticket title" {
			t.Fatalf("title = %q, want adopted PR title", updated.Title)
		}
	})

	t.Run("re-link does not clobber an existing title", func(t *testing.T) {
		sessions := newMockSessionStore()
		vp := newMockVCSProvider()
		vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen, Title: "[BOS-99] Different PR"}
		existingPR := 7
		sessions.sessions["sess-1"] = &models.Session{
			ID:         "sess-1",
			RepoID:     "repo-1",
			Title:      "User-edited title",
			BranchName: "cron-br-1",
			State:      machine.AwaitingChecks,
			PRNumber:   &existingPR,
		}

		lifecycle := newTestLifecycle(sessions, repos, nil, &recordingCronJobStore{}, &mockWorktreeManager{}, newMockAgentRunner(), nil, vp, logger)
		updated, err := lifecycle.LinkPR(ctx, "sess-1", "42")
		if err != nil {
			t.Fatalf("LinkPR: %v", err)
		}
		if updated.Title != "User-edited title" {
			t.Fatalf("title = %q, want unchanged (re-link must not clobber)", updated.Title)
		}
	})
}

func TestLinkPR_FinalizingSessionMovesToAwaitingChecks(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	cron := &recordingCronJobStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "git@github.com:owner/repo.git",
	}
	cronJobID := "cron-1"
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		BranchName: "cron-br-1",
		State:      machine.Finalizing,
		CronJobID:  &cronJobID,
	}

	lifecycle := newTestLifecycle(sessions, repos, nil, cron, wt, cr, nil, vp, logger)
	updated, err := lifecycle.LinkPR(ctx, "sess-1", "42")
	if err != nil {
		t.Fatalf("LinkPR: %v", err)
	}

	if updated.State != machine.AwaitingChecks {
		t.Fatalf("state = %s, want awaiting_checks", updated.State)
	}
	if updated.PRNumber == nil || *updated.PRNumber != 42 {
		t.Fatalf("PRNumber = %v, want 42", updated.PRNumber)
	}
	if len(cron.lastRunCalls) != 1 {
		t.Fatalf("UpdateLastRun calls = %d, want 1", len(cron.lastRunCalls))
	}
	if cron.lastRunCalls[0].outcome != models.CronJobOutcomePRCreated {
		t.Fatalf("last run outcome = %q, want pr_created", cron.lastRunCalls[0].outcome)
	}
}

func TestLinkPR_URLRejectsWrongRepo(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "git@github.com:owner/repo.git",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:     "sess-1",
		RepoID: "repo-1",
		State:  machine.Blocked,
	}

	lifecycle := newTestLifecycle(sessions, repos, nil, &stubCronJobStore{}, wt, cr, nil, vp, logger)
	_, err := lifecycle.LinkPR(ctx, "sess-1", "https://github.com/other/repo/pull/42")
	if err == nil {
		t.Fatal("expected wrong-repo PR URL to be rejected")
	}
	if sessions.sessions["sess-1"].PRNumber != nil {
		t.Fatalf("PRNumber = %v, want nil after rejection", sessions.sessions["sess-1"].PRNumber)
	}
	if len(vp.getPRStatusPRNumbers) != 0 {
		t.Fatalf("GetPRStatus should not be called before repo validation, got %v", vp.getPRStatusPRNumbers)
	}
}

// --- Cron tmux helpers ---

// recordedTmuxCall is one captured tmux invocation (subcommand + args).
type recordedTmuxCall struct {
	subcommand string
	args       []string
}

// fakeTmux drives a *tmux.Client via WithCommandFactory so cron tmux tests
// can assert which subcommands ran and stub their exit status without
// actually invoking tmux. Specific subcommands can be made to fail via
// failSubcommand. capture-pane returns capturePaneOutput so the SendPlan
// ready-marker poll succeeds without sleeping.
type fakeTmux struct {
	mu                sync.Mutex
	calls             []recordedTmuxCall
	failSubcommand    map[string]bool // subcommand → return non-zero
	failStderr        map[string]string
	capturePaneOutput string // output for capture-pane stdout
	available         bool   // controls whether `tmux -V` succeeds
}

func newFakeTmux() *fakeTmux {
	return &fakeTmux{
		failSubcommand:    map[string]bool{},
		failStderr:        map[string]string{},
		capturePaneOutput: "Welcome to Claude\n❯\n",
		available:         true,
	}
}

// factory implements tmux.CommandFactory. It records every invocation, then
// returns a no-op exec.Cmd whose exit status reflects the configured
// subcommand-level failure flag. capture-pane returns canned stdout so the
// SendPlan ready-marker wait passes immediately.
func (f *fakeTmux) factory(ctx context.Context, name string, args ...string) *exec.Cmd {
	f.mu.Lock()
	defer f.mu.Unlock()

	subcommand := ""
	if len(args) > 0 {
		subcommand = args[0]
	}
	// Treat `tmux -V` as the availability probe, not a subcommand.
	if subcommand == "-V" {
		if !f.available {
			return exec.CommandContext(ctx, "false")
		}
		return exec.CommandContext(ctx, "true")
	}

	f.calls = append(f.calls, recordedTmuxCall{subcommand: subcommand, args: append([]string(nil), args[1:]...)})

	if f.failSubcommand[subcommand] {
		if stderr := f.failStderr[subcommand]; stderr != "" {
			return exec.CommandContext(ctx, "sh", "-c", "printf '%s' \"$1\" >&2; exit 1", "sh", stderr)
		}
		return exec.CommandContext(ctx, "false")
	}
	if subcommand == "capture-pane" {
		return exec.CommandContext(ctx, "printf", "%s", f.capturePaneOutput)
	}
	return exec.CommandContext(ctx, "true")
}

func (f *fakeTmux) hasSubcommand(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c.subcommand == name {
			return true
		}
	}
	return false
}

// enterSendKeysCount reports how many recorded send-keys calls submit the
// composer with a bare Enter (args end with "Enter"). Used to assert that a
// DeliverySubmit path presses Enter and a DeliveryPrefillOnly path does not.
func (f *fakeTmux) enterSendKeysCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.subcommand == "send-keys" && len(c.args) > 0 && c.args[len(c.args)-1] == "Enter" {
			n++
		}
	}
	return n
}

// hasLiteralSendKeys reports whether any recorded send-keys call used the "-l"
// literal flag. Used to assert literal-keystroke delivery without pinning the
// full, payload-dependent arg vector.
func (f *fakeTmux) hasLiteralSendKeys() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c.subcommand != "send-keys" {
			continue
		}
		for _, a := range c.args {
			if a == "-l" {
				return true
			}
		}
	}
	return false
}

// --- Cron tmux tests ---

// TestStartSession_CronJobID_TmuxAvailable_HappyPath verifies the cron
// branch of StartSession: when CronJobID is set and tmux is available,
// it spawns claude inside a tmux session, persists a claude_chats row,
// invokes SendPlan, and writes the new claude session ID onto the
// session row.
func TestStartSession_CronJobID_TmuxAvailable_HappyPath(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	chats := &mockAgentChatStore{}
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	fake := newFakeTmux()
	tx := tmux.NewClient(tmux.WithCommandFactory(fake.factory))
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
		OriginURL:         "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Nightly audit",
		Plan:       "Run the audit",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
		AgentName:  "claude",
	}

	lc := newTestLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, tx, newMockVCSProvider(), logger)
	lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": newFakeAgent()})
	lc.SetAgentLogsDir(t.TempDir())

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{
		DeferPR:   true,
		CronJobID: "cron-42",
	}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	awaitDraftPR(t, lc, "sess-1")

	// The headless claude path must NOT have run.
	if len(cr.started) != 0 {
		t.Errorf("expected 0 headless claude.Start calls on cron path, got %d", len(cr.started))
	}

	// tmux new-session must have been issued with claude --session-id ...
	var newSessArgs []string
	for _, c := range fake.calls {
		if c.subcommand == "new-session" {
			newSessArgs = c.args
			break
		}
	}
	if newSessArgs == nil {
		t.Fatal("expected tmux new-session call, none recorded")
	}
	joined := strings.Join(newSessArgs, " ")
	if !strings.Contains(joined, "claude --session-id ") {
		t.Errorf("expected new-session args to contain `claude --session-id ...`, got: %s", joined)
	}

	// claude_chats.Create must have been called once with a matching ClaudeID
	// and the cron-style title.
	if len(chats.createCalls) != 1 {
		t.Fatalf("expected 1 agentChats.Create call, got %d", len(chats.createCalls))
	}
	createdAgentSessionID := chats.createCalls[0].AgentSessionID
	if createdAgentSessionID == "" {
		t.Error("expected non-empty ClaudeID on agentChats.Create")
	}
	if chats.createCalls[0].Title != `Run "Nightly audit"` {
		t.Errorf(`Title = %q, want %q`, chats.createCalls[0].Title, `Run "Nightly audit"`)
	}
	if chats.createCalls[0].SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want sess-1", chats.createCalls[0].SessionID)
	}

	// UpdateTmuxSessionName persisted the resolved tmux name onto the chat row.
	if len(chats.tmuxNameUpdates) != 1 {
		t.Fatalf("expected 1 UpdateTmuxSessionName call, got %d", len(chats.tmuxNameUpdates))
	}
	if chats.tmuxNameUpdates[0].agentSessionID != createdAgentSessionID {
		t.Errorf("UpdateTmuxSessionName agentSessionID = %q, want %q", chats.tmuxNameUpdates[0].agentSessionID, createdAgentSessionID)
	}
	if chats.tmuxNameUpdates[0].name == nil || *chats.tmuxNameUpdates[0].name == "" {
		t.Error("expected non-nil/non-empty tmux name persisted on chat row")
	}

	// SendPlan must have run. A single-line plan ("Run the audit") delivers via
	// literal keystrokes (send-keys -l), not bracketed paste.
	if fake.hasSubcommand("paste-buffer") {
		t.Error("single-line plan must not use bracketed paste")
	}
	if !fake.hasLiteralSendKeys() {
		t.Error("expected literal send-keys -l delivery from SendPlan, none recorded")
	}

	// The new claude session UUID was persisted on the session row.
	sess := sessions.sessions["sess-1"]
	if sess.AgentSessionID == nil || *sess.AgentSessionID != createdAgentSessionID {
		t.Errorf("session.AgentSessionID = %v, want %q", sess.AgentSessionID, createdAgentSessionID)
	}
	if sess.State != machine.ImplementingPlan {
		t.Errorf("session.State = %v, want ImplementingPlan", sess.State)
	}
}

// TestStartSession_TmuxUnattended_RoutesToTmux verifies that a tmux_unattended
// session (opts.IsTmuxUnattended=true, NO CronJobID) routes through the durable
// tmux-hosted path — a tmux new-session spawning claude, an agent_chats row with
// the PLAIN session title (not the cron `Run "<name>"` title) — and NOT the
// headless StartByAgent path.
func TestStartSession_TmuxUnattended_RoutesToTmux(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	chats := &mockAgentChatStore{}
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	fake := newFakeTmux()
	tx := tmux.NewClient(tmux.WithCommandFactory(fake.factory))
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
		OriginURL:         "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Epic child task",
		Plan:       "Do the epic work",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
		AgentName:  "claude",
	}

	lc := newTestLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, tx, newMockVCSProvider(), logger)
	lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": newFakeAgent()})
	lc.SetAgentLogsDir(t.TempDir())

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{
		DeferPR:          true,
		IsTmuxUnattended: true,
	}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	awaitDraftPR(t, lc, "sess-1")

	// The headless StartByAgent path must NOT have run.
	if len(cr.started) != 0 {
		t.Errorf("expected 0 headless StartByAgent calls on tmux_unattended path, got %d", len(cr.started))
	}

	// tmux new-session must have spawned claude.
	if !fake.hasSubcommand("new-session") {
		t.Fatal("expected tmux new-session call, none recorded")
	}

	// An agent_chats row must have been created with the PLAIN session title
	// (tmux_unattended keeps the session title; only cron gets `Run "<name>"`).
	if len(chats.createCalls) != 1 {
		t.Fatalf("expected 1 agentChats.Create call, got %d", len(chats.createCalls))
	}
	if got, want := chats.createCalls[0].Title, "Epic child task"; got != want {
		t.Errorf("Title = %q, want plain session title %q", got, want)
	}

	// The IsTmuxUnattended flag was persisted.
	if !sessions.sessions["sess-1"].IsTmuxUnattended {
		t.Error("expected IsTmuxUnattended persisted on the session row")
	}
	if sessions.sessions["sess-1"].State != machine.ImplementingPlan {
		t.Errorf("session.State = %v, want ImplementingPlan", sessions.sessions["sess-1"].State)
	}
}

// TestStartSession_TmuxUnattended_HookToken_ConfiguresFinalizeHook verifies that
// a tmux_unattended session started with a HookToken installs its finalize Stop
// hook (ConfigureFinalizeHook fires) and persists the token onto the session
// row, so a daemon restart can re-arm it. This is the arming the server does
// when it mints a HookToken for CreateSession {tmux_unattended:true}.
func TestStartSession_TmuxUnattended_HookToken_ConfiguresFinalizeHook(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	chats := &mockAgentChatStore{}
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	fake := newFakeTmux()
	tx := tmux.NewClient(tmux.WithCommandFactory(fake.factory))
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
		OriginURL:         "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Epic child task",
		Plan:       "Do the epic work",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
		AgentName:  "claude",
	}

	fakeAgent := newFakeAgent()
	lc := newTestLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, tx, newMockVCSProvider(), logger)
	lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": fakeAgent})
	lc.SetAgentLogsDir(t.TempDir())
	lc.SetHookPort(45678)

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{
		DeferPR:          true,
		IsTmuxUnattended: true,
		HookToken:        "hooktok-123",
	}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	awaitDraftPR(t, lc, "sess-1")

	if len(fakeAgent.ConfigureHookReqs) == 0 {
		t.Fatal("expected ConfigureFinalizeHook to fire for a HookToken session")
	}
	req := fakeAgent.ConfigureHookReqs[0]
	if req.GetHookToken() != "hooktok-123" {
		t.Errorf("ConfigureFinalizeHook HookToken = %q, want hooktok-123", req.GetHookToken())
	}
	if req.GetHookPort() != 45678 {
		t.Errorf("ConfigureFinalizeHook HookPort = %d, want 45678", req.GetHookPort())
	}
	if sessions.sessions["sess-1"].HookToken == nil || *sessions.sessions["sess-1"].HookToken != "hooktok-123" {
		t.Errorf("expected HookToken persisted on session, got %v", sessions.sessions["sess-1"].HookToken)
	}
}

// TestStartSession_DetachTmuxAvailable_RoutesToTmux is BOS-428's core proof: a
// `boss new --detach` run with tmux available is hosted in a durable tmux pane
// (routed through startTmuxChat, NOT the paneless headless StartByAgent path),
// persists sessions.detach = true, installs its finalize Stop hook (so the
// completion gate admits it and a restart can re-arm it), and does NOT arm the
// headless poll fallback (the Stop hook drives completion for a hook-supporting
// agent). This is the tmux-hosted branch that survives a daemon restart.
func TestStartSession_DetachTmuxAvailable_RoutesToTmux(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	chats := &mockAgentChatStore{}
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	fake := newFakeTmux() // available by default
	tx := tmux.NewClient(tmux.WithCommandFactory(fake.factory))
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
		OriginURL:         "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Detached run",
		Plan:       "Do the detached work",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
		AgentName:  "claude",
	}

	fakeAgent := newFakeAgent() // IsSupported defaults true (claude owns a hook)
	armer := &fakePollArmer{}
	lc := newTestLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, tx, newMockVCSProvider(), logger)
	lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": fakeAgent})
	lc.SetAgentLogsDir(t.TempDir())
	lc.SetHookPort(45678)
	lc.SetPollArmer(armer)
	lc.SetDaemonCtx(ctx)

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{
		DeferPR:   true,
		Detach:    true,
		HookToken: "detach-tok",
	}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	awaitDraftPR(t, lc, "sess-1")

	// Routed through the durable tmux path, not the paneless headless one.
	if len(cr.started) != 0 {
		t.Errorf("expected 0 headless StartByAgent calls on detach-via-tmux path, got %d", len(cr.started))
	}
	if !fake.hasSubcommand("new-session") {
		t.Fatal("expected tmux new-session call for a tmux-hosted detach run, none recorded")
	}

	// Detach was persisted (unattended-class recovery signal).
	if !sessions.sessions["sess-1"].Detach {
		t.Error("expected Detach persisted on the session row for a tmux-hosted detach run")
	}

	// The finalize Stop hook was installed and its token persisted.
	if len(fakeAgent.ConfigureHookReqs) == 0 {
		t.Fatal("expected ConfigureFinalizeHook to fire for a tmux-hosted detach run")
	}
	if got := fakeAgent.ConfigureHookReqs[0].GetHookToken(); got != "detach-tok" {
		t.Errorf("ConfigureFinalizeHook HookToken = %q, want detach-tok", got)
	}
	if sessions.sessions["sess-1"].HookToken == nil || *sessions.sessions["sess-1"].HookToken != "detach-tok" {
		t.Errorf("expected HookToken persisted on session, got %v", sessions.sessions["sess-1"].HookToken)
	}

	// A hook-supporting agent's Stop hook drives completion; the headless poll
	// fallback must NOT be armed (it would double-finalize).
	if armer.armCalled {
		t.Errorf("headless poll fallback must not be armed for a tmux-hosted detach run, armed=%v", armer.armedSessions)
	}

	if sessions.sessions["sess-1"].State != machine.ImplementingPlan {
		t.Errorf("session.State = %v, want ImplementingPlan", sessions.sessions["sess-1"].State)
	}
}

// TestStartSession_CronJobID_TmuxUnavailable_Errors verifies that when
// tmux is not available, the cron branch returns an error before any
// tmux session is created or any claude_chats row is written. The
// scheduler turns this into a fire_failed cron outcome.
func TestStartSession_CronJobID_TmuxUnavailable_Errors(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	chats := &mockAgentChatStore{}
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	fake := newFakeTmux()
	fake.available = false
	tx := tmux.NewClient(tmux.WithCommandFactory(fake.factory))
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
		OriginURL:         "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Nightly audit",
		Plan:       "Run the audit",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
		AgentName:  "claude",
	}

	lc := newTestLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, tx, newMockVCSProvider(), logger)
	lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": newFakeAgent()})
	lc.SetAgentLogsDir(t.TempDir())

	err := lc.StartSession(ctx, "sess-1", StartSessionOpts{
		DeferPR:   true,
		CronJobID: "cron-42",
	})
	awaitDraftPR(t, lc, "sess-1")
	if err == nil {
		t.Fatal("expected error when tmux is unavailable on cron path")
	}
	if !strings.Contains(err.Error(), "tmux unavailable") {
		t.Errorf("expected tmux-unavailable error, got: %v", err)
	}

	// No new-session call should have been issued.
	if fake.hasSubcommand("new-session") {
		t.Error("expected no tmux new-session call when tmux unavailable")
	}
	// No claude_chats row should have been created.
	if len(chats.createCalls) != 0 {
		t.Errorf("expected 0 agentChats.Create calls when tmux unavailable, got %d", len(chats.createCalls))
	}
}

// TestStartSession_CronJobID_ChatCreateFails_KillsTmux verifies the
// cleanup contract: if claude_chats.Create fails after tmux NewSession
// already succeeded, the tmux session is killed so we don't leave a
// running claude process orphaned from any DB row.
func TestStartSession_CronJobID_ChatCreateFails_KillsTmux(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	chats := &mockAgentChatStore{
		createErr: fmt.Errorf("simulated DB failure"),
	}
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	fake := newFakeTmux()
	tx := tmux.NewClient(tmux.WithCommandFactory(fake.factory))
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
		OriginURL:         "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Nightly audit",
		Plan:       "Run the audit",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
		AgentName:  "claude",
	}

	lc := newTestLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, tx, newMockVCSProvider(), logger)
	lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": newFakeAgent()})
	lc.SetAgentLogsDir(t.TempDir())

	err := lc.StartSession(ctx, "sess-1", StartSessionOpts{
		DeferPR:   true,
		CronJobID: "cron-42",
	})
	awaitDraftPR(t, lc, "sess-1")
	if err == nil {
		t.Fatal("expected error when claude_chats.Create fails")
	}

	if !fake.hasSubcommand("new-session") {
		t.Error("expected tmux new-session call before chat create failure")
	}
	if len(chats.createCalls) != 1 {
		t.Errorf("expected 1 agentChats.Create attempt, got %d", len(chats.createCalls))
	}
	if !fake.hasSubcommand("kill-session") {
		t.Error("expected tmux kill-session call to clean up after chat create failure")
	}
}

// TestFinalizeNoChanges_KillsChatTmuxSessionsBeforeDelete verifies the
// cleanup ordering: tmux must be torn down BEFORE the session row is
// deleted, because claude_chats.session_id is ON DELETE CASCADE — once
// the row is gone the tmux_session_name is unrecoverable.
func TestFinalizeNoChanges_KillsChatTmuxSessionsBeforeDelete(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{statusOut: ""} // empty = no changes
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()

	tmuxName := "boss-repo1234-claude01"
	chats := &mockAgentChatStore{
		chatsBySession: map[string][]*models.AgentChat{
			"sess-1": {{
				ID:              "chat-claude-01",
				SessionID:       "sess-1",
				AgentSessionID:  "claude-01",
				TmuxSessionName: &tmuxName,
			}},
		},
	}

	// Track the order of operations: kill-session must happen before sessions.Delete.
	var (
		op             atomic.Int32
		killOpIdx      atomic.Int32
		deleteOpIdx    atomic.Int32
		fakeTmuxClient = &fakeTmux{
			failSubcommand:    map[string]bool{},
			available:         true,
			capturePaneOutput: "",
		}
	)
	killOpIdx.Store(-1)
	deleteOpIdx.Store(-1)

	tx := tmux.NewClient(tmux.WithCommandFactory(func(ctx context.Context, name string, args ...string) *exec.Cmd {
		cmd := fakeTmuxClient.factory(ctx, name, args...)
		if len(args) > 0 && args[0] == "kill-session" {
			killOpIdx.CompareAndSwap(-1, op.Add(1))
		}
		return cmd
	}))

	// Wrap sessions to record when Delete is called relative to kill-session.
	wrappedSessions := &orderingSessionStore{
		mockSessionStore: sessions,
		onDelete: func(_ string) {
			deleteOpIdx.CompareAndSwap(-1, op.Add(1))
		},
	}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
	}
	cronJobID := "cron-1"
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		WorktreePath: "/tmp/wt-sess1",
		State:        machine.ImplementingPlan,
		CronJobID:    &cronJobID,
	}

	lc := newTestLifecycle(wrappedSessions, repos, chats, &stubCronJobStore{}, wt, cr, tx, vp, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}
	if res.Outcome != models.CronJobOutcomeDeletedNoChanges {
		t.Fatalf("outcome = %q, want %q", res.Outcome, models.CronJobOutcomeDeletedNoChanges)
	}

	// kill-session must have run, and must have run before sessions.Delete.
	if killOpIdx.Load() < 0 {
		t.Fatal("expected tmux kill-session to be invoked for chat with TmuxSessionName")
	}
	if deleteOpIdx.Load() < 0 {
		t.Fatal("expected sessions.Delete to be invoked")
	}
	if killOpIdx.Load() >= deleteOpIdx.Load() {
		t.Errorf("expected kill-session (op %d) to run before sessions.Delete (op %d)",
			killOpIdx.Load(), deleteOpIdx.Load())
	}
}

// orderingSessionStore wraps mockSessionStore to invoke a callback when
// Delete fires, so the finalize-cleanup-order test can record sequencing.
type orderingSessionStore struct {
	*mockSessionStore
	onDelete func(id string)
}

func (o *orderingSessionStore) Delete(ctx context.Context, id string) error {
	if o.onDelete != nil {
		o.onDelete(id)
	}
	return o.mockSessionStore.Delete(ctx, id)
}

// TestSetAgents_RoutesByAgentName verifies that a Lifecycle wired with
// multiple agent clients routes ConfigureFinalizeHook to the client whose
// name matches the session's AgentName — the production multi-agent path
// when more than one bossd-plugin-<agent> is installed.
func TestSetAgents_RoutesByAgentName(t *testing.T) {
	ctx := context.Background()
	worktreeDir := t.TempDir()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	chats := &mockAgentChatStore{}
	wt := &mockWorktreeManager{worktreePath: worktreeDir}
	cr := newMockAgentRunner()
	tmuxFake := newFakeTmux()
	tx := tmux.NewClient(tmux.WithCommandFactory(tmuxFake.factory))
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
		OriginURL:         "owner/repo",
	}
	sessions.sessions["sess-opencode"] = &models.Session{
		ID:         "sess-opencode",
		RepoID:     "repo-1",
		Title:      "opencode session",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
		AgentName:  "opencode",
	}

	claudeAgent := newFakeAgent()
	openCodeAgent := newFakeAgent()

	lc := newTestLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, tx, newMockVCSProvider(), logger)
	lc.SetHookPort(45678)
	lc.SetAgents(map[string]agent.AgentRunnerClient{
		"claude":   claudeAgent,
		"opencode": openCodeAgent,
	})
	lc.SetAgentLogsDir(t.TempDir())

	if err := lc.StartSession(ctx, "sess-opencode", StartSessionOpts{
		DeferPR:   true,
		CronJobID: "cron-1",
		HookToken: "tok",
	}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	awaitDraftPR(t, lc, "sess-opencode")

	if openCodeAgent.LastConfigureHookReq == nil {
		t.Fatal("expected ConfigureFinalizeHook on opencode agent")
	}
	if claudeAgent.LastConfigureHookReq != nil {
		t.Fatal("did not expect ConfigureFinalizeHook on claude agent")
	}
}

// TestSetAgents_UnknownAgentErrors verifies that StartSession surfaces a
// clear error when the session's AgentName has no matching client in the
// registry — defense in depth in case CreateSession ever persists a name
// for which no plugin is loaded.
func TestSetAgents_UnknownAgentErrors(t *testing.T) {
	ctx := context.Background()
	worktreeDir := t.TempDir()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	chats := &mockAgentChatStore{}
	wt := &mockWorktreeManager{worktreePath: worktreeDir}
	cr := newMockAgentRunner()
	tx := tmux.NewClient(tmux.WithCommandFactory(newFakeTmux().factory))
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
		OriginURL:         "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Unknown agent",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
		AgentName:  "ghost",
	}

	claudeAgent := newFakeAgent()
	lc := newTestLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, tx, newMockVCSProvider(), logger)
	lc.SetHookPort(45678)
	lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": claudeAgent})

	err := lc.StartSession(ctx, "sess-1", StartSessionOpts{
		DeferPR:   true,
		CronJobID: "cron-1",
		HookToken: "tok",
	})
	awaitDraftPR(t, lc, "sess-1")
	if err == nil {
		t.Fatal("expected error when AgentName has no registered client")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error %q should mention the missing agent name", err)
	}
}

// TestStartSessionArmsPollFallbackForHooklessNonCronRun verifies that a
// detach run whose tmux is UNAVAILABLE falls back to the paneless headless path
// and arms daemon-side poll fallback so the session is driven out of
// ImplementingPlan on completion. Even though the server minted a HookToken,
// StartSession gates hook install on the tmux-hosted branch, so the fallback
// installs no Stop hook (hookResp nil) and PollFallback — not a hook — must
// drive completion. (With tmux available a detach run is tmux-hosted instead;
// see TestStartSession_DetachTmuxAvailable_RoutesToTmux.)
func TestStartSessionArmsPollFallbackForHooklessNonCronRun(t *testing.T) {
	ctx := context.Background()
	worktreeDir := t.TempDir()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	chats := &mockAgentChatStore{}
	wt := &mockWorktreeManager{worktreePath: worktreeDir}
	cr := newMockAgentRunner()
	noTmux := newFakeTmux()
	noTmux.available = false // tmux unavailable → detach falls back to headless
	tx := tmux.NewClient(tmux.WithCommandFactory(noTmux.factory))

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
		OriginURL:         "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Hookless audit",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
		AgentName:  "codex",
	}

	fa := newFakeAgent()
	fa.IsSupported = false // hookless agent (e.g. codex)

	armer := &fakePollArmer{}
	lc := newTestLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, tx, newMockVCSProvider(), zerolog.Nop())
	lc.SetHookPort(45678)
	lc.SetAgents(map[string]agent.AgentRunnerClient{"codex": fa})
	lc.SetAgentLogsDir(t.TempDir())
	lc.SetPollArmer(armer)
	lc.SetDaemonCtx(ctx)

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{
		Detach:    true,
		DeferPR:   true,
		HookToken: "tok-1",
	}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	awaitDraftPR(t, lc, "sess-1")

	if !armer.armCalled {
		t.Fatal("poll fallback not armed for hookless non-cron headless run")
	}
	if armer.armedSessionID != "sess-1" {
		t.Errorf("armed session id = %q, want sess-1", armer.armedSessionID)
	}
	agentSessionID := sessions.sessions["sess-1"].AgentSessionID
	if agentSessionID == nil || armer.armedID != *agentSessionID {
		t.Errorf("armed agent session id = %q, want %v", armer.armedID, agentSessionID)
	}
}

// TestStartSessionArmsPollFallbackForHooklessNonCronRunWithoutHookToken pins
// the detach fallback path when tmux is unavailable AND no HookToken is passed:
// NO Stop hook is installed (hookResp == nil) for ANY agent on the headless
// branch, and Detach is left false (fallback stays in the headless class).
// Without arming, the run is stranded in ImplementingPlan forever.
func TestStartSessionArmsPollFallbackForHooklessNonCronRunWithoutHookToken(t *testing.T) {
	ctx := context.Background()
	worktreeDir := t.TempDir()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	chats := &mockAgentChatStore{}
	wt := &mockWorktreeManager{worktreePath: worktreeDir}
	cr := newMockAgentRunner()
	noTmux := newFakeTmux()
	noTmux.available = false // tmux unavailable → detach falls back to headless
	tx := tmux.NewClient(tmux.WithCommandFactory(noTmux.factory))

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
		OriginURL:         "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Detached codex run",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
		AgentName:  "codex",
	}

	fa := newFakeAgent()
	fa.IsSupported = false
	armer := &fakePollArmer{}
	lc := newTestLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, tx, newMockVCSProvider(), zerolog.Nop())
	lc.SetAgents(map[string]agent.AgentRunnerClient{"codex": fa})
	lc.SetAgentLogsDir(t.TempDir())
	lc.SetPollArmer(armer)
	lc.SetDaemonCtx(ctx)

	// No HookToken: the ConfigureFinalizeHook probe never runs, so hookResp is
	// nil. The gate must treat nil as "no competing hook" and arm.
	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{Detach: true, DeferPR: true}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	awaitDraftPR(t, lc, "sess-1")

	if !armer.armCalled {
		t.Fatal("poll fallback not armed for hookless non-cron headless run without HookToken")
	}
	if fa.LastConfigureHookReq != nil {
		t.Error("ConfigureFinalizeHook must not be probed when no HookToken is set")
	}
	// The fallback (tmux-unavailable) detach run stays in the headless class:
	// Detach must be left false so the restart orphan sweep still reaps it.
	if sessions.sessions["sess-1"].Detach {
		t.Error("detach fallback (tmux unavailable) must leave Detach=false")
	}
}

// TestSignalSessionRunCompleteCleanExitFinalizesHeadlessRun verifies that a
// clean (exitError=="") completion of a non-cron ImplementingPlan session
// drives FinalizeSession, advancing the session out of ImplementingPlan.
func TestSignalSessionRunCompleteCleanExitFinalizesHeadlessRun(t *testing.T) {
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{statusOut: ""} // empty = no changes → deleted_no_changes
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main", // different from worktree so Archive runs
	}
	runID := "run-1"
	sessions.sessions["sess-1"] = &models.Session{
		ID:             "sess-1",
		RepoID:         "repo-1",
		WorktreePath:   "/tmp/wt-sess1",
		State:          machine.ImplementingPlan,
		AgentName:      "codex",
		AgentSessionID: &runID,
	}

	lc := newTestLifecycle(sessions, repos, nil, &stubCronJobStore{}, wt, cr, nil, vp, zerolog.Nop())

	lc.SignalSessionRunComplete("sess-1", "run-1", "")

	if sess, ok := sessions.sessions["sess-1"]; ok && sess.State == machine.ImplementingPlan {
		t.Fatalf("clean headless exit must advance session out of ImplementingPlan, still %s", sess.State)
	}
}

// TestSignalSessionRunCompleteFailedExitBlocksHeadlessRun verifies a failed
// completion blocks the session with a non-empty, non-secret reason and never
// advances it to Finalizing.
func TestSignalSessionRunCompleteFailedExitBlocksHeadlessRun(t *testing.T) {
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()

	runID := "run-1"
	sessions.sessions["sess-1"] = &models.Session{
		ID:             "sess-1",
		RepoID:         "repo-1",
		WorktreePath:   "/tmp/wt-sess1",
		State:          machine.ImplementingPlan,
		AgentName:      "codex",
		AgentSessionID: &runID,
	}

	lc := newTestLifecycle(sessions, repos, nil, &stubCronJobStore{}, wt, cr, nil, vp, zerolog.Nop())

	lc.SignalSessionRunComplete("sess-1", "run-1", "boom: command exited 1")

	sess := sessions.sessions["sess-1"]
	if sess == nil {
		t.Fatal("failed headless exit must not delete the session")
	}
	if sess.State != machine.Blocked {
		t.Fatalf("failed headless exit state = %s, want Blocked", sess.State)
	}
	if sess.BlockedReason == nil || strings.TrimSpace(*sess.BlockedReason) == "" {
		t.Fatal("failed headless exit must set a non-empty BlockedReason")
	}
}

// TestHeadlessRunBlockedReason_RedactsMultilineAndCaps locks the security
// contract of the failed-exit reason summary: only the first line survives (so
// stack traces / multi-line agent output that may carry secrets never reach the
// session row) and the result is rune-truncated to a bounded length.
func TestHeadlessRunBlockedReason_RedactsMultilineAndCaps(t *testing.T) {
	t.Run("keeps only the first line", func(t *testing.T) {
		got := headlessRunBlockedReason("auth failed: bad token\nAPI_KEY=sk-secret-value\nstack trace line")
		if strings.Contains(got, "API_KEY") || strings.Contains(got, "\n") {
			t.Fatalf("reason leaked subsequent lines: %q", got)
		}
		if !strings.Contains(got, "auth failed: bad token") {
			t.Fatalf("reason dropped the first line: %q", got)
		}
	})
	t.Run("empty exit error yields a generic reason", func(t *testing.T) {
		if got := headlessRunBlockedReason("   "); strings.TrimSpace(got) == "" {
			t.Fatalf("empty exit error must still produce a non-empty reason, got %q", got)
		}
	})
	t.Run("rune-truncates an over-long single line", func(t *testing.T) {
		got := headlessRunBlockedReason(strings.Repeat("é", 500))
		if n := utf8.RuneCountInString(got); n > headlessRunBlockedReasonMaxLen+len("headless agent run failed: ")+1 {
			t.Fatalf("reason not bounded: %d runes", n)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("reason truncated mid-rune (invalid UTF-8): %q", got)
		}
	})
}

// TestSignalSessionRunCompleteCronAndAdvancedAreNoOps verifies the headless
// finalize path never touches a cron session (the cron gate owns it) and is a
// no-op on a session already advanced past ImplementingPlan (duplicate signal).
func TestSignalSessionRunCompleteCronAndAdvancedAreNoOps(t *testing.T) {
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{statusOut: ""}
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()

	cronID := "cron-1"
	runCron := "run-cron"
	sessions.sessions["sess-cron"] = &models.Session{
		ID:             "sess-cron",
		RepoID:         "repo-1",
		WorktreePath:   "/tmp/wt-cron",
		State:          machine.ImplementingPlan,
		AgentName:      "codex",
		AgentSessionID: &runCron,
		CronJobID:      &cronID,
	}
	runAdv := "run-adv"
	sessions.sessions["sess-adv"] = &models.Session{
		ID:             "sess-adv",
		RepoID:         "repo-1",
		WorktreePath:   "/tmp/wt-adv",
		State:          machine.AwaitingChecks, // already advanced
		AgentName:      "codex",
		AgentSessionID: &runAdv,
	}

	lc := newTestLifecycle(sessions, repos, nil, &stubCronJobStore{}, wt, cr, nil, vp, zerolog.Nop())

	lc.SignalSessionRunComplete("sess-cron", "run-cron", "")
	lc.SignalSessionRunComplete("sess-adv", "run-adv", "")

	if got := sessions.sessions["sess-cron"].State; got != machine.ImplementingPlan {
		t.Fatalf("cron session must be untouched by headless finalize, state=%s", got)
	}
	if got := sessions.sessions["sess-adv"].State; got != machine.AwaitingChecks {
		t.Fatalf("already-advanced session must be a no-op, state=%s", got)
	}
}

// TestBootstrapDoesNotReArmHeadlessImplementingRuns pins the deliberate choice
// NOT to re-arm paneless headless ImplementingPlan runs on daemon restart. The
// agent plugin's run-state map is in-memory, so after a restart ExitStatus
// reports a phantom clean exit (IsComplete=true, ExitError="") for every
// historical run; re-arming PollFallback would then clean-finalize runs that
// were actually interrupted by the restart (premature ready / lost failures /
// hard-delete). Bootstrap must leave such rows stranded-but-visible instead.
func TestBootstrapDoesNotReArmHeadlessImplementingRuns(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()

	runH := "run-h"
	sessions.sessions["sess-headless"] = &models.Session{
		ID:             "sess-headless",
		RepoID:         "repo-1",
		WorktreePath:   "/tmp/wt-h",
		BaseBranch:     "main",
		State:          machine.ImplementingPlan,
		AgentName:      "codex",
		AgentSessionID: &runH,
	}

	chats := &mockAgentChatStore{} // no live tmux chats
	fa := newFakeAgent()
	fa.IsSupported = false
	armer := &fakePollArmer{}
	lc := newTestLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, nil, newMockVCSProvider(), zerolog.Nop())
	lc.SetAgents(map[string]agent.AgentRunnerClient{"codex": fa})
	lc.SetPollArmer(armer)
	// Session reported dead so the orphan sweep engages.
	lc.SetSessionLiveness(fakeSessionLiveness{running: map[string]bool{}})
	lc.SetDaemonCtx(ctx)

	lc.Bootstrap(ctx)

	if armer.armCalled {
		t.Fatalf("Bootstrap must NOT re-arm a paneless headless ImplementingPlan run (phantom-clean-exit risk); armed %v", armer.armedSessions)
	}
	// The run is never re-armed, but a restart-killed headless run must not be
	// left stranded-and-silent: the orphan sweep marks it ORPHANED.
	if got := sessions.sessions["sess-headless"].State; got != machine.Orphaned {
		t.Fatalf("dead headless run must be marked Orphaned on restart, got %s", got)
	}
	if r := sessions.sessions["sess-headless"].BlockedReason; r == nil || *r == "" {
		t.Fatalf("orphaned headless run must carry a reason, got %v", r)
	}
}

func TestBootstrapKeepsHeadlessRunImplementingWhenOrphanMarkerCannotPersist(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	runID := "run-marker-write-failure"
	sessions.sessions["sess-headless"] = &models.Session{
		ID:             "sess-headless",
		RepoID:         "repo-1",
		WorktreePath:   "/tmp/wt-h",
		BaseBranch:     "main",
		State:          machine.ImplementingPlan,
		AgentName:      "codex",
		AgentSessionID: &runID,
	}
	sessions.updateHook = func(_ string, params db.UpdateSessionParams) error {
		if params.BlockedReason != nil {
			return errors.New("blocked reason write failed")
		}
		return nil
	}

	lc := newTestLifecycle(sessions, repos, &mockAgentChatStore{}, &stubCronJobStore{}, wt, cr, nil, newMockVCSProvider(), zerolog.Nop())
	lc.SetSessionLiveness(fakeSessionLiveness{running: map[string]bool{}})
	lc.SetDaemonCtx(ctx)

	lc.Bootstrap(ctx)

	if got := sessions.sessions["sess-headless"].State; got != machine.ImplementingPlan {
		t.Fatalf("state after failed orphan marker write = %s, want ImplementingPlan", got)
	}
}

// TestBootstrapDoesNotOrphanLiveHeadlessRun pins the conservative rule: a headless
// ImplementingPlan session whose liveness reports still-alive (e.g. an interactive
// run whose tmux pane survived the restart) is left untouched, never orphaned.
func TestBootstrapDoesNotOrphanLiveHeadlessRun(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()

	runH := "run-live"
	sessions.sessions["sess-live"] = &models.Session{
		ID:             "sess-live",
		RepoID:         "repo-1",
		WorktreePath:   "/tmp/wt-live",
		BaseBranch:     "main",
		State:          machine.ImplementingPlan,
		AgentName:      "codex",
		AgentSessionID: &runH,
	}

	chats := &mockAgentChatStore{}
	lc := newTestLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, nil, newMockVCSProvider(), zerolog.Nop())
	lc.SetAgents(map[string]agent.AgentRunnerClient{"codex": newFakeAgent()})
	lc.SetSessionLiveness(fakeSessionLiveness{running: map[string]bool{"sess-live": true}})
	lc.SetDaemonCtx(ctx)

	lc.Bootstrap(ctx)

	if got := sessions.sessions["sess-live"].State; got != machine.ImplementingPlan {
		t.Fatalf("a live headless run must not be orphaned, got %s", got)
	}
}

// TestBootstrapRetriesDeferredBaseSyncs pins that Bootstrap flushes any base-branch
// syncs that were deferred (e.g. because the worktree wasn't clean at merge time) by
// calling RetryDeferredBaseSyncs exactly once during startup.
func TestBootstrapRetriesDeferredBaseSyncs(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()

	chats := &mockAgentChatStore{}
	lc := newTestLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, nil, newMockVCSProvider(), zerolog.Nop())
	lc.SetAgents(map[string]agent.AgentRunnerClient{"codex": newFakeAgent()})
	lc.SetDaemonCtx(ctx)

	lc.Bootstrap(ctx)

	if wt.retryDeferredBaseSyncsCalls != 1 {
		t.Fatalf("Bootstrap must retry deferred base syncs exactly once, got %d", wt.retryDeferredBaseSyncsCalls)
	}
}

// TestBootstrapDoesNotOrphanWithoutLiveness pins fail-toward-not-orphaning when the
// liveness checker is unwired: with no way to confirm death, the session stays put.
func TestBootstrapDoesNotOrphanWithoutLiveness(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()

	runH := "run-nolive"
	sessions.sessions["sess-nolive"] = &models.Session{
		ID:             "sess-nolive",
		RepoID:         "repo-1",
		WorktreePath:   "/tmp/wt-nolive",
		BaseBranch:     "main",
		State:          machine.ImplementingPlan,
		AgentName:      "codex",
		AgentSessionID: &runH,
	}

	chats := &mockAgentChatStore{}
	lc := newTestLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, nil, newMockVCSProvider(), zerolog.Nop())
	lc.SetAgents(map[string]agent.AgentRunnerClient{"codex": newFakeAgent()})
	// No SetSessionLiveness — liveness unwired.
	lc.SetDaemonCtx(ctx)

	lc.Bootstrap(ctx)

	if got := sessions.sessions["sess-nolive"].State; got != machine.ImplementingPlan {
		t.Fatalf("without liveness the sweep must not orphan, got %s", got)
	}
}

// TestBootstrapDoesNotOrphanUnattended pins that cron / tmux_unattended sessions —
// tmux-hosted and owned by the re-arm loop + stranded-cron sweep — are never
// treated as headless orphans, even when liveness reports them dead.
func TestBootstrapDoesNotOrphanUnattended(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()

	runH := "run-cron"
	cronID := "cron-1"
	sessions.sessions["sess-cron"] = &models.Session{
		ID:             "sess-cron",
		RepoID:         "repo-1",
		WorktreePath:   "/tmp/wt-cron",
		BaseBranch:     "main",
		State:          machine.ImplementingPlan,
		AgentName:      "codex",
		AgentSessionID: &runH,
		CronJobID:      &cronID,
	}

	chats := &mockAgentChatStore{}
	lc := newTestLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, nil, newMockVCSProvider(), zerolog.Nop())
	lc.SetAgents(map[string]agent.AgentRunnerClient{"codex": newFakeAgent()})
	lc.SetSessionLiveness(fakeSessionLiveness{running: map[string]bool{}}) // dead
	lc.SetDaemonCtx(ctx)

	lc.Bootstrap(ctx)

	if got := sessions.sessions["sess-cron"].State; got != machine.ImplementingPlan {
		t.Fatalf("unattended session must not be orphaned by the headless sweep, got %s", got)
	}
}

// TestBootstrapDoesNotOrphanDetach is BOS-428's restart-survival proof at the
// sweep boundary: a tmux-hosted `boss new --detach` run (Detach=true, non-cron)
// joins the unattended class, so sweepOrphanedHeadlessRuns skips it — it is
// re-monitored by the re-arm loop, not orphaned — even when liveness reports it
// dead. Contrast TestBootstrapDoesNotReArmHeadlessImplementingRuns, where a
// Detach=false paneless fallback run IS swept to Orphaned.
func TestBootstrapDoesNotOrphanDetach(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()

	runH := "run-detach"
	sessions.sessions["sess-detach"] = &models.Session{
		ID:             "sess-detach",
		RepoID:         "repo-1",
		WorktreePath:   "/tmp/wt-detach",
		BaseBranch:     "main",
		State:          machine.ImplementingPlan,
		AgentName:      "codex",
		AgentSessionID: &runH,
		Detach:         true,
	}

	chats := &mockAgentChatStore{}
	lc := newTestLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, nil, newMockVCSProvider(), zerolog.Nop())
	lc.SetAgents(map[string]agent.AgentRunnerClient{"codex": newFakeAgent()})
	lc.SetSessionLiveness(fakeSessionLiveness{running: map[string]bool{}}) // dead
	lc.SetDaemonCtx(ctx)

	lc.Bootstrap(ctx)

	if got := sessions.sessions["sess-detach"].State; got != machine.ImplementingPlan {
		t.Fatalf("tmux-hosted detach session must not be orphaned by the headless sweep, got %s", got)
	}
}

// TestBootstrapDoesNotPollHooklessTmuxRuns verifies that on daemon restart,
// Lifecycle.Bootstrap does not poll plugin ExitStatus for tmux-hosted rows
// whose agent reports IsSupported=false.
func TestBootstrapDoesNotPollHooklessTmuxRuns(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	tmuxFake := newFakeTmux()
	tx := tmux.NewClient(tmux.WithCommandFactory(tmuxFake.factory))

	tok := "tok-3"
	cronID := "cron-1"
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		Title:        "Some session",
		WorktreePath: "/tmp/wt",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
		AgentName:    "codex",
		HookToken:    &tok,
		CronJobID:    &cronID,
	}

	tmuxName := "tmux-x"
	chats := &mockAgentChatStore{
		chatsWithTmux: []*models.AgentChat{
			{
				ID:              "chat-1",
				SessionID:       "sess-1",
				AgentSessionID:  "run-1",
				AgentName:       "codex",
				TmuxSessionName: &tmuxName,
			},
		},
	}

	fa := newFakeAgent()
	fa.IsSupported = false

	armer := &fakePollArmer{}
	notifier := &recordingCronCompletionNotifier{}
	lc := newTestLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, tx, newMockVCSProvider(), zerolog.Nop())
	lc.SetHookPort(45678)
	lc.SetAgents(map[string]agent.AgentRunnerClient{"codex": fa})
	lc.SetAgentLogsDir(t.TempDir())
	lc.SetPollArmer(armer)
	lc.SetCronCompletionNotifier(notifier)
	lc.SetDaemonCtx(ctx)
	lc.tmuxCompletionPollInterval = time.Millisecond

	lc.Bootstrap(ctx)

	if armer.armCalled {
		t.Errorf("Bootstrap armed plugin poll fallback for tmux-hosted run %q", armer.armedID)
	}
	tmuxFake.mu.Lock()
	tmuxFake.failSubcommand["has-session"] = true
	tmuxFake.failStderr["has-session"] = "can't find session"
	tmuxFake.mu.Unlock()

	waitForCount(t, "NotifyCronAgentStopped", notifier.count)
	calls := notifier.callsCopy()
	if calls[0] != "sess-1" {
		t.Fatalf("NotifyCronAgentStopped called with %q, want sess-1", calls[0])
	}
}

// TestBootstrapDoesNotArmForHookedAgents verifies the cache prevents
// arming when the agent reports IsSupported=true (claude).
func TestBootstrapDoesNotArmForHookedAgents(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()

	tok := "tok-3"
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		Title:        "Some session",
		WorktreePath: "/tmp/wt",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
		AgentName:    "claude",
		HookToken:    &tok,
	}

	tmuxName := "tmux-c"
	chats := &mockAgentChatStore{
		chatsWithTmux: []*models.AgentChat{
			{
				ID:              "chat-1",
				SessionID:       "sess-1",
				AgentSessionID:  "run-c",
				AgentName:       "claude",
				TmuxSessionName: &tmuxName,
			},
		},
	}

	fa := newFakeAgent() // IsSupported defaults to true
	armer := &fakePollArmer{}
	lc := newTestLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, nil, newMockVCSProvider(), zerolog.Nop())
	lc.SetHookPort(45678)
	lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": fa})
	lc.SetAgentLogsDir(t.TempDir())
	lc.SetPollArmer(armer)
	lc.SetDaemonCtx(ctx)

	lc.Bootstrap(ctx)

	if armer.armCalled {
		t.Error("Bootstrap should NOT arm for hooked agents (claude)")
	}
	if fa.LastConfigureHookReq == nil {
		t.Fatal("Bootstrap should probe ConfigureFinalizeHook")
	}
	if fa.LastConfigureHookReq.GetAgentSessionId() != "" {
		t.Errorf("bootstrap hook probe AgentSessionId = %q, want empty", fa.LastConfigureHookReq.GetAgentSessionId())
	}
}

// TestBootstrapReConfiguresHookForEveryWorktree pins the invariant that when
// multiple hook-supporting (claude) cron chats survive a daemon restart,
// Bootstrap calls ConfigureFinalizeHook for EACH one. The hook config is
// written per-worktree and carries this daemon's restarted port; caching the
// per-agent support result must not skip the rewrite for chats after the
// first, or those worktrees keep the previous daemon's dead port and their
// Stop hook never reaches the restarted daemon.
func TestBootstrapReConfiguresHookForEveryWorktree(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()

	tok1 := "tok-1"
	tok2 := "tok-2"
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		WorktreePath: "/tmp/wt-1",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
		AgentName:    "claude",
		HookToken:    &tok1,
	}
	sessions.sessions["sess-2"] = &models.Session{
		ID:           "sess-2",
		RepoID:       "repo-1",
		WorktreePath: "/tmp/wt-2",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
		AgentName:    "claude",
		HookToken:    &tok2,
	}

	tmux1 := "tmux-1"
	tmux2 := "tmux-2"
	chats := &mockAgentChatStore{
		chatsWithTmux: []*models.AgentChat{
			{ID: "chat-1", SessionID: "sess-1", AgentSessionID: "run-1", AgentName: "claude", TmuxSessionName: &tmux1},
			{ID: "chat-2", SessionID: "sess-2", AgentSessionID: "run-2", AgentName: "claude", TmuxSessionName: &tmux2},
		},
	}

	fa := newFakeAgent() // IsSupported defaults to true
	lc := newTestLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, nil, newMockVCSProvider(), zerolog.Nop())
	lc.SetHookPort(45678)
	lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": fa})
	lc.SetAgentLogsDir(t.TempDir())
	lc.SetDaemonCtx(ctx)

	lc.Bootstrap(ctx)

	if len(fa.ConfigureHookReqs) != 2 {
		t.Fatalf("ConfigureFinalizeHook calls = %d, want 2 (one per surviving worktree)", len(fa.ConfigureHookReqs))
	}
	gotWorkDirs := map[string]bool{}
	for _, req := range fa.ConfigureHookReqs {
		gotWorkDirs[req.GetWorkDir()] = true
		if req.GetHookPort() != 45678 {
			t.Errorf("ConfigureFinalizeHook HookPort = %d, want 45678", req.GetHookPort())
		}
	}
	if !gotWorkDirs["/tmp/wt-1"] || !gotWorkDirs["/tmp/wt-2"] {
		t.Errorf("ConfigureFinalizeHook worktrees = %v, want both /tmp/wt-1 and /tmp/wt-2", gotWorkDirs)
	}
}

// TestBootstrapReArmsTmuxUnattendedButNotHeadless is BOS-208's #1 required
// proof: after a daemon restart, Bootstrap re-arms completion for a
// tmux_unattended + HookToken session (it is tmux-hosted, just like a cron
// session) exactly as it would for a cron session, while a headless,
// paneless, hookless ImplementingPlan run is left alone. Both rows are seeded
// in a single Bootstrap call so the test proves the CONTRAST, not just that
// one flavor works in isolation.
func TestBootstrapReArmsTmuxUnattendedButNotHeadless(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()

	// Session A: tmux_unattended (e.g. a /boss-epic child), carries a HookToken
	// like a cron session does, and has a live tmux-hosted chat surviving the
	// restart.
	tokA := "tok-a"
	runA := "run-a"
	sessions.sessions["sess-tu"] = &models.Session{
		ID:               "sess-tu",
		RepoID:           "repo-1",
		WorktreePath:     "/tmp/wt-tu",
		BaseBranch:       "main",
		State:            machine.ImplementingPlan,
		AgentName:        "claude",
		HookToken:        &tokA,
		AgentSessionID:   &runA,
		IsTmuxUnattended: true,
	}

	// Session B: headless (boss new, non-cron, non-tmux_unattended) run — no
	// HookToken, no live tmux pane to survive the restart.
	runB := "run-b"
	sessions.sessions["sess-headless"] = &models.Session{
		ID:               "sess-headless",
		RepoID:           "repo-1",
		WorktreePath:     "/tmp/wt-headless",
		BaseBranch:       "main",
		State:            machine.ImplementingPlan,
		AgentName:        "claude",
		AgentSessionID:   &runB,
		IsTmuxUnattended: false,
	}

	tmuxA := "tmux-tu"
	chats := &mockAgentChatStore{
		chatsWithTmux: []*models.AgentChat{
			// Only session A has a surviving tmux-hosted chat; session B is
			// paneless (no chat here), matching a headless run's shape.
			{ID: "chat-tu", SessionID: "sess-tu", AgentSessionID: runA, AgentName: "claude", TmuxSessionName: &tmuxA},
		},
	}

	fa := newFakeAgent() // IsSupported defaults to true (claude owns a finalize hook)
	lc := newTestLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, nil, newMockVCSProvider(), zerolog.Nop())
	lc.SetHookPort(45678)
	lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": fa})
	lc.SetAgentLogsDir(t.TempDir())
	lc.SetDaemonCtx(ctx)

	lc.Bootstrap(ctx)

	if len(fa.ConfigureHookReqs) != 1 {
		t.Fatalf("ConfigureFinalizeHook calls = %d, want 1 (only the tmux_unattended session)", len(fa.ConfigureHookReqs))
	}
	req := fa.ConfigureHookReqs[0]
	if req.GetWorkDir() != "/tmp/wt-tu" || req.GetSessionId() != "sess-tu" {
		t.Errorf("ConfigureFinalizeHook called for WorkDir=%q SessionId=%q, want /tmp/wt-tu / sess-tu (tmux_unattended session A)",
			req.GetWorkDir(), req.GetSessionId())
	}
	for _, r := range fa.ConfigureHookReqs {
		if r.GetWorkDir() == "/tmp/wt-headless" || r.GetSessionId() == "sess-headless" {
			t.Errorf("Bootstrap must NOT re-arm the headless paneless session, but called ConfigureFinalizeHook for it: %+v", r)
		}
	}
	if got := sessions.sessions["sess-headless"].State; got != machine.ImplementingPlan {
		t.Errorf("headless run must be left in ImplementingPlan on restart, got %s", got)
	}
}

// TestStartSessionDoesNotArmPollFallbackWhenHookSupported verifies that
// for agents that own a finalize hook (claude), StartSession does NOT arm
// the poll fallback — the Stop hook drives completion directly.
func TestStartSessionDoesNotArmPollFallbackWhenHookSupported(t *testing.T) {
	ctx := context.Background()
	worktreeDir := t.TempDir()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	chats := &mockAgentChatStore{}
	wt := &mockWorktreeManager{worktreePath: worktreeDir}
	cr := newMockAgentRunner()
	tx := tmux.NewClient(tmux.WithCommandFactory(newFakeTmux().factory))

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
		OriginURL:         "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Hooked agent",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
		AgentName:  "claude",
	}

	fa := newFakeAgent() // IsSupported defaults to true
	armer := &fakePollArmer{}
	lc := newTestLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, tx, newMockVCSProvider(), zerolog.Nop())
	lc.SetHookPort(45678)
	lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": fa})
	lc.SetAgentLogsDir(t.TempDir())
	lc.SetPollArmer(armer)
	lc.SetDaemonCtx(ctx)

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{
		DeferPR:   true,
		HookToken: "tok-1",
	}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	awaitDraftPR(t, lc, "sess-1")

	if armer.armCalled {
		t.Error("poll fallback should NOT be armed when hook is supported")
	}
}

func TestStartSession_DeferPRTrue_HookSupportedDoesNotArmPollFallback(t *testing.T) {
	ctx := context.Background()
	worktreeDir := t.TempDir()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	chats := &mockAgentChatStore{}
	wt := &mockWorktreeManager{worktreePath: worktreeDir}
	cr := newMockAgentRunner()
	tx := tmux.NewClient(tmux.WithCommandFactory(newFakeTmux().factory))

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
		OriginURL:         "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Hooked cron agent",
		Plan:       "do work",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
		AgentName:  "claude",
	}

	client := newFakeAgent()
	pollArmer := &fakePollArmer{}
	lc := newTestLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, tx, newMockVCSProvider(), zerolog.Nop())
	lc.SetHookPort(45678)
	lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": client})
	lc.SetAgentLogsDir(t.TempDir())
	lc.SetPollArmer(pollArmer)
	lc.SetDaemonCtx(ctx)

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{DeferPR: true, CronJobID: "cron-1", HookToken: "tok-1"}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	awaitDraftPR(t, lc, "sess-1")
	if pollArmer.armCalled {
		t.Fatal("hook-supported cron agents should use hook notification, not poll fallback")
	}
}

func TestStartSession_DeferPRTrue_HooklessAgentDoesNotArmPollFallback(t *testing.T) {
	ctx := context.Background()
	worktreeDir := t.TempDir()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	chats := &mockAgentChatStore{}
	wt := &mockWorktreeManager{worktreePath: worktreeDir}
	cr := newMockAgentRunner()
	tmuxFake := newFakeTmux()
	tx := tmux.NewClient(tmux.WithCommandFactory(tmuxFake.factory))

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
		OriginURL:         "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Hookless cron agent",
		Plan:       "do work",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
		AgentName:  "codex",
	}

	client := newFakeAgent()
	client.IsSupported = false

	pollArmer := &fakePollArmer{}
	notifier := &recordingCronCompletionNotifier{}
	lc := newTestLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, tx, newMockVCSProvider(), zerolog.Nop())
	lc.newTmuxChatAgentSessionID = func() string { return "agent-1" }
	lc.tmuxCompletionPollInterval = time.Millisecond
	lc.SetHookPort(45678)
	lc.SetAgents(map[string]agent.AgentRunnerClient{"codex": client})
	lc.SetAgentLogsDir(t.TempDir())
	lc.SetPollArmer(pollArmer)
	lc.SetCronCompletionNotifier(notifier)
	lc.SetDaemonCtx(ctx)

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{DeferPR: true, CronJobID: "cron-1", HookToken: "tok-1"}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	awaitDraftPR(t, lc, "sess-1")
	agentSessionID := sessions.sessions["sess-1"].AgentSessionID
	if agentSessionID == nil {
		t.Fatal("primary cron agent session id missing")
	}
	if pollArmer.armCalled {
		t.Fatalf("poll fallback armed for tmux-hosted hookless cron run %q", *agentSessionID)
	}
	tmuxFake.mu.Lock()
	tmuxFake.failSubcommand["has-session"] = true
	tmuxFake.failStderr["has-session"] = "can't find session"
	tmuxFake.mu.Unlock()

	waitForCount(t, "NotifyCronAgentStopped", notifier.count)
	calls := notifier.callsCopy()
	if calls[0] != "sess-1" {
		t.Fatalf("NotifyCronAgentStopped called with %q, want sess-1", calls[0])
	}
}

// labeledRunner records the "<name>:<agentSessionID>" tag on each Start so
// lifecycle tests can assert which underlying runner the dispatcher routed
// to. This mirrors the helper in services/bossd/internal/agent/dispatcher_test.go
// but is local to the session package because Go does not export test helpers
// across packages. Other AgentRunner methods are no-op stubs.
type labeledRunner struct {
	name        string
	startSeen   atomic.Pointer[string] // "<name>:<agentSessionID>" set on each Start
	profileSeen atomic.Int32           // last profile passed to a profiled call
}

func newLabeledRunner(name string) *labeledRunner {
	return &labeledRunner{name: name}
}

func (r *labeledRunner) Start(_ context.Context, _, _ string, _ *string, agentSessionID, _ string, _ map[string]string) (string, error) {
	tag := r.name + ":" + agentSessionID
	r.startSeen.Store(&tag)
	if agentSessionID == "" {
		return r.name + "-generated-id", nil
	}
	return agentSessionID, nil
}

// PreflightHeadlessCapabilityProfile / StartWithHeadlessCapabilityProfile make
// labeledRunner profile-aware so an unattended codex launch — which the launch
// policy now requires TRACKER_PLAN_ATTACHMENT_V1 for — still exercises real
// dispatcher routing instead of failing closed on a runner that predates the
// profile seam. Both record what they saw so routing assertions still hold.
func (r *labeledRunner) PreflightHeadlessCapabilityProfile(_ context.Context, _ string, _ map[string]string, profile pb.HeadlessCapabilityProfile) error {
	r.profileSeen.Store(int32(profile))
	return nil
}

func (r *labeledRunner) StartWithHeadlessCapabilityProfile(ctx context.Context, workDir, plan string, resume *string, agentSessionID, model string, extraEnv map[string]string, profile pb.HeadlessCapabilityProfile) (string, error) {
	r.profileSeen.Store(int32(profile))
	return r.Start(ctx, workDir, plan, resume, agentSessionID, model, extraEnv)
}
func (r *labeledRunner) Stop(_ string) error      { return nil }
func (r *labeledRunner) IsRunning(_ string) bool  { return false }
func (r *labeledRunner) ExitError(_ string) error { return nil }
func (r *labeledRunner) Subscribe(_ context.Context, _ string) (<-chan agent.OutputLine, error) {
	ch := make(chan agent.OutputLine)
	close(ch)
	return ch, nil
}
func (r *labeledRunner) History(_ string) []agent.OutputLine { return nil }

// TestStartSession_RoutesToCodexWhenSessionAgentNameIsCodex is the regression
// test for the agent-selection bug: a session whose AgentName="codex" must
// have its headless run dispatched to the codex plugin runner, not the
// default claude runner. Before the fix in commits 171246a0 / a8656b9e /
// f0bf5858 the lifecycle called Start(..., "") on the dispatcher's plain
// Start path, which fell back to defaultAgent="claude" and silently routed
// every codex session to claude.
//
// This test wires a *real* agent.Dispatcher (not the package-local
// mockAgentRunner forwarder, which discards agentName) into the Lifecycle so
// we exercise both the lifecycle-side StartByAgent call site AND the
// dispatcher-side routing in one go.
func TestStartSession_RoutesToCodexWhenSessionAgentNameIsCodex(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	logger := zerolog.Nop()

	// Build a real Dispatcher with two named runners. The lookup func is
	// only consulted by the legacy Start path; the lifecycle uses
	// StartByAgent which bypasses lookup entirely.
	claudeRunner := newLabeledRunner("claude")
	codexRunner := newLabeledRunner("codex")
	registry := map[string]agent.AgentRunner{
		"claude": claudeRunner,
		"codex":  codexRunner,
	}
	dispatcher := agent.NewDispatcher(registry, func(string) (string, error) {
		t.Fatalf("lookup must not be consulted on the StartByAgent path")
		return "", nil
	}, "claude", logger)

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Codex Session",
		Plan:       "Build something with codex",
		BaseBranch: "main",
		AgentName:  "codex",
		State:      machine.CreatingWorktree,
	}

	lc := newTestLifecycle(sessions, repos, nil, nil, wt, dispatcher, nil, newMockVCSProvider(), logger)

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{Detach: true}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	awaitDraftPR(t, lc, "sess-1")

	// Codex must have seen Start; claude must not. The empty agentSessionID
	// is what the lifecycle passes for fresh runs (the runner generates one
	// on its own).
	if seen := codexRunner.startSeen.Load(); seen == nil {
		t.Errorf("codex runner did not see Start")
	} else if *seen != "codex:" {
		t.Errorf("codex runner saw Start with unexpected tag %q, want %q", *seen, "codex:")
	}
	if seen := claudeRunner.startSeen.Load(); seen != nil {
		t.Errorf("claude runner unexpectedly saw Start: %q (routing regression: codex session leaked to claude)", *seen)
	}

	// The launch policy makes this unattended codex run a profiled one, so the
	// profiled dispatch must route to codex too — and must not reach claude.
	if got := codexRunner.profileSeen.Load(); got != int32(pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1) {
		t.Errorf("codex runner profile = %d, want %d (tracker-plan-attachment-v1)", got, int32(pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1))
	}
	if got := claudeRunner.profileSeen.Load(); got != int32(pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_UNSPECIFIED) {
		t.Errorf("claude runner saw profile %d, want none (profiled dispatch leaked to claude)", got)
	}

	// Sanity-check that the session was advanced and the codex runner's
	// generated ID is what got persisted — guards against a future
	// refactor that "fixes" routing but loses the returned session ID.
	sess := sessions.sessions["sess-1"]
	if sess.State != machine.ImplementingPlan {
		t.Errorf("session state = %v, want ImplementingPlan", sess.State)
	}
	if sess.AgentSessionID == nil || *sess.AgentSessionID != "codex-generated-id" {
		t.Errorf("session.AgentSessionID = %v, want codex-generated-id", sess.AgentSessionID)
	}
}

func TestStartSession_CreatesAgentChatRowForHeadlessRun(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	chats := &mockAgentChatStore{}
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Test Session",
		Plan:       "Do something",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
		AgentName:  "codex",
	}

	lc := newTestLifecycle(sessions, repos, chats, nil, wt, cr, nil, newMockVCSProvider(), logger)

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{Detach: true}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	awaitDraftPR(t, lc, "sess-1")

	if len(chats.createCalls) != 1 {
		t.Fatalf("expected 1 agent_chats Create, got %d", len(chats.createCalls))
	}
	got := chats.createCalls[0]
	if got.SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want sess-1", got.SessionID)
	}
	if got.AgentSessionID != "claude-123" { // newMockAgentRunner().nextID
		t.Errorf("AgentSessionID = %q, want claude-123", got.AgentSessionID)
	}
	if got.AgentName != "codex" {
		t.Errorf("AgentName = %q, want codex", got.AgentName)
	}
	if got.Title != "Test Session" {
		t.Errorf("Title = %q, want Test Session", got.Title)
	}
}

func TestStartSession_TrackerSessionCreatesNoChatRow(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	chats := &mockAgentChatStore{}
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()

	repos.repos["repo-1"] = &models.Repo{
		ID: "repo-1", LocalPath: "/tmp/repo", DefaultBaseBranch: "main", WorktreeBaseDir: "/tmp/worktrees",
	}
	tracker := "FRE-1"
	sessions.sessions["sess-1"] = &models.Session{
		ID: "sess-1", RepoID: "repo-1", Title: "Tracker", Plan: "x", BaseBranch: "main",
		State: machine.CreatingWorktree, AgentName: "codex", TrackerID: &tracker,
	}

	lc := newTestLifecycle(sessions, repos, chats, nil, wt, cr, nil, newMockVCSProvider(), zerolog.Nop())
	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	awaitDraftPR(t, lc, "sess-1")
	if len(chats.createCalls) != 0 {
		t.Fatalf("tracker session must not create an agent_chats row, got %d", len(chats.createCalls))
	}
}

// --- Headless run status watcher helpers ---

type recordingChatStatus struct {
	mu      sync.Mutex
	updates []recordedStatus
}

type recordedStatus struct {
	id     string
	status pb.ChatStatus
}

func (r *recordingChatStatus) Update(id string, st pb.ChatStatus, _ time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updates = append(r.updates, recordedStatus{id: id, status: st})
}

func (r *recordingChatStatus) snapshot() []recordedStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedStatus(nil), r.updates...)
}

func (r *recordingChatStatus) waitForStatus(t *testing.T, status pb.ChatStatus, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	got := 0
	for time.Now().Before(deadline) {
		got = 0
		for _, update := range r.snapshot() {
			if update.status == status {
				got++
			}
		}
		if got >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d %v updates; got %d", want, status, got)
}

// flipRunner reports the run as running for the first `trueFor` IsRunningByAgent
// calls, then not-running. Embeds mockAgentRunner for the rest of the interface.
type flipRunner struct {
	*mockAgentRunner
	calls   atomic.Int32
	trueFor int32
}

func (f *flipRunner) IsRunningByAgent(_, _ string) bool {
	return f.calls.Add(1) <= f.trueFor
}

type mutableRunner struct {
	*mockAgentRunner
	running atomic.Bool
}

func (r *mutableRunner) IsRunningByAgent(_, _ string) bool { return r.running.Load() }

func TestWatchHeadlessRunStatus_NilQuestionSignalsPreservesWorkingThenStopped(t *testing.T) {
	cr := &flipRunner{mockAgentRunner: newMockAgentRunner(), trueFor: 3}
	rec := &recordingChatStatus{}
	lc := newTestLifecycle(newMockSessionStore(), newMockRepoStore(), &mockAgentChatStore{}, nil, &mockWorktreeManager{}, cr, nil, newMockVCSProvider(), zerolog.Nop())
	lc.SetChatStatus(rec)
	lc.headlessStatusPollInterval = time.Millisecond

	lc.watchHeadlessRunStatus("codex", "agent-1")

	got := rec.snapshot()
	if len(got) < 2 {
		t.Fatalf("want >=2 status updates, got %d (%+v)", len(got), got)
	}
	if got[0].status != pb.ChatStatus_CHAT_STATUS_WORKING || got[0].id != "agent-1" {
		t.Errorf("first update = %+v, want WORKING/agent-1", got[0])
	}
	last := got[len(got)-1]
	if last.status != pb.ChatStatus_CHAT_STATUS_STOPPED || last.id != "agent-1" {
		t.Errorf("last update = %+v, want STOPPED/agent-1", last)
	}
}

// TestWatchHeadlessRunStatus_RefreshesWorking guards the fix that re-stamps
// WORKING on every positive poll. Without it the lone initial WORKING update
// ages past status.StaleThreshold and GetBatch reports the still-running chat
// as STOPPED. With trueFor=3 the loop sees the run alive three times, so there
// must be more than one WORKING update before the terminal STOPPED.
func TestWatchHeadlessRunStatus_RefreshesWorking(t *testing.T) {
	cr := &flipRunner{mockAgentRunner: newMockAgentRunner(), trueFor: 3}
	rec := &recordingChatStatus{}
	lc := newTestLifecycle(newMockSessionStore(), newMockRepoStore(), &mockAgentChatStore{}, nil, &mockWorktreeManager{}, cr, nil, newMockVCSProvider(), zerolog.Nop())
	lc.SetChatStatus(rec)
	lc.headlessStatusPollInterval = time.Millisecond

	lc.watchHeadlessRunStatus("codex", "agent-1")

	working := 0
	for _, u := range rec.snapshot() {
		if u.status == pb.ChatStatus_CHAT_STATUS_WORKING {
			working++
		}
	}
	if working < 2 {
		t.Errorf("WORKING updates = %d, want >=2 (initial + refresh on each positive poll)", working)
	}
}

func TestWatchHeadlessRunStatus_QuestionSignalSurvivesRefresh(t *testing.T) {
	const agentSessionID = "headless-question"
	cr := &mutableRunner{mockAgentRunner: newMockAgentRunner()}
	cr.running.Store(true)
	rec := &recordingChatStatus{}
	store := questionsignal.NewStore(time.Minute)
	store.SetPending(agentSessionID, "test")
	lc := newTestLifecycle(newMockSessionStore(), newMockRepoStore(), &mockAgentChatStore{}, nil, &mockWorktreeManager{}, cr, nil, newMockVCSProvider(), zerolog.Nop())
	lc.SetChatStatus(rec)
	lc.SetQuestionSignals(store)
	lc.headlessStatusPollInterval = time.Millisecond

	done := make(chan struct{})
	go func() {
		lc.watchHeadlessRunStatus("opencode", agentSessionID)
		close(done)
	}()

	rec.waitForStatus(t, pb.ChatStatus_CHAT_STATUS_QUESTION, 2)
	store.Clear(agentSessionID)
	rec.waitForStatus(t, pb.ChatStatus_CHAT_STATUS_WORKING, 1)
	store.SetPending(agentSessionID, "test-before-exit")
	cr.running.Store(false)
	<-done

	updates := rec.snapshot()
	questions := 0
	for _, update := range updates {
		if update.status == pb.ChatStatus_CHAT_STATUS_QUESTION {
			questions++
		}
	}
	if questions < 2 {
		t.Errorf("QUESTION updates = %d, want >=2 (initial + refresh)", questions)
	}
	if updates[0].status != pb.ChatStatus_CHAT_STATUS_QUESTION {
		t.Errorf("initial status = %v, want QUESTION", updates[0].status)
	}
	if last := updates[len(updates)-1]; last.status != pb.ChatStatus_CHAT_STATUS_STOPPED {
		t.Errorf("last status = %v, want STOPPED", last.status)
	}
	if store.HasPending(agentSessionID) {
		t.Error("pending signal remained after headless run stopped")
	}
}

func TestWatchHeadlessRunStatus_ExpiredSignalReturnsWorking(t *testing.T) {
	const agentSessionID = "headless-expired-question"
	now := time.Now()
	store := questionsignal.NewStoreWithClock(time.Minute, func() time.Time { return now })
	store.SetPending(agentSessionID, "test")
	now = now.Add(time.Minute + time.Nanosecond)
	cr := &flipRunner{mockAgentRunner: newMockAgentRunner(), trueFor: 1}
	rec := &recordingChatStatus{}
	lc := newTestLifecycle(newMockSessionStore(), newMockRepoStore(), &mockAgentChatStore{}, nil, &mockWorktreeManager{}, cr, nil, newMockVCSProvider(), zerolog.Nop())
	lc.SetChatStatus(rec)
	lc.SetQuestionSignals(store)
	lc.headlessStatusPollInterval = time.Millisecond

	lc.watchHeadlessRunStatus("opencode", agentSessionID)

	if got := rec.snapshot()[0].status; got != pb.ChatStatus_CHAT_STATUS_WORKING {
		t.Errorf("initial status = %v, want WORKING after signal expiry", got)
	}
}

// --- BOS-486: headless question-hook env threading ---

// fakeQuestionHookRegistrar records the (agent_session_id, token) bindings the
// lifecycle installs for headless runs, and the ids it later releases.
type fakeQuestionHookRegistrar struct {
	mu       sync.Mutex
	tokens   map[string]string
	released []string
}

func newFakeQuestionHookRegistrar() *fakeQuestionHookRegistrar {
	return &fakeQuestionHookRegistrar{tokens: make(map[string]string)}
}

func (f *fakeQuestionHookRegistrar) RegisterHeadlessRunHookToken(agentSessionID, token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens[agentSessionID] = token
}

// Deletes as well as recording, mirroring production
// HostServiceServer.ReleaseHeadlessRunHookToken. A fake that only appended to
// `released` would make every "is the token still registered?" assertion pass
// whether or not a release had happened — it silently hid the StartSession
// release path from every test in this file.
func (f *fakeQuestionHookRegistrar) ReleaseHeadlessRunHookToken(agentSessionID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, agentSessionID)
	delete(f.tokens, agentSessionID)
}

func (f *fakeQuestionHookRegistrar) token(agentSessionID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokens[agentSessionID]
}

// tokenCount reports how many tokens are registered. Guarded like the other
// accessors: registration happens inline in StartSession today, but reading the
// map directly would turn a future async registration into a -race report
// against the test rather than a finding about the code.
func (f *fakeQuestionHookRegistrar) tokenCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.tokens)
}

func (f *fakeQuestionHookRegistrar) releasedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.released...)
}

// seedHeadlessSession builds the minimal repo+session pair the detached
// StartSession path needs, returning the mocks the assertions read.
func seedHeadlessSession(t *testing.T) (*mockSessionStore, *mockRepoStore, *mockWorktreeManager, *mockAgentRunner) {
	t.Helper()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Test Session",
		Plan:       "Do something",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
		AgentName:  "opencode",
	}
	return sessions, repos, &mockWorktreeManager{}, newMockAgentRunner()
}

// TestStartSession_HeadlessRunCarriesQuestionHookEnv is the BOS-486 threading
// contract: a detached (headless) run is handed BOSS_HOOK_PORT + BOSS_HOOK_TOKEN
// in its ExtraEnv, and the SAME token is registered against the agent session id
// the plugin resolved — which is the id the injected hook POSTs under, so
// /hooks/question/{id} can authenticate. Both halves must agree or the signal
// silently 401s.
func TestStartSession_HeadlessRunCarriesQuestionHookEnv(t *testing.T) {
	ctx := context.Background()
	sessions, repos, wt, cr := seedHeadlessSession(t)
	cr.nextID = "ses_opencode_1"
	reg := newFakeQuestionHookRegistrar()

	lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), zerolog.Nop())
	lc.SetHookPort(45678)
	lc.SetQuestionHookRegistrar(reg)
	// The poll fallback is what eventually releases the token, so StartSession
	// only LEAVES it registered when the arm succeeds. Without these two the
	// defer would release before the assertions below and they would pass only
	// because the fake could not see it.
	armQuestionHookHandoff(lc, "opencode")

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{Detach: true}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	awaitDraftPR(t, lc, "sess-1")

	if len(cr.started) != 1 {
		t.Fatalf("expected 1 headless start, got %d", len(cr.started))
	}
	env := cr.started[0].env
	if got := env["BOSS_HOOK_PORT"]; got != "45678" {
		t.Errorf("BOSS_HOOK_PORT = %q, want 45678", got)
	}
	token := env["BOSS_HOOK_TOKEN"]
	if len(token) != 64 {
		t.Fatalf("BOSS_HOOK_TOKEN = %q (len %d), want 64 hex chars", token, len(token))
	}
	if _, err := hex.DecodeString(token); err != nil {
		t.Errorf("BOSS_HOOK_TOKEN is not hex: %v", err)
	}
	if got := reg.token("ses_opencode_1"); got != token {
		t.Errorf("registered token for ses_opencode_1 = %q, want the env token %q", got, token)
	}
}

// TestStartSession_HeadlessQuestionHookEnvRequiresFullWiring pins the
// fail-safe: with either half of the wiring missing the headless run is started
// with NO BOSS_HOOK_* keys at all and nothing is registered. A run without the
// signal must still run — losing the question hook is a degradation, not an
// error — and a token the receiver could never validate must never be handed
// out.
func TestStartSession_HeadlessQuestionHookEnvRequiresFullWiring(t *testing.T) {
	cases := []struct {
		name        string
		hookPort    int
		wireRegistr bool
	}{
		{"no registrar wired", 45678, false},
		{"hook server never bound", 0, true},
		{"neither", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			sessions, repos, wt, cr := seedHeadlessSession(t)
			reg := newFakeQuestionHookRegistrar()

			lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), zerolog.Nop())
			lc.SetHookPort(tc.hookPort)
			if tc.wireRegistr {
				lc.SetQuestionHookRegistrar(reg)
			}

			if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{Detach: true}); err != nil {
				t.Fatalf("StartSession: %v", err)
			}
			awaitDraftPR(t, lc, "sess-1")

			if len(cr.started) != 1 {
				t.Fatalf("expected 1 headless start, got %d", len(cr.started))
			}
			for _, key := range []string{"BOSS_HOOK_PORT", "BOSS_HOOK_TOKEN"} {
				if v, ok := cr.started[0].env[key]; ok {
					t.Errorf("%s present (%q) despite incomplete question-hook wiring", key, v)
				}
			}
			if got := reg.tokenCount(); got != 0 {
				t.Errorf("registrar recorded %d tokens, want 0", got)
			}
		})
	}
}

// TestStartSession_QuestionHookEnvSkipsNonConsumingAgents is the blast-radius
// gate: BOSS_HOOK_TOKEN is a live bearer credential for a daemon endpoint, so
// agents that provably do not consume it are never handed it. This is also what
// makes "a headless claude or codex run is byte-identical to pre-BOS-486"
// structural rather than incidental — with the wiring fully armed, their
// ExtraEnv must still contain neither key and nothing may be registered.
//
// The empty-AgentName case is the load-bearing one: agent.Dispatcher resolves
// "" to the default agent OR to the sole loaded runner, so an opencode-only
// install legitimately starts runs with no stored name. It must stay armed, or
// the feature silently disappears on exactly the install that wants it.
func TestStartSession_QuestionHookEnvSkipsNonConsumingAgents(t *testing.T) {
	cases := []struct {
		agentName string
		wantArmed bool
	}{
		{"claude", false},
		{"codex", false},
		{"opencode", true},
		// Unknown/unset agents stay armed: the plugin-side gate
		// (installQuestionHook) is the functional backstop, and an opencode-only
		// install runs with AgentName == "".
		{"", true},
		{"some-future-agent", true},
	}
	for _, tc := range cases {
		name := tc.agentName
		if name == "" {
			name = "unset"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			sessions, repos, wt, cr := seedHeadlessSession(t)
			sessions.sessions["sess-1"].AgentName = tc.agentName
			cr.nextID = "ses_run_1"
			reg := newFakeQuestionHookRegistrar()

			lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), zerolog.Nop())
			lc.SetHookPort(45678)
			lc.SetQuestionHookRegistrar(reg)
			armQuestionHookHandoff(lc, tc.agentName)

			if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{Detach: true}); err != nil {
				t.Fatalf("StartSession: %v", err)
			}
			awaitDraftPR(t, lc, "sess-1")

			if len(cr.started) != 1 {
				t.Fatalf("expected 1 headless start, got %d", len(cr.started))
			}
			env := cr.started[0].env
			_, portOK := env["BOSS_HOOK_PORT"]
			_, tokenOK := env["BOSS_HOOK_TOKEN"]
			if portOK != tc.wantArmed || tokenOK != tc.wantArmed {
				t.Errorf("agent %q: BOSS_HOOK_PORT present=%v BOSS_HOOK_TOKEN present=%v, want both %v",
					tc.agentName, portOK, tokenOK, tc.wantArmed)
			}
			wantTokens := 0
			if tc.wantArmed {
				wantTokens = 1
			}
			if got := reg.tokenCount(); got != wantTokens {
				t.Errorf("agent %q: registrar recorded %d tokens, want %d", tc.agentName, got, wantTokens)
			}
		})
	}
}

// armQuestionHookHandoff wires the poll fallback a headless StartSession needs
// in order to HAND OFF its question-hook token instead of releasing it at exit.
// Both halves are required: armHeadlessPollFallback bails without a pollArmer
// AND without a resolvable agent client.
func armQuestionHookHandoff(lc *Lifecycle, agentName string) {
	lc.SetPollArmer(&fakePollArmer{})
	lc.SetAgents(map[string]agent.AgentRunnerClient{agentName: newFakeAgent()})
}

// TestStartSession_ReleasesQuestionHookTokenWhenNoHandoff is the regression
// test for the leak the release-on-exit defer exists to close.
//
// The token is registered right after the spawn, but the only production
// releaser is SignalSessionRunComplete — reached through the poll fallback. If
// that fallback is never armed (no pollArmer, no agent client, a cron run, or
// any error return in between), nothing will EVER release the token: the entry
// would sit in the registry for the daemon's lifetime while the spawned child
// still holds the matching credential in its environment. StartSession must
// therefore hand the token back before it returns.
func TestStartSession_ReleasesQuestionHookTokenWhenNoHandoff(t *testing.T) {
	ctx := context.Background()
	sessions, repos, wt, cr := seedHeadlessSession(t)
	cr.nextID = "ses_opencode_1"
	reg := newFakeQuestionHookRegistrar()

	lc := newTestLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), zerolog.Nop())
	lc.SetHookPort(45678)
	lc.SetQuestionHookRegistrar(reg)
	// Deliberately NO armQuestionHookHandoff: the poll fallback declines, so
	// completion will never be signalled for this run.

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{Detach: true}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	awaitDraftPR(t, lc, "sess-1")

	// The run still got its env — the degradation is the signal, not the run.
	if len(cr.started) != 1 {
		t.Fatalf("expected 1 headless start, got %d", len(cr.started))
	}
	if cr.started[0].env["BOSS_HOOK_TOKEN"] == "" {
		t.Error("headless run started without BOSS_HOOK_TOKEN")
	}

	if !slices.Contains(reg.releasedIDs(), "ses_opencode_1") {
		t.Errorf("token for ses_opencode_1 was never released: %v", reg.releasedIDs())
	}
	if got := reg.tokenCount(); got != 0 {
		t.Errorf("registrar still holds %d token(s) for a run nothing will ever complete", got)
	}
}

// TestSignalSessionRunComplete_ReleasesQuestionHookToken a finished run's
// question-hook token is dropped, so the registry can't grow for the daemon's
// lifetime and a stale POST can no longer authenticate.
func TestSignalSessionRunComplete_ReleasesQuestionHookToken(t *testing.T) {
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", LocalPath: "/tmp/repo-main"}
	runID := "ses_opencode_1"
	sessions.sessions["sess-1"] = &models.Session{
		ID:             "sess-1",
		RepoID:         "repo-1",
		WorktreePath:   "/tmp/wt-sess1",
		State:          machine.ImplementingPlan,
		AgentName:      "opencode",
		AgentSessionID: &runID,
	}
	reg := newFakeQuestionHookRegistrar()
	lc := newTestLifecycle(sessions, repos, nil, &stubCronJobStore{}, &mockWorktreeManager{statusOut: ""}, newMockAgentRunner(), nil, newMockVCSProvider(), zerolog.Nop())
	lc.SetQuestionHookRegistrar(reg)

	lc.SignalSessionRunComplete("sess-1", runID, "")

	if got := reg.releasedIDs(); len(got) != 1 || got[0] != runID {
		t.Errorf("released = %v, want [%s]", got, runID)
	}
}
