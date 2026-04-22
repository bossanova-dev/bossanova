// Package tuitest provides end-to-end test infrastructure for the Boss TUI.
// It includes a mock daemon, test harness, and integration helpers that
// allow agents to programmatically drive and verify TUI behavior.
package tuitest

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var socketCounter atomic.Int64

// MockDaemon is a minimal ConnectRPC server that implements the DaemonService
// interface with in-memory data. Only the RPCs actually used by the TUI are
// implemented; the rest return Unimplemented.
type MockDaemon struct {
	mu            sync.RWMutex
	repos         []*pb.Repo
	sessions      []*pb.Session
	workflows     []*pb.AutopilotWorkflow
	chats         []*pb.ClaudeChat
	prs           map[string][]*pb.PRSummary    // keyed by repo ID
	trackerIssues map[string][]*pb.TrackerIssue // keyed by repo ID

	// lastCreateSession records the most recent CreateSession request so tests
	// can assert on what the TUI sent (e.g. that filter-narrowed selection uses
	// the correct original-index PR).
	lastCreateSession *pb.CreateSessionRequest

	// updateSessionCalls records every UpdateSession request so tests can
	// assert the TUI sent the expected title / field updates.
	updateSessionCalls []*pb.UpdateSessionRequest

	// Channel-backed AttachSession streaming. Tests push events via
	// PushOutputLine / PushStateChange / PushSessionEnded; the AttachSession
	// RPC reads from the per-session channel and forwards to the stream.
	attachEvents map[string]chan *pb.AttachSessionResponse
	attachCalls  []*pb.AttachSessionRequest

	// ensureTmuxErr, when non-nil, is returned from every EnsureTmuxSession
	// call. Lets tests exercise AttachView's error path without depending on
	// tmux behavior inside the PTY harness.
	ensureTmuxErr error

	// ensureTmuxCalls records every EnsureTmuxSession request so tests can
	// assert on which session / mode / claude_id the TUI sent.
	ensureTmuxCalls []*pb.EnsureTmuxSessionRequest

	// validateRepoPathResp, when non-nil, overrides the default ValidateRepoPath
	// response (IsValid=true). Lets tests exercise RepoAddView's error-path.
	validateRepoPathResp *pb.ValidateRepoPathResponse
	validateRepoPathErr  error

	// registerRepoCalls records every RegisterRepo request so tests can assert
	// the TUI sent the expected display name / path / setup script.
	registerRepoCalls []*pb.RegisterRepoRequest

	// notifyAuthChangeCalls records the action ("login" / "logout") of every
	// NotifyAuthChange request so tests can assert the TUI notified the
	// daemon after the user authenticated or signed out.
	notifyAuthChangeCalls []string

	socketPath string
	httpServer *http.Server
	listener   net.Listener
}

// NewMockDaemon starts a mock daemon on a temporary Unix socket.
// The server is cleaned up when the test finishes.
func NewMockDaemon(t *testing.T) *MockDaemon {
	t.Helper()

	// Use /tmp directly — t.TempDir() paths can exceed the 104-char macOS Unix socket limit.
	// Include PID so parallel test binaries (tuitest + clitest run side-by-side under
	// `go test ./...`) don't collide on `/tmp/boss-tuitest-1.sock`: each package gets
	// its own counter starting at 1, so without the PID qualifier the second binary
	// would remove and rebind the first binary's still-active socket.
	socketPath := filepath.Join("/tmp", fmt.Sprintf("boss-tuitest-%d-%d.sock", os.Getpid(), socketCounter.Add(1)))
	t.Cleanup(func() {
		_ = removeSocket(socketPath)
	})
	_ = removeSocket(socketPath)

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}

	m := &MockDaemon{
		socketPath:    socketPath,
		listener:      ln,
		prs:           make(map[string][]*pb.PRSummary),
		trackerIssues: make(map[string][]*pb.TrackerIssue),
		attachEvents:  make(map[string]chan *pb.AttachSessionResponse),
	}

	mux := http.NewServeMux()
	path, handler := bossanovav1connect.NewDaemonServiceHandler(m)
	mux.Handle(path, handler)

	m.httpServer = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = m.httpServer.Serve(ln) }()

	t.Cleanup(func() {
		_ = m.httpServer.Close()
	})

	return m
}

