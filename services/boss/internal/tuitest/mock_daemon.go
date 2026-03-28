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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var socketCounter atomic.Int64

// MockDaemon is a minimal ConnectRPC server that implements the DaemonService
// interface with in-memory data. Only the RPCs actually used by the TUI are
// implemented; the rest return Unimplemented.
type MockDaemon struct {
	mu       sync.RWMutex
	repos    []*pb.Repo
	sessions []*pb.Session

	socketPath string
	httpServer *http.Server
	listener   net.Listener
}

// NewMockDaemon starts a mock daemon on a temporary Unix socket.
// The server is cleaned up when the test finishes.
func NewMockDaemon(t *testing.T) *MockDaemon {
	t.Helper()

	// Use /tmp directly — t.TempDir() paths can exceed the 104-char macOS Unix socket limit.
	socketPath := filepath.Join("/tmp", fmt.Sprintf("boss-tuitest-%d.sock", socketCounter.Add(1)))
	t.Cleanup(func() {
		_ = removeSocket(socketPath)
	})
	_ = removeSocket(socketPath)

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}

	m := &MockDaemon{
		socketPath: socketPath,
		listener:   ln,
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

// Sessions returns a copy of the current sessions.
func (m *MockDaemon) Sessions() []*pb.Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*pb.Session, len(m.sessions))
	copy(out, m.sessions)
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
	var out []*pb.Session
	for _, s := range m.sessions {
		if s.ArchivedAt != nil && !req.Msg.IncludeArchived {
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

func (m *MockDaemon) ListChats(_ context.Context, _ *connect.Request[pb.ListChatsRequest]) (*connect.Response[pb.ListChatsResponse], error) {
	return connect.NewResponse(&pb.ListChatsResponse{}), nil
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

func (m *MockDaemon) ListRepoPRs(_ context.Context, _ *connect.Request[pb.ListRepoPRsRequest]) (*connect.Response[pb.ListRepoPRsResponse], error) {
	return connect.NewResponse(&pb.ListRepoPRsResponse{}), nil
}

// --- Unimplemented RPCs (not used by the TUI views we test) ---

func (m *MockDaemon) ResolveContext(context.Context, *connect.Request[pb.ResolveContextRequest]) (*connect.Response[pb.ResolveContextResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

func (m *MockDaemon) ValidateRepoPath(context.Context, *connect.Request[pb.ValidateRepoPathRequest]) (*connect.Response[pb.ValidateRepoPathResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

func (m *MockDaemon) RegisterRepo(context.Context, *connect.Request[pb.RegisterRepoRequest]) (*connect.Response[pb.RegisterRepoResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

func (m *MockDaemon) CloneAndRegisterRepo(context.Context, *connect.Request[pb.CloneAndRegisterRepoRequest]) (*connect.Response[pb.CloneAndRegisterRepoResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

func (m *MockDaemon) RemoveRepo(context.Context, *connect.Request[pb.RemoveRepoRequest]) (*connect.Response[pb.RemoveRepoResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

func (m *MockDaemon) UpdateRepo(context.Context, *connect.Request[pb.UpdateRepoRequest]) (*connect.Response[pb.UpdateRepoResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

func (m *MockDaemon) CreateSession(context.Context, *connect.Request[pb.CreateSessionRequest], *connect.ServerStream[pb.CreateSessionResponse]) error {
	return connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

func (m *MockDaemon) AttachSession(context.Context, *connect.Request[pb.AttachSessionRequest], *connect.ServerStream[pb.AttachSessionResponse]) error {
	return connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
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

func (m *MockDaemon) RecordChat(context.Context, *connect.Request[pb.RecordChatRequest]) (*connect.Response[pb.RecordChatResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

func (m *MockDaemon) UpdateChatTitle(context.Context, *connect.Request[pb.UpdateChatTitleRequest]) (*connect.Response[pb.UpdateChatTitleResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

func (m *MockDaemon) DeleteChat(context.Context, *connect.Request[pb.DeleteChatRequest]) (*connect.Response[pb.DeleteChatResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

func (m *MockDaemon) DeliverVCSEvent(context.Context, *connect.Request[pb.DeliverVCSEventRequest]) (*connect.Response[pb.DeliverVCSEventResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

func (m *MockDaemon) StartAutopilot(context.Context, *connect.Request[pb.StartAutopilotRequest]) (*connect.Response[pb.StartAutopilotResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

func (m *MockDaemon) PauseAutopilot(context.Context, *connect.Request[pb.PauseAutopilotRequest]) (*connect.Response[pb.PauseAutopilotResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

func (m *MockDaemon) ResumeAutopilot(context.Context, *connect.Request[pb.ResumeAutopilotRequest]) (*connect.Response[pb.ResumeAutopilotResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

func (m *MockDaemon) CancelAutopilot(context.Context, *connect.Request[pb.CancelAutopilotRequest]) (*connect.Response[pb.CancelAutopilotResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

func (m *MockDaemon) GetAutopilotStatus(context.Context, *connect.Request[pb.GetAutopilotStatusRequest]) (*connect.Response[pb.GetAutopilotStatusResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

func (m *MockDaemon) ListAutopilotWorkflows(context.Context, *connect.Request[pb.ListAutopilotWorkflowsRequest]) (*connect.Response[pb.ListAutopilotWorkflowsResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

func (m *MockDaemon) StreamAutopilotOutput(context.Context, *connect.Request[pb.StreamAutopilotOutputRequest], *connect.ServerStream[pb.StreamAutopilotOutputResponse]) error {
	return connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

// removeSocket removes a socket file, ignoring "not exist" errors.
func removeSocket(path string) error {
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
