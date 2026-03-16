package server

import (
	"context"
	"database/sql"
	"fmt"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bosso/internal/auth"
)

// resolveUserID extracts the owning user's ID from the auth context.
// Accepts both user auth (OIDC JWT) and daemon auth (session token).
func resolveUserID(ctx context.Context) (string, error) {
	info := auth.InfoFromContext(ctx)
	if info == nil {
		return "", connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("authentication required"))
	}
	if info.IsUser() {
		return info.UserID, nil
	}
	return info.DaemonUserID, nil
}

// getDaemonClient looks up the session's daemon, verifies ownership, and returns
// the DaemonServiceClient from the relay pool.
func (s *Server) getDaemonClient(ctx context.Context, sessionID string) (bossanovav1connect.DaemonServiceClient, error) {
	userID, err := resolveUserID(ctx)
	if err != nil {
		return nil, err
	}

	entry, err := s.sessions.Get(ctx, sessionID)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session not found"))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get session: %w", err))
	}

	daemon, err := s.daemons.Get(ctx, entry.DaemonID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get daemon: %w", err))
	}

	if daemon.UserID != userID {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("session belongs to another user"))
	}

	if !daemon.Online {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("daemon is offline"))
	}

	client := s.pool.Get(daemon.ID)
	if client == nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("daemon has no proxy endpoint"))
	}

	return client, nil
}

// ProxyListSessions lists sessions from one or all of the user's daemons.
func (s *Server) ProxyListSessions(ctx context.Context, req *connect.Request[pb.ProxyListSessionsRequest]) (*connect.Response[pb.ProxyListSessionsResponse], error) {
	userID, err := resolveUserID(ctx)
	if err != nil {
		return nil, err
	}

	msg := req.Msg

	// If a specific daemon is requested, proxy to it directly.
	if msg.DaemonId != nil && *msg.DaemonId != "" {
		daemon, err := s.daemons.Get(ctx, *msg.DaemonId)
		if err != nil {
			if err == sql.ErrNoRows {
				return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("daemon not found"))
			}
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get daemon: %w", err))
		}
		if daemon.UserID != userID {
			return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("daemon belongs to another user"))
		}

		client := s.pool.Get(daemon.ID)
		if client == nil {
			return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("daemon has no proxy endpoint"))
		}

		sessions, err := s.proxyListSessionsFromDaemon(ctx, client, msg)
		if err != nil {
			return nil, err
		}
		return connect.NewResponse(&pb.ProxyListSessionsResponse{Sessions: sessions}), nil
	}

	// No daemon specified: query all user's online daemons.
	daemons, err := s.daemons.ListByUser(ctx, userID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list daemons: %w", err))
	}

	var allSessions []*pb.Session
	for _, d := range daemons {
		if !d.Online {
			continue
		}
		client := s.pool.Get(d.ID)
		if client == nil {
			continue
		}
		sessions, err := s.proxyListSessionsFromDaemon(ctx, client, msg)
		if err != nil {
			// Skip daemons that fail — partial results are better than none.
			continue
		}
		allSessions = append(allSessions, sessions...)
	}

	return connect.NewResponse(&pb.ProxyListSessionsResponse{Sessions: allSessions}), nil
}

// proxyListSessionsFromDaemon forwards a ListSessions request to a daemon.
func (s *Server) proxyListSessionsFromDaemon(ctx context.Context, client bossanovav1connect.DaemonServiceClient, msg *pb.ProxyListSessionsRequest) ([]*pb.Session, error) {
	daemonReq := connect.NewRequest(&pb.ListSessionsRequest{
		RepoId:          msg.RepoId,
		States:          msg.States,
		IncludeArchived: msg.IncludeArchived,
	})

	resp, err := client.ListSessions(ctx, daemonReq)
	if err != nil {
		return nil, err
	}
	return resp.Msg.Sessions, nil
}

// ProxyGetSession retrieves a session from its daemon with ownership verification.
func (s *Server) ProxyGetSession(ctx context.Context, req *connect.Request[pb.ProxyGetSessionRequest]) (*connect.Response[pb.ProxyGetSessionResponse], error) {
	client, err := s.getDaemonClient(ctx, req.Msg.Id)
	if err != nil {
		return nil, err
	}

	daemonReq := connect.NewRequest(&pb.GetSessionRequest{
		Id: req.Msg.Id,
	})

	resp, err := client.GetSession(ctx, daemonReq)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("proxy get session: %w", err))
	}

	return connect.NewResponse(&pb.ProxyGetSessionResponse{
		Session: resp.Msg.Session,
	}), nil
}

