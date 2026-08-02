package server

import (
	"context"
	"fmt"
	"os"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// DescribeChatLaunch re-derives the exact command bossd would run to launch a
// chat's agent in its worktree and returns it (plus the daemon host) so the TUI
// can show a copy-pasteable reproduction command when a pane dies on startup.
// The usual cause is a PATH / login-shell problem that only reproduces on the
// daemon host, where a `tmux new-session` succeeds but the agent fails to exec.
//
// It mirrors spawnChatTmux's resume decision but deliberately passes an empty
// system prompt and MCP config: those flags do not change whether the agent
// binary resolves through the login shell — the failure this surfaces — and
// omitting them keeps the displayed command readable.
func (s *Server) DescribeChatLaunch(ctx context.Context, req *connect.Request[pb.DescribeChatLaunchRequest]) (*connect.Response[pb.DescribeChatLaunchResponse], error) {
	agentSessionID := req.Msg.GetAgentSessionId()
	if agentSessionID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("agent_session_id is required"))
	}

	chat, err := s.agentChats.GetByAgentSessionID(ctx, agentSessionID)
	if err != nil || chat == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("chat not found for agent_session_id %q", agentSessionID))
	}
	sess, err := s.sessions.Get(ctx, chat.SessionID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get session: %w", err))
	}

	// Mirror the wakeHook seam WakeChat uses so tests can inject fakes and the
	// resume decision matches the real spawn path.
	var transcripts transcriptOracle = liveTranscriptOracle{clients: s.agentClients}
	if s.wakeHook.transcripts != nil {
		transcripts = s.wakeHook.transcripts
	}
	var builder argvBuilder = liveArgvBuilder{clients: s.agentClients}
	if s.wakeHook.argv != nil {
		builder = s.wakeHook.argv
	}

	resumeID, _ := chatResumeSessionID(chat)
	resume := transcripts.TranscriptExists(ctx, chat.AgentName, sess.WorktreePath, resumeID)

	// This is a read-only preview, not a spawn: it deliberately passes an empty
	// append_system_prompt, so there are no instruction classes to report and
	// this path emits no undelivered-instruction record whatever the runner
	// declares.
	cmdResp, err := builder.BuildInteractive(ctx, chat.AgentName, resumeID, resume, sess.WorktreePath, "", "", chat.Model, "", false)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("build launch command: %w", err))
	}

	// Best-effort: a hostname lookup failure is non-fatal — the command and
	// worktree are still useful, so return what we have with an empty host.
	host, _ := os.Hostname()

	// captured_output carries the agent's own final output if the status poller
	// grabbed it at pane death (BOS-477) — e.g. "Session ID … already in use".
	// Empty when nothing was captured; the TUI falls back to the neutral prose.
	var captured string
	if s.chatStatus != nil {
		captured = s.chatStatus.CapturedOutput(agentSessionID)
	}

	return connect.NewResponse(&pb.DescribeChatLaunchResponse{
		Argv:           cmdResp.GetArgv(),
		WorktreePath:   sess.WorktreePath,
		Host:           host,
		AgentName:      chat.AgentName,
		CapturedOutput: captured,
	}), nil
}
