package server

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/domain"
)

// GetChatTranscript reads a chat's conversation by routing to the owning agent
// plugin's ReadTranscript RPC. It resolves the owning agent + worktree from the
// agent_chats row when one exists; when it does not (e.g. a freshly created
// headless run whose row hasn't been created/propagated yet) it falls back to
// the session, which carries the same agent_session_id and worktree. The
// fallback requires a session_id (the ownership scope) and an exact
// agent_session_id match, so it cannot read a chat the caller didn't scope.
//
// A structured read (should_include_activity) also ends on the question the
// chat's terminal menu shows, when the transcript does not already: Claude
// Code records a question tool call only once it is answered, so while its
// menu waits the pane is the only record of it (withMenuQuestion).
func (s *Server) GetChatTranscript(ctx context.Context, req *connect.Request[pb.GetChatTranscriptRequest]) (*connect.Response[pb.GetChatTranscriptResponse], error) {
	resp, source, err := s.readChatTranscript(ctx, req)
	if err != nil || !req.Msg.GetShouldIncludeActivity() {
		return resp, err
	}
	s.withMenuQuestion(ctx, resp.Msg, source)
	return resp, nil
}

// transcriptSource is where a chat's transcript was read: the agent's runner
// and the chat's tmux session, which shows any question menu it waits on.
type transcriptSource struct {
	client   agent.RunnerClient
	tmuxName string
}

func (s *Server) readChatTranscript(ctx context.Context, req *connect.Request[pb.GetChatTranscriptRequest]) (*connect.Response[pb.GetChatTranscriptResponse], transcriptSource, error) {
	agentSessionID := req.Msg.GetAgentSessionId()

	var (
		worktreePath string
		agentName    string
		// providerSessionID is codex's own rollout UUID (or another provider's
		// resume id), tracked separately from the caller-supplied
		// agent_session_id. Hoisted out of the chat-row branch so the read
		// fallback below can retry ReadTranscript keyed by it on a miss.
		providerSessionID string
		tmuxName          string
	)

	chat, err := s.agentChats.GetByAgentSessionID(ctx, agentSessionID)
	if err == nil && chat != nil {
		if chat.ProviderSessionID != nil {
			providerSessionID = *chat.ProviderSessionID
		}
		if reqID := req.Msg.GetSessionId(); reqID != "" && chat.SessionID != reqID {
			return nil, transcriptSource{}, connect.NewError(connect.CodeNotFound, fmt.Errorf("chat %s not found in session %s", agentSessionID, reqID))
		}
		sess, serr := s.sessions.Get(ctx, chat.SessionID)
		if serr != nil || sess == nil {
			return nil, transcriptSource{}, connect.NewError(connect.CodeNotFound, fmt.Errorf("session not found for chat %s", agentSessionID))
		}
		worktreePath = sess.WorktreePath
		agentName = chat.AgentName
		tmuxName = chatTmuxName(chat, sess.RepoID)
	} else {
		// No agent_chats row — fall back to the session. Requires session_id
		// (ownership scope) and an exact agent_session_id match.
		reqID := req.Msg.GetSessionId()
		if reqID == "" {
			return nil, transcriptSource{}, connect.NewError(connect.CodeNotFound, fmt.Errorf("chat not found: %s", agentSessionID))
		}
		sess, serr := s.sessions.Get(ctx, reqID)
		if serr != nil || sess == nil || sess.AgentSessionID == nil || *sess.AgentSessionID != agentSessionID {
			return nil, transcriptSource{}, connect.NewError(connect.CodeNotFound, fmt.Errorf("chat not found: %s", agentSessionID))
		}
		worktreePath = sess.WorktreePath
		agentName = sess.AgentName
		tmuxName = domain.ChatSessionName(sess.RepoID, agentSessionID)
	}

	if agentName == "" {
		agentName = defaultLegacyAgent
	}
	client, ok := s.agentClients[agentName]
	if !ok {
		return nil, transcriptSource{}, connect.NewError(connect.CodeFailedPrecondition, agent.RunnerNotLoaded(agentName, s.agentClients))
	}

	pluginResp, err := client.ReadTranscript(ctx, &pb.ReadTranscriptRequest{
		WorkDir:               worktreePath,
		AgentSessionId:        agentSessionID,
		MaxMessages:           req.Msg.GetMaxMessages(),
		ShouldIncludeActivity: req.Msg.GetShouldIncludeActivity(),
	})
	if err != nil {
		return nil, transcriptSource{}, connect.NewError(connect.CodeInternal, fmt.Errorf("read transcript: %w", err))
	}
	if pluginResp.GetExists() {
		return transcriptResponse(pluginResp, ""), transcriptSource{client, tmuxName}, nil
	}

	// Primary read (keyed by agent_session_id) missed. Agents like codex mint
	// their own rollout UUID at boot and key the on-disk transcript by it, so a
	// record_chat chat whose caller-supplied agent_session_id differs from that
	// UUID misses here. The daemon persists the rollout UUID as
	// chat.ProviderSessionID; retry once keyed by it when it differs from the
	// agent_session_id (avoid a redundant identical glob). claude launches with
	// --session-id <agent_session_id>, so its ids coincide and it never retries.
	if providerSessionID != "" && providerSessionID != agentSessionID {
		retryResp, retryErr := client.ReadTranscript(ctx, &pb.ReadTranscriptRequest{
			WorkDir:               worktreePath,
			AgentSessionId:        providerSessionID,
			MaxMessages:           req.Msg.GetMaxMessages(),
			ShouldIncludeActivity: req.Msg.GetShouldIncludeActivity(),
		})
		if retryErr != nil {
			return nil, transcriptSource{}, connect.NewError(connect.CodeInternal, fmt.Errorf("read transcript by provider session id: %w", retryErr))
		}
		if retryResp.GetExists() {
			return transcriptResponse(retryResp, ""), transcriptSource{client, tmuxName}, nil
		}
		// Provider id known but the rollout file is still absent on disk.
		return transcriptResponse(retryResp, "transcript not found on disk"), transcriptSource{client, tmuxName}, nil
	}

	// Genuine miss with no usable provider id to retry. Fail loud with a reason
	// so callers never see a bare {} that reads like "no messages yet".
	reason := "transcript not found"
	if chat != nil {
		// A chat row exists but the transcript is not readable yet. Keep the
		// wording agent-accurate: only codex mints a separate rollout UUID that
		// the async discovery races the read to persist; claude keys its
		// transcript by the agent_session_id and never has a "rollout".
		reason = "transcript not yet available for this chat"
		if chat.AgentName == "codex" {
			reason = "codex rollout not yet discovered for this chat"
		}
	}
	return transcriptResponse(pluginResp, reason), transcriptSource{client, tmuxName}, nil
}

