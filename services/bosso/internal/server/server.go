// Package server provides the ConnectRPC handler for the OrchestratorService.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bosso/internal/auth"
	"github.com/recurser/bosso/internal/db"
	"github.com/recurser/bosso/internal/relay"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Server implements the OrchestratorServiceHandler.
type Server struct {
	users    db.UserStore
	daemons  db.DaemonStore
	sessions db.SessionRegistryStore
	audit    db.AuditStore
	webhooks db.WebhookConfigStore
	pool     *relay.Pool

	bossanovav1connect.UnimplementedOrchestratorServiceHandler
}

// New creates a new orchestrator server.
func New(users db.UserStore, daemons db.DaemonStore, sessions db.SessionRegistryStore, audit db.AuditStore, webhooks db.WebhookConfigStore, pool *relay.Pool) *Server {
	return &Server{
		users:    users,
		daemons:  daemons,
		sessions: sessions,
		audit:    audit,
		webhooks: webhooks,
		pool:     pool,
	}
}

// RegisterDaemon registers a daemon with the orchestrator. Requires user auth (OIDC JWT).
func (s *Server) RegisterDaemon(ctx context.Context, req *connect.Request[pb.RegisterDaemonRequest]) (*connect.Response[pb.RegisterDaemonResponse], error) {
	info := auth.InfoFromContext(ctx)
	if info == nil || !info.IsUser() {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("user authentication required"))
	}

	msg := req.Msg
	if msg.DaemonId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("daemon_id is required"))
	}
	if msg.Hostname == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("hostname is required"))
	}

	// Generate a session token for the daemon.
	token, err := generateToken()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("generate token: %w", err))
	}

	endpoint := ""
	if msg.Endpoint != nil {
		endpoint = *msg.Endpoint
	}

	daemon, err := s.daemons.Create(ctx, db.CreateDaemonParams{
		ID:           msg.DaemonId,
		UserID:       info.UserID,
		Hostname:     msg.Hostname,
		Endpoint:     endpoint,
		SessionToken: token,
		RepoIDs:      msg.RepoIds,
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("register daemon: %w", err))
	}

	// Register in relay pool if endpoint provided.
	if endpoint != "" {
		s.pool.Register(daemon.ID, endpoint)
	}

	// Audit log.
	detail := fmt.Sprintf("daemon_id=%s hostname=%s", daemon.ID, daemon.Hostname)
	_, _ = s.audit.Create(ctx, db.CreateAuditParams{
		UserID:   &info.UserID,
		Action:   "daemon.register",
		Resource: "daemon:" + daemon.ID,
		Detail:   &detail,
	})

	return connect.NewResponse(&pb.RegisterDaemonResponse{
		DaemonId:     daemon.ID,
		SessionToken: token,
	}), nil
}

// Heartbeat updates a daemon's heartbeat timestamp and active session count.
// Requires daemon auth (session token).
func (s *Server) Heartbeat(ctx context.Context, req *connect.Request[pb.HeartbeatRequest]) (*connect.Response[pb.HeartbeatResponse], error) {
	info := auth.InfoFromContext(ctx)
	if info == nil || !info.IsDaemon() {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("daemon authentication required"))
	}

	msg := req.Msg
	if msg.DaemonId == "" || msg.DaemonId != info.DaemonID {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("daemon_id mismatch"))
	}

	ts := time.Now().UTC()
	if msg.Timestamp != nil {
		ts = msg.Timestamp.AsTime()
	}
	heartbeat := ts.Format("2006-01-02T15:04:05.000Z")
	activeSessions := int(msg.ActiveSessions)
	online := true

	_, err := s.daemons.Update(ctx, info.DaemonID, db.UpdateDaemonParams{
		LastHeartbeat:  &heartbeat,
		ActiveSessions: &activeSessions,
		Online:         &online,
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("update heartbeat: %w", err))
	}

	return connect.NewResponse(&pb.HeartbeatResponse{}), nil
}

// ListDaemons returns all daemons owned by the authenticated user.
// Accepts both user auth and daemon auth.
func (s *Server) ListDaemons(ctx context.Context, _ *connect.Request[pb.ListDaemonsRequest]) (*connect.Response[pb.ListDaemonsResponse], error) {
	info := auth.InfoFromContext(ctx)
	if info == nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("authentication required"))
	}

	var userID string
	if info.IsUser() {
		userID = info.UserID
	} else {
		userID = info.DaemonUserID
	}

	daemons, err := s.daemons.ListByUser(ctx, userID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list daemons: %w", err))
	}

	pbDaemons := make([]*pb.DaemonInfo, len(daemons))
	for i, d := range daemons {
		pbDaemons[i] = daemonToProto(d)
	}

	return connect.NewResponse(&pb.ListDaemonsResponse{Daemons: pbDaemons}), nil
}

// daemonToProto converts a db.Daemon to protobuf DaemonInfo.
func daemonToProto(d *db.Daemon) *pb.DaemonInfo {
	info := &pb.DaemonInfo{
		DaemonId:       d.ID,
		Hostname:       d.Hostname,
		RepoIds:        d.RepoIDs,
		ActiveSessions: int32(d.ActiveSessions),
		Online:         d.Online,
	}
	if d.LastHeartbeat != nil {
		info.LastHeartbeat = timestamppb.New(*d.LastHeartbeat)
	}
	return info
}

// generateToken generates a cryptographically random 32-byte hex token.
func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
