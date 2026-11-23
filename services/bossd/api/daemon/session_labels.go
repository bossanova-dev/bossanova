package server

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/domain"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// SessionLabelToProto converts a models.SessionLabel to its wire representation.
func SessionLabelToProto(l *models.SessionLabel) *pb.SessionLabel {
	if l == nil {
		return nil
	}
	out := &pb.SessionLabel{
		Id:           l.ID,
		Name:         protoString(l.Name),
		Color:        protoString(l.Color),
		Description:  protoString(l.Description),
		SessionCount: clampInt32(l.SessionCount),
	}
	if !l.CreatedAt.IsZero() {
		out.CreatedAt = timestamppb.New(l.CreatedAt)
	}
	if !l.UpdatedAt.IsZero() {
		out.UpdatedAt = timestamppb.New(l.UpdatedAt)
	}
	return out
}

// SessionLabelsToProto converts a slice of models.SessionLabel to protobuf.
func SessionLabelsToProto(labels []*models.SessionLabel) []*pb.SessionLabel {
	if len(labels) == 0 {
		return nil
	}
	out := make([]*pb.SessionLabel, 0, len(labels))
	for _, l := range labels {
		if p := SessionLabelToProto(l); p != nil {
			out = append(out, p)
		}
	}
	return out
}

func (s *Server) hydrateSessionLabels(ctx context.Context, p *pb.Session) {
	if s.sessionLabels == nil || p == nil || p.Id == "" {
		return
	}
	labels, err := s.sessionLabels.ListForSession(ctx, p.Id)
	if err != nil {
		s.logger.Warn().Err(err).Str("session", p.Id).Msg("hydrate session labels failed")
		return
	}
	p.Labels = SessionLabelsToProto(labels)
}

func (s *Server) hydrateSessionLabelsBatch(ctx context.Context, sessions []*pb.Session) {
	if s.sessionLabels == nil || len(sessions) == 0 {
		return
	}
	ids := make([]string, 0, len(sessions))
	for _, p := range sessions {
		if p != nil && p.Id != "" {
			ids = append(ids, p.Id)
		}
	}
	if len(ids) == 0 {
		return
	}
	bySession, err := s.sessionLabels.ListForSessions(ctx, ids)
	if err != nil {
		s.logger.Warn().Err(err).Msg("hydrate session labels batch failed")
		return
	}
	for _, p := range sessions {
		if p == nil {
			continue
		}
		if labels := bySession[p.Id]; len(labels) > 0 {
			p.Labels = SessionLabelsToProto(labels)
		}
	}
}

func filterSessionsByLabels(sessions []*pb.Session, want []string) []*pb.Session {
	if len(want) == 0 {
		return sessions
	}
	wantSet := make(map[string]struct{}, len(want))
	for _, raw := range want {
		key := strings.ToLower(strings.TrimSpace(raw))
		if key != "" {
			wantSet[key] = struct{}{}
		}
	}
	if len(wantSet) == 0 {
		return sessions
	}
	filtered := make([]*pb.Session, 0, len(sessions))
	for _, sess := range sessions {
		for _, lbl := range sess.GetLabels() {
			if _, ok := wantSet[strings.ToLower(lbl.GetId())]; ok {
				filtered = append(filtered, sess)
				break
			}
			if _, ok := wantSet[strings.ToLower(lbl.GetName())]; ok {
				filtered = append(filtered, sess)
				break
			}
		}
	}
	return filtered
}

func (s *Server) ListSessionLabels(ctx context.Context, _ *connect.Request[pb.ListSessionLabelsRequest]) (*connect.Response[pb.ListSessionLabelsResponse], error) {
	if s.sessionLabels == nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("session label store unavailable"))
	}
	labels, err := s.sessionLabels.List(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list session labels: %w", err))
	}
	return connect.NewResponse(&pb.ListSessionLabelsResponse{
		Labels: SessionLabelsToProto(labels),
	}), nil
}

