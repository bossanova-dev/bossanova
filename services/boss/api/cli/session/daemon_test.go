package session

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/adapter/daemontest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type sessionDaemonFake struct {
	bossanovav1connect.UnimplementedDaemonServiceHandler
	mu                         sync.RWMutex
	socket                     string
	sessions                   []*pb.Session
	archiveRequests            []*pb.ArchiveSessionRequest
	archiveDeferBlockingID     string
	archiveDeferStalledSeconds int32
}

func newSessionDaemonFake(t *testing.T) *sessionDaemonFake {
	t.Helper()
	f := &sessionDaemonFake{}
	f.socket = daemontest.Serve(t, f)
	return f
}
func (m *sessionDaemonFake) SocketPath() string { return m.socket }
func (m *sessionDaemonFake) AddSession(sess *pb.Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions = append(m.sessions, proto.CloneOf(sess))
}

func (m *sessionDaemonFake) ArchiveSession(_ context.Context, req *connect.Request[pb.ArchiveSessionRequest]) (*connect.Response[pb.ArchiveSessionResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.archiveRequests = append(m.archiveRequests, proto.CloneOf(req.Msg))
	for _, s := range m.sessions {
		if s.Id == req.Msg.Id {
			if m.archiveDeferBlockingID != "" && !req.Msg.GetShouldForce() {
				s.ArchivePending = true
				return connect.NewResponse(&pb.ArchiveSessionResponse{
					Session:                    proto.CloneOf(s),
					IsDeferred:                 true,
					BlockingAgentSessionId:     m.archiveDeferBlockingID,
					BlockingChatStalledSeconds: m.archiveDeferStalledSeconds,
				}), nil
			}
			s.ArchivedAt = timestamppb.Now()
			return connect.NewResponse(&pb.ArchiveSessionResponse{Session: proto.CloneOf(s)}), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session %q not found", req.Msg.Id))
}

func (m *sessionDaemonFake) SetArchiveDeferred(blockingAgentSessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.archiveDeferBlockingID = blockingAgentSessionID
}

func (m *sessionDaemonFake) SetArchiveDeferredStalled(blockingAgentSessionID string, stalledSeconds int32) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.archiveDeferBlockingID = blockingAgentSessionID
	m.archiveDeferStalledSeconds = stalledSeconds
}

func (m *sessionDaemonFake) ArchiveSessionRequests() []*pb.ArchiveSessionRequest {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*pb.ArchiveSessionRequest, 0, len(m.archiveRequests))
	for _, req := range m.archiveRequests {
		out = append(out, proto.CloneOf(req))
	}
	return out
}
func (m *sessionDaemonFake) ListSessions(context.Context, *connect.Request[pb.ListSessionsRequest]) (*connect.Response[pb.ListSessionsResponse], error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sessions := make([]*pb.Session, 0, len(m.sessions))
	for _, session := range m.sessions {
		sessions = append(sessions, proto.CloneOf(session))
	}
	return connect.NewResponse(&pb.ListSessionsResponse{Sessions: sessions}), nil
}