// transcriptResponse builds a GetChatTranscriptResponse from a plugin
// ReadTranscript reply, attaching reason (empty on a hit).
func transcriptResponse(pluginResp *pb.ReadTranscriptResponse, reason string) *connect.Response[pb.GetChatTranscriptResponse] {
	return connect.NewResponse(&pb.GetChatTranscriptResponse{
		Messages:           pluginResp.GetMessages(),
		FinalAssistantText: pluginResp.GetFinalAssistantText(),
		Exists:             pluginResp.GetExists(),
		Reason:             reason,
	})
}

type questionMenuReader interface {
	ReadQuestionMenu(context.Context, *pb.ReadQuestionMenuRequest) (*pb.ReadQuestionMenuResponse, error)
}

// withMenuQuestion ends resp on the question the chat's terminal menu shows,
// unless resp already ends on it. Best effort: a chat that is not running, an
// agent with no question menu, or a pane that cannot be read leaves resp as
// the transcript has it.
func (s *Server) withMenuQuestion(ctx context.Context, resp *pb.GetChatTranscriptResponse, source transcriptSource) {
	reader, ok := source.client.(questionMenuReader)
	tmuxName := source.tmuxName
	chatPane, isPane := s.chatTmux.(ChatPane)
	if !ok || !isPane || !resp.GetExists() || tmuxName == "" || !chatPane.HasSession(ctx, tmuxName) {
		return
	}
	pane, err := chatPane.CapturePane(ctx, tmuxName)
	if err != nil {
		return
	}
	read, err := reader.ReadQuestionMenu(ctx, &pb.ReadQuestionMenuRequest{PaneContent: []byte(pane)})
	question := read.GetQuestion()
	if err != nil || question == nil {
		return
	}
	if n := len(resp.GetMessages()); n > 0 && resp.GetMessages()[n-1].GetQuestion().GetId() == question.GetId() {
		return
	}
	resp.Messages = append(resp.Messages, &pb.ChatMessage{
		Role: "assistant", Kind: "question", Text: question.GetText(),
		Timestamp: time.Now().UTC().Format(time.RFC3339), Question: question,
	})
}