// SocketPath returns the Unix socket path for the mock daemon.
func (m *MockDaemon) SocketPath() string {
	return m.socketPath
}

// --- Data store accessors ---

// AddRepo adds a repo to the mock daemon's in-memory store.
func (m *MockDaemon) AddRepo(repo *pb.Repo) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.repos = append(m.repos, repo)
}

// AddSession adds a session to the mock daemon's in-memory store.
func (m *MockDaemon) AddSession(sess *pb.Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions = append(m.sessions, sess)
}

// AddWorkflow adds an autopilot workflow to the mock daemon's in-memory store.
func (m *MockDaemon) AddWorkflow(w *pb.AutopilotWorkflow) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.workflows = append(m.workflows, w)
}

// AddChat adds a claude chat to the mock daemon's in-memory store.
func (m *MockDaemon) AddChat(c *pb.ClaudeChat) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.chats = append(m.chats, c)
}

// AddPRs adds pull request summaries for a repo to the mock daemon's in-memory store.
func (m *MockDaemon) AddPRs(repoID string, prs []*pb.PRSummary) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prs[repoID] = append(m.prs[repoID], prs...)
}

// AddTrackerIssues adds tracker (Linear) issues for a repo to the mock daemon's in-memory store.
func (m *MockDaemon) AddTrackerIssues(repoID string, issues []*pb.TrackerIssue) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.trackerIssues[repoID] = append(m.trackerIssues[repoID], issues...)
}

// LastCreateSession returns the most recent CreateSession request received
// by the mock, or nil if none was received.
func (m *MockDaemon) LastCreateSession() *pb.CreateSessionRequest {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.lastCreateSession
}

// UpdateSessionCalls returns a copy of every UpdateSession request recorded
// by the mock.
func (m *MockDaemon) UpdateSessionCalls() []*pb.UpdateSessionRequest {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*pb.UpdateSessionRequest, len(m.updateSessionCalls))
	copy(out, m.updateSessionCalls)
	return out
}

// Sessions returns a copy of the current sessions.
func (m *MockDaemon) Sessions() []*pb.Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*pb.Session, len(m.sessions))
	copy(out, m.sessions)
	return out
}

// Repos returns a copy of the current repos.
func (m *MockDaemon) Repos() []*pb.Repo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*pb.Repo, len(m.repos))
	copy(out, m.repos)
	return out
}

// Workflows returns a copy of the current workflows.
func (m *MockDaemon) Workflows() []*pb.AutopilotWorkflow {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*pb.AutopilotWorkflow, len(m.workflows))
	copy(out, m.workflows)
	return out
}

// --- DaemonServiceHandler implementation ---

func (m *MockDaemon) ListRepos(_ context.Context, _ *connect.Request[pb.ListReposRequest]) (*connect.Response[pb.ListReposResponse], error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return connect.NewResponse(&pb.ListReposResponse{Repos: m.repos}), nil
}

func (m *MockDaemon) ListSessions(_ context.Context, req *connect.Request[pb.ListSessionsRequest]) (*connect.Response[pb.ListSessionsResponse], error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	stateSet := make(map[pb.SessionState]bool, len(req.Msg.States))
	for _, st := range req.Msg.States {
		stateSet[st] = true
	}
	var out []*pb.Session
	for _, s := range m.sessions {
		if s.ArchivedAt != nil && !req.Msg.IncludeArchived {
			continue
		}
		if req.Msg.RepoId != nil && *req.Msg.RepoId != "" && s.RepoId != *req.Msg.RepoId {
			continue
		}
		if len(stateSet) > 0 && !stateSet[s.State] {
			continue
		}
		out = append(out, s)
	}
	return connect.NewResponse(&pb.ListSessionsResponse{Sessions: out}), nil
}

