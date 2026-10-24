package server

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/displaystatus"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (s *Server) SetChatPhase(ctx context.Context, req *connect.Request[pb.SetChatPhaseRequest]) (*connect.Response[pb.SetChatPhaseResponse], error) {
	if req.Msg.AgentSessionId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("agent_session_id is required"))
	}
	chat, err := s.agentChats.GetByAgentSessionID(ctx, req.Msg.AgentSessionId)
	if err != nil {
		if isAgentChatNotFound(err) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("chat not found"))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get chat: %w", err))
	}
	if chat == nil || (req.Msg.SessionId != "" && chat.SessionID != req.Msg.SessionId) {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("chat not found in requested session"))
	}
	phase := req.Msg.Phase
	if phase != "" {
		phase, err = displaystatus.NormalizePhase(phase)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
	}
	if s.chatStatus == nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("chat status tracker is unavailable"))
	}
	s.chatStatus.SetPhase(req.Msg.AgentSessionId, phase)
	return connect.NewResponse(&pb.SetChatPhaseResponse{}), nil
}

func (s *Server) ReportChatStatus(ctx context.Context, req *connect.Request[pb.ReportChatStatusRequest]) (*connect.Response[pb.ReportChatStatusResponse], error) {
	if s.chatStatus == nil {
		return connect.NewResponse(&pb.ReportChatStatusResponse{}), nil
	}
	for _, r := range req.Msg.Reports {
		if r.AgentSessionId == "" {
			continue
		}
		// Tmux-tracked chats are owned by the daemon's TmuxStatusPoller,
		// which captures the live pane every 3 s. The boss CLI heartbeat
		// reports the same chat from its own PTY ring buffer (or worse, a
		// stale `tmux attach` child whose Process.done has closed and
		// reports STOPPED forever) — accepting both writers makes the
		// tracker oscillate and the session label flash between
		// "? question" and "draft". Drop client reports for such chats
		// and let the poller be the single source of truth.
		if s.agentChats != nil {
			if chat, err := s.agentChats.GetByAgentSessionID(ctx, r.AgentSessionId); err == nil &&
				chat != nil && chat.TmuxSessionName != nil && *chat.TmuxSessionName != "" {
				continue
			}
		}
		var lastOutputAt time.Time
		if r.LastOutputAt != nil {
			lastOutputAt = r.LastOutputAt.AsTime()
		}
		s.chatStatus.Update(r.AgentSessionId, r.Status, lastOutputAt)
	}
	return connect.NewResponse(&pb.ReportChatStatusResponse{}), nil
}

func (s *Server) GetChatStatuses(ctx context.Context, req *connect.Request[pb.GetChatStatusesRequest]) (*connect.Response[pb.GetChatStatusesResponse], error) {
	if req.Msg.SessionId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("session_id is required"))
	}

	// Look up chats for this session.
	chats, err := s.agentChats.ListBySession(ctx, req.Msg.SessionId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list chats: %w", err))
	}

	if s.chatStatus == nil || len(chats) == 0 {
		return connect.NewResponse(&pb.GetChatStatusesResponse{}), nil
	}

	agentSessionIDs := make([]string, len(chats))
	for i, c := range chats {
		agentSessionIDs[i] = c.AgentSessionID
	}

	entries := s.chatStatus.GetBatch(agentSessionIDs)

	// Build a map of which chats have live tmux sessions (per-chat liveness).
	tmuxAlive := make(map[string]bool, len(chats))
	for _, c := range chats {
		if c.TmuxSessionName != nil && *c.TmuxSessionName != "" &&
			s.lifecycle.IsTmuxSessionAlive(ctx, *c.TmuxSessionName) {
			tmuxAlive[c.AgentSessionID] = true
		}
	}

	statuses := make([]*pb.ChatStatusEntry, 0, len(entries))
	for id, e := range entries {
		entry := &pb.ChatStatusEntry{
			AgentSessionId: id,
			Status:         e.Status,
		}
		if !e.LastOutputAt.IsZero() {
			entry.LastOutputAt = timestamppb.New(e.LastOutputAt)
		}
		// When heartbeats report stopped, fall back to per-chat tmux liveness.
		// Default to IDLE (not WORKING) — the poller will upgrade to WORKING
		// within 3s if Claude is actually producing output.
		if entry.Status == pb.ChatStatus_CHAT_STATUS_STOPPED && tmuxAlive[id] {
			entry.Status = pb.ChatStatus_CHAT_STATUS_IDLE
		}
		applyWaitingMarker(entry, s.chatStatus.Waiting(id))
		if entry.Status == pb.ChatStatus_CHAT_STATUS_WORKING {
			entry.Phase = s.chatStatus.Phase(id)
		}
		spinnerPresent, substantiveAt, seeded := s.chatStatus.Liveness(id)
		applyLivenessMarkers(entry, spinnerPresent, substantiveAt, seeded)
		statuses = append(statuses, entry)
	}

	// If any tmux-active chat had no heartbeat entry at all, add a synthetic one.
	for _, c := range chats {
		if !tmuxAlive[c.AgentSessionID] {
			continue
		}
		found := false
		for _, st := range statuses {
			if st.AgentSessionId == c.AgentSessionID {
				found = true
				break
			}
		}
		if !found {
			synthetic := &pb.ChatStatusEntry{
				AgentSessionId: c.AgentSessionID,
				Status:         pb.ChatStatus_CHAT_STATUS_IDLE,
			}
			spinnerPresent, substantiveAt, seeded := s.chatStatus.Liveness(c.AgentSessionID)
			applyLivenessMarkers(synthetic, spinnerPresent, substantiveAt, seeded)
			statuses = append(statuses, synthetic)
		}
	}

	return connect.NewResponse(&pb.GetChatStatusesResponse{Statuses: statuses}), nil
}

// --- Auth Change Notification ---