func (s *Server) CreateSessionLabel(ctx context.Context, req *connect.Request[pb.CreateSessionLabelRequest]) (*connect.Response[pb.CreateSessionLabelResponse], error) {
	if s.sessionLabels == nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("session label store unavailable"))
	}
	msg := req.Msg
	if _, err := models.NormalizeLabelName(msg.GetName()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if _, err := models.NormalizeLabelColor(msg.GetColor()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if _, err := models.NormalizeLabelDescription(msg.GetDescription()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	lbl, err := s.sessionLabels.Create(ctx, domain.CreateSessionLabelParams{
		Name:        msg.GetName(),
		Color:       msg.GetColor(),
		Description: msg.GetDescription(),
	})
	if err != nil {
		if strings.Contains(err.Error(), "already exists") {
			return nil, connect.NewError(connect.CodeAlreadyExists, err)
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create session label: %w", err))
	}
	return connect.NewResponse(&pb.CreateSessionLabelResponse{
		Label: SessionLabelToProto(lbl),
	}), nil
}

func (s *Server) UpdateSessionLabel(ctx context.Context, req *connect.Request[pb.UpdateSessionLabelRequest]) (*connect.Response[pb.UpdateSessionLabelResponse], error) {
	if s.sessionLabels == nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("session label store unavailable"))
	}
	msg := req.Msg
	if strings.TrimSpace(msg.GetId()) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}
	params := domain.UpdateSessionLabelParams{}
	if msg.Name != nil {
		if _, err := models.NormalizeLabelName(*msg.Name); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		params.Name = msg.Name
	}
	if msg.Color != nil {
		if _, err := models.NormalizeLabelColor(*msg.Color); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		params.Color = msg.Color
	}
	if msg.Description != nil {
		if _, err := models.NormalizeLabelDescription(*msg.Description); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		params.Description = msg.Description
	}
	lbl, err := s.sessionLabels.Update(ctx, msg.GetId(), params)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session label not found"))
		}
		if strings.Contains(err.Error(), "already exists") {
			return nil, connect.NewError(connect.CodeAlreadyExists, err)
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("update session label: %w", err))
	}
	s.notifySessionsForLabel(ctx, lbl.ID)
	return connect.NewResponse(&pb.UpdateSessionLabelResponse{
		Label: SessionLabelToProto(lbl),
	}), nil
}

func (s *Server) DeleteSessionLabel(ctx context.Context, req *connect.Request[pb.DeleteSessionLabelRequest]) (*connect.Response[pb.DeleteSessionLabelResponse], error) {
	if s.sessionLabels == nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("session label store unavailable"))
	}
	id := strings.TrimSpace(req.Msg.GetId())
	if id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}
	affected, err := s.sessionLabels.Delete(ctx, id)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session label not found"))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("delete session label: %w", err))
	}
	s.notifySessionIDsUpdated(ctx, affected)
	return connect.NewResponse(&pb.DeleteSessionLabelResponse{}), nil
}

func (s *Server) SetSessionLabels(ctx context.Context, req *connect.Request[pb.SetSessionLabelsRequest]) (*connect.Response[pb.SetSessionLabelsResponse], error) {
	if s.sessionLabels == nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("session label store unavailable"))
	}
	sessionID := strings.TrimSpace(req.Msg.GetSessionId())
	if sessionID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("session_id is required"))
	}
	sess, err := s.sessions.Get(ctx, sessionID)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session not found: %w", err))
	}
	resolvedIDs, err := s.resolveLabelIDs(ctx, req.Msg.GetLabelIds())
	if err != nil {
		return nil, err
	}
	labels, err := s.sessionLabels.SetForSession(ctx, sessionID, resolvedIDs)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("set session labels: %w", err))
	}
	p := s.sessionProtoWithRepo(ctx, sess)
	p.Labels = SessionLabelsToProto(labels)
	if s.onSessionUpdated != nil {
		s.onSessionUpdated(ctx, p)
	}
	return connect.NewResponse(&pb.SetSessionLabelsResponse{Session: p}), nil
}

// resolveLabelIDs accepts either label IDs or case-insensitive label names.
func (s *Server) resolveLabelIDs(ctx context.Context, inputs []string) ([]string, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	resolved := make([]string, 0, len(inputs))
	for _, raw := range inputs {
		ref := strings.TrimSpace(raw)
		if ref == "" {
			continue
		}
		if lbl, err := s.sessionLabels.Get(ctx, ref); err == nil {
			resolved = append(resolved, lbl.ID)
			continue
		}
		lbl, err := s.sessionLabels.GetByName(ctx, ref)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session label %q not found", ref))
			}
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("resolve session label %q: %w", ref, err))
		}
		resolved = append(resolved, lbl.ID)
	}
	return resolved, nil
}

func (s *Server) notifySessionsForLabel(ctx context.Context, labelID string) {
	if s.onSessionUpdated == nil || s.sessionLabels == nil {
		return
	}
	ids, err := s.sessionLabels.ListSessionIDsByLabel(ctx, labelID)
	if err != nil {
		return
	}
	s.notifySessionIDsUpdated(ctx, ids)
}

func (s *Server) notifySessionIDsUpdated(ctx context.Context, sessionIDs []string) {
	if s.onSessionUpdated == nil || len(sessionIDs) == 0 {
		return
	}
	for _, sid := range sessionIDs {
		sess, err := s.sessions.Get(ctx, sid)
		if err != nil {
			continue
		}
		p := s.sessionProtoWithRepo(ctx, sess)
		s.hydrateSessionLabels(ctx, p)
		s.onSessionUpdated(ctx, p)
	}
}