func (m *MockDaemon) GetSession(_ context.Context, req *connect.Request[pb.GetSessionRequest]) (*connect.Response[pb.GetSessionResponse], error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, s := range m.sessions {
		if s.Id == req.Msg.Id {
			return connect.NewResponse(&pb.GetSessionResponse{Session: s}), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session %q not found", req.Msg.Id))
}

func (m *MockDaemon) ArchiveSession(_ context.Context, req *connect.Request[pb.ArchiveSessionRequest]) (*connect.Response[pb.ArchiveSessionResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		if s.Id == req.Msg.Id {
			s.ArchivedAt = timestamppb.Now()
			return connect.NewResponse(&pb.ArchiveSessionResponse{Session: s}), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session %q not found", req.Msg.Id))
}

func (m *MockDaemon) ResurrectSession(_ context.Context, req *connect.Request[pb.ResurrectSessionRequest]) (*connect.Response[pb.ResurrectSessionResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		if s.Id == req.Msg.Id {
			s.ArchivedAt = nil
			return connect.NewResponse(&pb.ResurrectSessionResponse{Session: s}), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session %q not found", req.Msg.Id))
}

func (m *MockDaemon) RemoveSession(_ context.Context, req *connect.Request[pb.RemoveSessionRequest]) (*connect.Response[pb.RemoveSessionResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, s := range m.sessions {
		if s.Id == req.Msg.Id {
			m.sessions = append(m.sessions[:i], m.sessions[i+1:]...)
			return connect.NewResponse(&pb.RemoveSessionResponse{}), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session %q not found", req.Msg.Id))
}

func (m *MockDaemon) EmptyTrash(_ context.Context, _ *connect.Request[pb.EmptyTrashRequest]) (*connect.Response[pb.EmptyTrashResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var kept []*pb.Session
	var count int32
	for _, s := range m.sessions {
		if s.ArchivedAt != nil {
			count++
		} else {
			kept = append(kept, s)
		}
	}
	m.sessions = kept
	return connect.NewResponse(&pb.EmptyTrashResponse{DeletedCount: count}), nil
}

func (m *MockDaemon) ListChats(_ context.Context, req *connect.Request[pb.ListChatsRequest]) (*connect.Response[pb.ListChatsResponse], error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*pb.ClaudeChat
	for _, c := range m.chats {
		if req.Msg.SessionId == "" || c.SessionId == req.Msg.SessionId {
			out = append(out, c)
		}
	}
	return connect.NewResponse(&pb.ListChatsResponse{Chats: out}), nil
}

func (m *MockDaemon) ReportChatStatus(_ context.Context, _ *connect.Request[pb.ReportChatStatusRequest]) (*connect.Response[pb.ReportChatStatusResponse], error) {
	return connect.NewResponse(&pb.ReportChatStatusResponse{}), nil
}

func (m *MockDaemon) GetChatStatuses(_ context.Context, _ *connect.Request[pb.GetChatStatusesRequest]) (*connect.Response[pb.GetChatStatusesResponse], error) {
	return connect.NewResponse(&pb.GetChatStatusesResponse{}), nil
}

func (m *MockDaemon) GetSessionStatuses(_ context.Context, _ *connect.Request[pb.GetSessionStatusesRequest]) (*connect.Response[pb.GetSessionStatusesResponse], error) {
	return connect.NewResponse(&pb.GetSessionStatusesResponse{}), nil
}

func (m *MockDaemon) ListRepoPRs(_ context.Context, req *connect.Request[pb.ListRepoPRsRequest]) (*connect.Response[pb.ListRepoPRsResponse], error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	prs := m.prs[req.Msg.RepoId]
	return connect.NewResponse(&pb.ListRepoPRsResponse{PullRequests: prs}), nil
}

func (m *MockDaemon) ListTrackerIssues(_ context.Context, req *connect.Request[pb.ListTrackerIssuesRequest]) (*connect.Response[pb.ListTrackerIssuesResponse], error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	issues := m.trackerIssues[req.Msg.RepoId]
	// When the TUI sends a query, simulate the Linear-side filter by narrowing
	// to issues whose title contains the query (case-insensitive). This lets
	// tests exercise the debounced-search code path without spinning up a real
	// Linear API.
	if q := strings.TrimSpace(req.Msg.Query); q != "" {
		filtered := issues[:0:0]
		needle := strings.ToLower(q)
		for _, i := range issues {
			if strings.Contains(strings.ToLower(i.Title), needle) {
				filtered = append(filtered, i)
			}
		}
		issues = filtered
	}
	return connect.NewResponse(&pb.ListTrackerIssuesResponse{Issues: issues}), nil
}

// --- Repo management RPCs ---

func (m *MockDaemon) RemoveRepo(_ context.Context, req *connect.Request[pb.RemoveRepoRequest]) (*connect.Response[pb.RemoveRepoResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, r := range m.repos {
		if r.Id == req.Msg.Id {
			m.repos = append(m.repos[:i], m.repos[i+1:]...)
			return connect.NewResponse(&pb.RemoveRepoResponse{}), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("repo %q not found", req.Msg.Id))
}

func (m *MockDaemon) UpdateRepo(_ context.Context, req *connect.Request[pb.UpdateRepoRequest]) (*connect.Response[pb.UpdateRepoResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.repos {
		if r.Id == req.Msg.Id {
			if req.Msg.DisplayName != nil {
				r.DisplayName = *req.Msg.DisplayName
			}
			if req.Msg.CanAutoMerge != nil {
				r.CanAutoMerge = *req.Msg.CanAutoMerge
			}
			if req.Msg.CanAutoMergeDependabot != nil {
				r.CanAutoMergeDependabot = *req.Msg.CanAutoMergeDependabot
			}
			if req.Msg.CanAutoAddressReviews != nil {
				r.CanAutoAddressReviews = *req.Msg.CanAutoAddressReviews
			}
			if req.Msg.CanAutoResolveConflicts != nil {
				r.CanAutoResolveConflicts = *req.Msg.CanAutoResolveConflicts
			}
			if req.Msg.MergeStrategy != nil {
				r.MergeStrategy = *req.Msg.MergeStrategy
			}
			if req.Msg.SetupScript != nil {
				r.SetupScript = req.Msg.SetupScript
			}
			r.UpdatedAt = timestamppb.Now()
			return connect.NewResponse(&pb.UpdateRepoResponse{Repo: r}), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("repo %q not found", req.Msg.Id))
}

func (m *MockDaemon) ValidateRepoPath(_ context.Context, _ *connect.Request[pb.ValidateRepoPathRequest]) (*connect.Response[pb.ValidateRepoPathResponse], error) {
	m.mu.RLock()
	resp := m.validateRepoPathResp
	err := m.validateRepoPathErr
	m.mu.RUnlock()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if resp != nil {
		return connect.NewResponse(resp), nil
	}
	return connect.NewResponse(&pb.ValidateRepoPathResponse{
		IsValid:       true,
		IsGithub:      true,
		DefaultBranch: "main",
	}), nil
}

// SetValidateRepoPathResult overrides the default ValidateRepoPath response.
// Passing nil clears the override.
func (m *MockDaemon) SetValidateRepoPathResult(resp *pb.ValidateRepoPathResponse) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.validateRepoPathResp = resp
}

// SetValidateRepoPathError makes every ValidateRepoPath call return err.
func (m *MockDaemon) SetValidateRepoPathError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.validateRepoPathErr = err
}

func (m *MockDaemon) RegisterRepo(_ context.Context, req *connect.Request[pb.RegisterRepoRequest]) (*connect.Response[pb.RegisterRepoResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.registerRepoCalls = append(m.registerRepoCalls, req.Msg)
	repo := &pb.Repo{
		Id:                fmt.Sprintf("repo-%d", len(m.repos)+1),
		DisplayName:       req.Msg.DisplayName,
		LocalPath:         req.Msg.LocalPath,
		DefaultBaseBranch: req.Msg.DefaultBaseBranch,
		WorktreeBaseDir:   req.Msg.WorktreeBaseDir,
		SetupScript:       req.Msg.SetupScript,
		CreatedAt:         timestamppb.Now(),
		UpdatedAt:         timestamppb.Now(),
	}
	m.repos = append(m.repos, repo)
	return connect.NewResponse(&pb.RegisterRepoResponse{Repo: repo}), nil
}

// RegisterRepoCalls returns a copy of every RegisterRepo request recorded
// by the mock.
func (m *MockDaemon) RegisterRepoCalls() []*pb.RegisterRepoRequest {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*pb.RegisterRepoRequest, len(m.registerRepoCalls))
	copy(out, m.registerRepoCalls)
	return out
}

// --- Autopilot RPCs ---

func (m *MockDaemon) ListAutopilotWorkflows(_ context.Context, _ *connect.Request[pb.ListAutopilotWorkflowsRequest]) (*connect.Response[pb.ListAutopilotWorkflowsResponse], error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return connect.NewResponse(&pb.ListAutopilotWorkflowsResponse{Workflows: m.workflows}), nil
}

func (m *MockDaemon) PauseAutopilot(_ context.Context, req *connect.Request[pb.PauseAutopilotRequest]) (*connect.Response[pb.PauseAutopilotResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, w := range m.workflows {
		if w.Id == req.Msg.WorkflowId {
			w.Status = pb.WorkflowStatus_WORKFLOW_STATUS_PAUSED
			w.UpdatedAt = timestamppb.Now()
			return connect.NewResponse(&pb.PauseAutopilotResponse{Workflow: w}), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("workflow %q not found", req.Msg.WorkflowId))
}

func (m *MockDaemon) ResumeAutopilot(_ context.Context, req *connect.Request[pb.ResumeAutopilotRequest]) (*connect.Response[pb.ResumeAutopilotResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, w := range m.workflows {
		if w.Id == req.Msg.WorkflowId {
			w.Status = pb.WorkflowStatus_WORKFLOW_STATUS_RUNNING
			w.UpdatedAt = timestamppb.Now()
			return connect.NewResponse(&pb.ResumeAutopilotResponse{Workflow: w}), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("workflow %q not found", req.Msg.WorkflowId))
}

func (m *MockDaemon) CancelAutopilot(_ context.Context, req *connect.Request[pb.CancelAutopilotRequest]) (*connect.Response[pb.CancelAutopilotResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, w := range m.workflows {
		if w.Id == req.Msg.WorkflowId {
			w.Status = pb.WorkflowStatus_WORKFLOW_STATUS_CANCELLED
			w.UpdatedAt = timestamppb.Now()
			return connect.NewResponse(&pb.CancelAutopilotResponse{Workflow: w}), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("workflow %q not found", req.Msg.WorkflowId))
}

// --- Chat RPCs ---

func (m *MockDaemon) DeleteChat(_ context.Context, req *connect.Request[pb.DeleteChatRequest]) (*connect.Response[pb.DeleteChatResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, c := range m.chats {
		if c.ClaudeId == req.Msg.ClaudeId {
			m.chats = append(m.chats[:i], m.chats[i+1:]...)
			return connect.NewResponse(&pb.DeleteChatResponse{}), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("chat %q not found", req.Msg.ClaudeId))
}

func (m *MockDaemon) UpdateChatTitle(_ context.Context, _ *connect.Request[pb.UpdateChatTitleRequest]) (*connect.Response[pb.UpdateChatTitleResponse], error) {
	return connect.NewResponse(&pb.UpdateChatTitleResponse{}), nil
}

// --- Unimplemented RPCs (streaming or not used by tested views) ---

func (m *MockDaemon) ResolveContext(context.Context, *connect.Request[pb.ResolveContextRequest]) (*connect.Response[pb.ResolveContextResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

func (m *MockDaemon) CloneAndRegisterRepo(context.Context, *connect.Request[pb.CloneAndRegisterRepoRequest]) (*connect.Response[pb.CloneAndRegisterRepoResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

func (m *MockDaemon) CreateSession(_ context.Context, req *connect.Request[pb.CreateSessionRequest], _ *connect.ServerStream[pb.CreateSessionResponse]) error {
	m.mu.Lock()
	m.lastCreateSession = req.Msg
	m.mu.Unlock()
	// Return Unimplemented so the TUI surfaces an error banner after recording
	// the request — tests assert on the captured request, not on created sessions.
	return connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

// AttachSession reads events from the per-session channel populated by
// PushOutputLine / PushStateChange / PushSessionEnded and forwards them to
// the stream. Returns nil on SessionEnded or ctx cancellation.
func (m *MockDaemon) AttachSession(ctx context.Context, req *connect.Request[pb.AttachSessionRequest], stream *connect.ServerStream[pb.AttachSessionResponse]) error {
	m.mu.Lock()
	m.attachCalls = append(m.attachCalls, req.Msg)
	m.mu.Unlock()

	ch := m.ensureAttachChannel(req.Msg.Id)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return nil
			}
			if err := stream.Send(ev); err != nil {
				return err
			}
			if _, ended := ev.Event.(*pb.AttachSessionResponse_SessionEnded); ended {
				return nil
			}
		case <-ctx.Done():
			return nil
		}
	}
}

// AttachSessionCalls returns a copy of every AttachSession request recorded
// by the mock.
func (m *MockDaemon) AttachSessionCalls() []*pb.AttachSessionRequest {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*pb.AttachSessionRequest, len(m.attachCalls))
	copy(out, m.attachCalls)
	return out
}

// PushOutputLine enqueues an OutputLine event on the session's attach stream.
// Blocks if the channel is full (64-event buffer should be enough for tests).
func (m *MockDaemon) PushOutputLine(sessionID, text string) {
	m.ensureAttachChannel(sessionID) <- &pb.AttachSessionResponse{
		Event: &pb.AttachSessionResponse_OutputLine{
			OutputLine: &pb.OutputLine{
				Text:      text,
				Timestamp: timestamppb.Now(),
			},
		},
	}
}

// PushStateChange enqueues a StateChange event on the session's attach stream.
func (m *MockDaemon) PushStateChange(sessionID string, previous, next pb.SessionState) {
	m.ensureAttachChannel(sessionID) <- &pb.AttachSessionResponse{
		Event: &pb.AttachSessionResponse_StateChange{
			StateChange: &pb.StateChange{
				PreviousState: previous,
				NewState:      next,
			},
		},
	}
}

// PushSessionEnded enqueues a SessionEnded event. The active AttachSession
// stream returns nil after sending this event, closing the stream cleanly.
func (m *MockDaemon) PushSessionEnded(sessionID string, finalState pb.SessionState) {
	m.ensureAttachChannel(sessionID) <- &pb.AttachSessionResponse{
		Event: &pb.AttachSessionResponse_SessionEnded{
			SessionEnded: &pb.SessionEnded{
				FinalState: finalState,
			},
		},
	}
}

// ensureAttachChannel returns the buffered event channel for a session,
// creating it if needed. Safe for concurrent callers.
func (m *MockDaemon) ensureAttachChannel(sessionID string) chan *pb.AttachSessionResponse {
	m.mu.Lock()
	defer m.mu.Unlock()
	ch, ok := m.attachEvents[sessionID]
	if !ok {
		ch = make(chan *pb.AttachSessionResponse, 64)
		m.attachEvents[sessionID] = ch
	}
	return ch
}

func (m *MockDaemon) UpdateSession(_ context.Context, req *connect.Request[pb.UpdateSessionRequest]) (*connect.Response[pb.UpdateSessionResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updateSessionCalls = append(m.updateSessionCalls, req.Msg)
	for _, s := range m.sessions {
		if s.Id == req.Msg.Id {
			if req.Msg.Title != nil {
				s.Title = *req.Msg.Title
			}
			return connect.NewResponse(&pb.UpdateSessionResponse{Session: s}), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session %q not found", req.Msg.Id))
}

func (m *MockDaemon) StopSession(context.Context, *connect.Request[pb.StopSessionRequest]) (*connect.Response[pb.StopSessionResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

func (m *MockDaemon) PauseSession(context.Context, *connect.Request[pb.PauseSessionRequest]) (*connect.Response[pb.PauseSessionResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

func (m *MockDaemon) ResumeSession(context.Context, *connect.Request[pb.ResumeSessionRequest]) (*connect.Response[pb.ResumeSessionResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

func (m *MockDaemon) RetrySession(context.Context, *connect.Request[pb.RetrySessionRequest]) (*connect.Response[pb.RetrySessionResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

func (m *MockDaemon) CloseSession(context.Context, *connect.Request[pb.CloseSessionRequest]) (*connect.Response[pb.CloseSessionResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

func (m *MockDaemon) MergeSession(context.Context, *connect.Request[pb.MergeSessionRequest]) (*connect.Response[pb.MergeSessionResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

func (m *MockDaemon) RecordChat(context.Context, *connect.Request[pb.RecordChatRequest]) (*connect.Response[pb.RecordChatResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

func (m *MockDaemon) DeliverVCSEvent(context.Context, *connect.Request[pb.DeliverVCSEventRequest]) (*connect.Response[pb.DeliverVCSEventResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

func (m *MockDaemon) StartAutopilot(context.Context, *connect.Request[pb.StartAutopilotRequest]) (*connect.Response[pb.StartAutopilotResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

func (m *MockDaemon) GetAutopilotStatus(context.Context, *connect.Request[pb.GetAutopilotStatusRequest]) (*connect.Response[pb.GetAutopilotStatusResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

func (m *MockDaemon) StreamAutopilotOutput(context.Context, *connect.Request[pb.StreamAutopilotOutputRequest], *connect.ServerStream[pb.StreamAutopilotOutputResponse]) error {
	return connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

func (m *MockDaemon) EnsureTmuxSession(_ context.Context, req *connect.Request[pb.EnsureTmuxSessionRequest]) (*connect.Response[pb.EnsureTmuxSessionResponse], error) {
	m.mu.Lock()
	m.ensureTmuxCalls = append(m.ensureTmuxCalls, req.Msg)
	injectedErr := m.ensureTmuxErr
	m.mu.Unlock()
	if injectedErr != nil {
		return nil, connect.NewError(connect.CodeInternal, injectedErr)
	}
	claudeID := req.Msg.ClaudeId
	if claudeID == "" {
		claudeID = uuid.New().String()
	}
	return connect.NewResponse(&pb.EnsureTmuxSessionResponse{
		TmuxSessionName: "boss-mock-tmux",
		ClaudeId:        claudeID,
	}), nil
}

// EnsureTmuxSessionCalls returns a copy of every EnsureTmuxSession request
// recorded by the mock.
func (m *MockDaemon) EnsureTmuxSessionCalls() []*pb.EnsureTmuxSessionRequest {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*pb.EnsureTmuxSessionRequest, len(m.ensureTmuxCalls))
	copy(out, m.ensureTmuxCalls)
	return out
}

// SetEnsureTmuxError makes every EnsureTmuxSession call return err. Used by
// tests that exercise AttachView's error path without depending on tmux.
func (m *MockDaemon) SetEnsureTmuxError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureTmuxErr = err
}

func (m *MockDaemon) NotifyAuthChange(_ context.Context, req *connect.Request[pb.NotifyAuthChangeRequest]) (*connect.Response[pb.NotifyAuthChangeResponse], error) {
	m.mu.Lock()
	m.notifyAuthChangeCalls = append(m.notifyAuthChangeCalls, req.Msg.Action)
	m.mu.Unlock()
	return connect.NewResponse(&pb.NotifyAuthChangeResponse{}), nil
}

// NotifyAuthChangeCalls returns a copy of the actions ("login" / "logout")
// passed to every NotifyAuthChange request recorded by the mock.
func (m *MockDaemon) NotifyAuthChangeCalls() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, len(m.notifyAuthChangeCalls))
	copy(out, m.notifyAuthChangeCalls)
	return out
}

// removeSocket removes a socket file, ignoring "not exist" errors.
func removeSocket(path string) error {
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