// ProxyAttachSession relays a streaming attach from the daemon to the caller.
func (s *Server) ProxyAttachSession(ctx context.Context, req *connect.Request[pb.ProxyAttachSessionRequest], stream *connect.ServerStream[pb.ProxyAttachSessionResponse]) error {
	client, err := s.getDaemonClient(ctx, req.Msg.Id)
	if err != nil {
		return err
	}

	daemonReq := connect.NewRequest(&pb.AttachSessionRequest{
		Id: req.Msg.Id,
	})

	daemonStream, err := client.AttachSession(ctx, daemonReq)
	if err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("proxy attach session: %w", err))
	}
	defer func() { _ = daemonStream.Close() }()

	for daemonStream.Receive() {
		msg := daemonStream.Msg()
		proxyMsg := &pb.ProxyAttachSessionResponse{}

		switch ev := msg.Event.(type) {
		case *pb.AttachSessionResponse_OutputLine:
			proxyMsg.Event = &pb.ProxyAttachSessionResponse_OutputLine{
				OutputLine: ev.OutputLine,
			}
		case *pb.AttachSessionResponse_StateChange:
			proxyMsg.Event = &pb.ProxyAttachSessionResponse_StateChange{
				StateChange: ev.StateChange,
			}
		case *pb.AttachSessionResponse_SessionEnded:
			proxyMsg.Event = &pb.ProxyAttachSessionResponse_SessionEnded{
				SessionEnded: ev.SessionEnded,
			}
		}

		if err := stream.Send(proxyMsg); err != nil {
			return err
		}
	}

	if err := daemonStream.Err(); err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("daemon stream error: %w", err))
	}

	return nil
}

// ProxyStopSession stops a session on its daemon with ownership verification.
func (s *Server) ProxyStopSession(ctx context.Context, req *connect.Request[pb.ProxyStopSessionRequest]) (*connect.Response[pb.ProxyStopSessionResponse], error) {
	client, err := s.getDaemonClient(ctx, req.Msg.Id)
	if err != nil {
		return nil, err
	}

	daemonReq := connect.NewRequest(&pb.StopSessionRequest{
		Id: req.Msg.Id,
	})

	resp, err := client.StopSession(ctx, daemonReq)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("proxy stop session: %w", err))
	}

	return connect.NewResponse(&pb.ProxyStopSessionResponse{
		Session: resp.Msg.Session,
	}), nil
}

// ProxyPauseSession pauses a session on its daemon with ownership verification.
func (s *Server) ProxyPauseSession(ctx context.Context, req *connect.Request[pb.ProxyPauseSessionRequest]) (*connect.Response[pb.ProxyPauseSessionResponse], error) {
	client, err := s.getDaemonClient(ctx, req.Msg.Id)
	if err != nil {
		return nil, err
	}

	daemonReq := connect.NewRequest(&pb.PauseSessionRequest{
		Id: req.Msg.Id,
	})

	resp, err := client.PauseSession(ctx, daemonReq)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("proxy pause session: %w", err))
	}

	return connect.NewResponse(&pb.ProxyPauseSessionResponse{
		Session: resp.Msg.Session,
	}), nil
}

// ProxyResumeSession resumes a session on its daemon with ownership verification.
func (s *Server) ProxyResumeSession(ctx context.Context, req *connect.Request[pb.ProxyResumeSessionRequest]) (*connect.Response[pb.ProxyResumeSessionResponse], error) {
	client, err := s.getDaemonClient(ctx, req.Msg.Id)
	if err != nil {
		return nil, err
	}

	daemonReq := connect.NewRequest(&pb.ResumeSessionRequest{
		Id: req.Msg.Id,
	})

	resp, err := client.ResumeSession(ctx, daemonReq)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("proxy resume session: %w", err))
	}

	return connect.NewResponse(&pb.ProxyResumeSessionResponse{
		Session: resp.Msg.Session,
	}), nil
}
