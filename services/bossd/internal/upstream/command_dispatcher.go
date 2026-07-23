package upstream

import (
	"context"
	"fmt"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/safego"
)

// dispatchCommand routes an inbound OrchestratorCommand to the matching
// daemon handler and returns the DaemonEvent that should be sent back
// on the stream. For stop/pause/resume/webhook the response fits in a
// single DaemonEvent (CommandResult or WebhookAck). For attach, the
// dispatcher kicks off a streaming goroutine and returns an immediate
// CommandResult{Ok:true} so the orchestrator knows the attach started
// — subsequent chunks flow via outbound as DaemonEvent_AttachChunk
// events correlated back via the shared command_id.
//
// Unknown oneof values (from a newer bosso) log and return nil. No
// CommandResult is emitted, matching the forward-compat contract in
// the design doc: the daemon MUST NOT invent a failure for commands
// it doesn't understand — bosso will time out its own waiter.
func (c *StreamClient) dispatchCommand(
	ctx context.Context,
	cmd *pb.OrchestratorCommand,
	outbound chan<- *pb.DaemonEvent,
) *pb.DaemonEvent {
	if cmd == nil {
		return nil
	}
	cmdID := cmd.GetCommandId()

	switch cmd.GetCmd().(type) {
	case *pb.OrchestratorCommand_Stop:
		return c.dispatchStop(ctx, cmdID, cmd.GetStop())
	case *pb.OrchestratorCommand_Pause:
		return c.dispatchPause(ctx, cmdID, cmd.GetPause())
	case *pb.OrchestratorCommand_Resume:
		return c.dispatchResume(ctx, cmdID, cmd.GetResume())
	case *pb.OrchestratorCommand_Transfer:
		return c.dispatchTransfer(ctx, cmdID, cmd.GetTransfer())
	case *pb.OrchestratorCommand_TransferConfirmed:
		return c.dispatchTransferConfirmed(ctx, cmdID, cmd.GetTransferConfirmed())
	case *pb.OrchestratorCommand_TransferCancel:
		return c.dispatchTransferCancel(ctx, cmdID, cmd.GetTransferCancel())
	case *pb.OrchestratorCommand_Webhook:
		return c.dispatchWebhook(ctx, cmdID, cmd.GetWebhook())
	case *pb.OrchestratorCommand_Attach:
		return c.dispatchAttach(ctx, cmdID, cmd.GetAttach(), outbound)
	case *pb.OrchestratorCommand_CreateSession:
		return c.dispatchCreate(ctx, cmdID, cmd.GetCreateSession(), outbound)
	case *pb.OrchestratorCommand_WakeChat:
		return c.dispatchWakeChat(ctx, cmdID, cmd.GetWakeChat())
	case *pb.OrchestratorCommand_SwitchAccount:
		return c.dispatchSwitchAccount(ctx, cmdID, cmd.GetSwitchAccount())
	case *pb.OrchestratorCommand_Merge:
		return c.dispatchMerge(ctx, cmdID, cmd.GetMerge(), outbound)
	case *pb.OrchestratorCommand_Archive:
		return c.dispatchArchive(ctx, cmdID, cmd.GetArchive())
	case *pb.OrchestratorCommand_RetrySession:
		return c.dispatchRetrySession(ctx, cmdID, cmd.GetRetrySession())
	case *pb.OrchestratorCommand_UpdateSession:
		return c.dispatchUpdateSession(ctx, cmdID, cmd.GetUpdateSession())
	case *pb.OrchestratorCommand_LinkSessionPr:
		return c.dispatchLinkSessionPR(ctx, cmdID, cmd.GetLinkSessionPr())
	case *pb.OrchestratorCommand_RecordChat:
		return c.dispatchRecordChat(ctx, cmdID, cmd.GetRecordChat())
	case *pb.OrchestratorCommand_DeleteChat:
		return c.dispatchDeleteChat(ctx, cmdID, cmd.GetDeleteChat())
	case *pb.OrchestratorCommand_UpdateChatTitle:
		return c.dispatchUpdateChatTitle(ctx, cmdID, cmd.GetUpdateChatTitle())
	case *pb.OrchestratorCommand_ReportChatStatus:
		return c.dispatchReportChatStatus(ctx, cmdID, cmd.GetReportChatStatus())
	case *pb.OrchestratorCommand_ListRepos:
		return c.dispatchListRepos(ctx, cmdID, outbound)
	case *pb.OrchestratorCommand_ListAgents:
		return c.dispatchListAgents(ctx, cmdID, outbound)
	case *pb.OrchestratorCommand_ListAccounts:
		return c.dispatchListAccounts(ctx, cmdID, cmd.GetListAccounts(), outbound)
	case *pb.OrchestratorCommand_GetRepo:
		return c.dispatchGetRepo(ctx, cmdID, cmd.GetGetRepo(), outbound)
	case *pb.OrchestratorCommand_UpdateRepo:
		return c.dispatchUpdateRepo(ctx, cmdID, cmd.GetUpdateRepo(), outbound)
	case *pb.OrchestratorCommand_RemoveRepo:
		return c.dispatchRemoveRepo(ctx, cmdID, cmd.GetRemoveRepo(), outbound)
	case *pb.OrchestratorCommand_ListRepoPrs:
		return c.dispatchListRepoPRs(ctx, cmdID, cmd.GetListRepoPrs(), outbound)
	case *pb.OrchestratorCommand_ListTrackerIssues:
		return c.dispatchListTrackerIssues(ctx, cmdID, cmd.GetListTrackerIssues(), outbound)
	case *pb.OrchestratorCommand_GetChatTranscript:
		return c.dispatchGetChatTranscript(ctx, cmdID, cmd.GetGetChatTranscript(), outbound)
	case *pb.OrchestratorCommand_SendChatMessage:
		return c.dispatchSendChatMessage(ctx, cmdID, cmd.GetSendChatMessage(), outbound)
	case *pb.OrchestratorCommand_ListCronJobs:
		return c.dispatchListCronJobs(ctx, cmdID, outbound)
	case *pb.OrchestratorCommand_CreateCronJob:
		return c.dispatchCreateCronJob(ctx, cmdID, cmd.GetCreateCronJob(), outbound)
	case *pb.OrchestratorCommand_UpdateCronJob:
		return c.dispatchUpdateCronJob(ctx, cmdID, cmd.GetUpdateCronJob(), outbound)
	case *pb.OrchestratorCommand_DeleteCronJob:
		return c.dispatchDeleteCronJob(ctx, cmdID, cmd.GetDeleteCronJob(), outbound)
	case *pb.OrchestratorCommand_RunCronJobNow:
		return c.dispatchRunCronJobNow(ctx, cmdID, cmd.GetRunCronJobNow(), outbound)
	case *pb.OrchestratorCommand_CreateGithubCallback:
		return c.dispatchCreateGithubCallback(ctx, cmdID, cmd.GetCreateGithubCallback(), outbound)
	case *pb.OrchestratorCommand_ListGithubCallbacks:
		return c.dispatchListGithubCallbacks(ctx, cmdID, cmd.GetListGithubCallbacks(), outbound)
	case *pb.OrchestratorCommand_DeleteGithubCallback:
		return c.dispatchDeleteGithubCallback(ctx, cmdID, cmd.GetDeleteGithubCallback(), outbound)
	case *pb.OrchestratorCommand_AddAccount:
		return c.dispatchAddAccount(ctx, cmdID, cmd.GetAddAccount(), outbound)
	case *pb.OrchestratorCommand_RefreshAccount:
		return c.dispatchRefreshAccount(ctx, cmdID, cmd.GetRefreshAccount(), outbound)
	case *pb.OrchestratorCommand_UpdateAccount:
		return c.dispatchUpdateAccount(ctx, cmdID, cmd.GetUpdateAccount(), outbound)
	case *pb.OrchestratorCommand_RemoveAccount:
		return c.dispatchRemoveAccount(ctx, cmdID, cmd.GetRemoveAccount(), outbound)
	case *pb.OrchestratorCommand_TestAccount:
		return c.dispatchTestAccount(ctx, cmdID, cmd.GetTestAccount(), outbound)
	case *pb.OrchestratorCommand_ListChats:
		return c.dispatchListChats(ctx, cmdID, cmd.GetListChats(), outbound)
	case *pb.OrchestratorCommand_GetSessionStatuses:
		return c.dispatchGetSessionStatuses(ctx, cmdID, cmd.GetGetSessionStatuses(), outbound)
	case *pb.OrchestratorCommand_ListCheckSnapshots:
		return c.dispatchListCheckSnapshots(ctx, cmdID, cmd.GetListCheckSnapshots(), outbound)
	case *pb.OrchestratorCommand_ListPlugins:
		return c.dispatchListPlugins(ctx, cmdID, outbound)
	case *pb.OrchestratorCommand_GetCronJob:
		return c.dispatchGetCronJob(ctx, cmdID, cmd.GetGetCronJob(), outbound)
	case *pb.OrchestratorCommand_RepairDoctor:
		return c.dispatchRepairDoctor(ctx, cmdID, outbound)
	case *pb.OrchestratorCommand_CloseSession:
		return c.dispatchCloseSession(ctx, cmdID, cmd.GetCloseSession())
	case *pb.OrchestratorCommand_ResurrectSession:
		return c.dispatchResurrectSession(ctx, cmdID, cmd.GetResurrectSession())
	case *pb.OrchestratorCommand_RemoveSession:
		return c.dispatchRemoveSession(ctx, cmdID, cmd.GetRemoveSession(), outbound)
	case *pb.OrchestratorCommand_EmptyTrash:
		return c.dispatchEmptyTrash(ctx, cmdID, cmd.GetEmptyTrash(), outbound)
	default:
		// Unknown oneof — forward-compat: log and drop. Do NOT emit a
		// CommandResult; bosso will time out the correlation slot.
		c.logger.Warn().
			Str("command_id", cmdID).
			Msgf("unknown orchestrator command: %T", cmd.GetCmd())
		return nil
	}
}

// dispatchStop routes to the daemon's existing stop path. The handler
// interface keeps this package free of an import cycle with the
// server package — T3.7 wires a concrete adapter.
func (c *StreamClient) dispatchStop(ctx context.Context, cmdID string, req *pb.StopSessionCommand) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	sess, err := c.commandHandler.Stop(ctx, req.GetSessionId())
	if err != nil {
		return commandErr(cmdID, err.Error())
	}
	return commandOK(cmdID, sess)
}

func (c *StreamClient) dispatchPause(ctx context.Context, cmdID string, req *pb.PauseSessionCommand) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	sess, err := c.commandHandler.Pause(ctx, req.GetSessionId())
	if err != nil {
		return commandErr(cmdID, err.Error())
	}
	return commandOK(cmdID, sess)
}

func (c *StreamClient) dispatchResume(ctx context.Context, cmdID string, req *pb.ResumeSessionCommand) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	sess, err := c.commandHandler.Resume(ctx, req.GetSessionId())
	if err != nil {
		return commandErr(cmdID, err.Error())
	}
	return commandOK(cmdID, sess)
}

// dispatchTransfer is the first leg of the coordinated transfer protocol
// (decision #14). Bosso sends this to both the SOURCE (pause + set
// transferring_to) and the TARGET (create with transferring_from + resume).
// The daemon-side session-lifecycle work to satisfy either role lands in a
// follow-up task; for now the dispatcher routes through an optional
// TransferHandler interface when wired (tests stub it) and ACKs a structured
// error when not — matching the webhook/attach pattern so bosso's waiter
// never hangs.
func (c *StreamClient) dispatchTransfer(ctx context.Context, cmdID string, req *pb.TransferSessionCommand) *pb.DaemonEvent {
	if c.transferHandler == nil {
		// No TransferHandler wired: ACK a structured error so bosso's
		// command waiter resolves and triggers the rollback path. This
		// preserves the existing "transfer not implemented" semantics
		// that the T3.6 test locks in, just expressed through the new
		// handler seam rather than hardcoded.
		return commandErr(cmdID, "transfer not yet implemented")
	}
	confirmed, err := c.transferHandler.Transfer(ctx, req)
	if err != nil {
		return commandErr(cmdID, err.Error())
	}
	if confirmed == nil {
		// Source role: no TransferConfirmed payload. ACK Ok:true with
		// no payload so bosso knows the source accepted the pause +
		// emitted the SessionDelta{UPDATED, transferring_to=target}.
		return commandOK(cmdID, nil)
	}
	// Target role: embed the TransferConfirmed payload so bosso can
	// proceed to step 4 (forward TransferConfirmed to source).
	return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{
		Result: &pb.CommandResult{
			CommandId: cmdID,
			Ok:        true,
			Payload:   &pb.CommandResult_TransferConfirmed{TransferConfirmed: confirmed},
		},
	}}
}

// dispatchTransferConfirmed is step 4 of the coordinated transfer protocol
// (decision #14) on the SOURCE daemon. Bosso sends this after the target
// has CONFIRMED resume; the source MUST emit SessionDelta{DELETED} for the
// session so every subscriber sees the hand-off complete. The ACK is
// informational — bosso doesn't block on it — but we still return Ok:true
// so the waiter resolves cleanly.
func (c *StreamClient) dispatchTransferConfirmed(ctx context.Context, cmdID string, req *pb.TransferConfirmed) *pb.DaemonEvent {
	if c.transferHandler == nil {
		// No handler wired: the ACK still succeeds (idempotent no-op)
		// so bosso doesn't trip its waiter. Production wiring lands in
		// the follow-up that implements Transfer.
		return commandOK(cmdID, nil)
	}
	if err := c.transferHandler.Confirmed(ctx, req); err != nil {
		return commandErr(cmdID, err.Error())
	}
	return commandOK(cmdID, nil)
}

// dispatchTransferCancel is the rollback leg of the coordinated transfer
// protocol (decision #14). Sent by bosso to either role when any step
// fails:
//   - Source: clear transferring_to so the session reappears on
//     ListForUser (emits SessionDelta{UPDATED}).
//   - Target: if the session was already created, DELETE it (emits
//     SessionDelta{DELETED}).
//
// The daemon need not know its role — both outcomes boil down to "undo any
// transfer-related state you have for this session_id". Handler no-ops on
// unknown session_id so bosso can safely fan cancel to both legs.
func (c *StreamClient) dispatchTransferCancel(ctx context.Context, cmdID string, req *pb.TransferCancel) *pb.DaemonEvent {
	if c.transferHandler == nil {
		return commandOK(cmdID, nil)
	}
	if err := c.transferHandler.Cancel(ctx, req); err != nil {
		return commandErr(cmdID, err.Error())
	}
	return commandOK(cmdID, nil)
}

// dispatchWebhook forwards the webhook payload to the in-daemon
// dispatcher and wraps the outcome in a WebhookAck event. WebhookAck
// is its own DaemonEvent oneof rather than a CommandResult variant —
// bosso correlates via command_id either way.
func (c *StreamClient) dispatchWebhook(ctx context.Context, cmdID string, ev *pb.WebhookEvent) *pb.DaemonEvent {
	if c.webhooks == nil {
		return webhookAck(cmdID, false, "webhook dispatcher not wired")
	}
	if err := c.webhooks.Dispatch(ctx, ev); err != nil {
		return webhookAck(cmdID, false, err.Error())
	}
	return webhookAck(cmdID, true, "")
}

// dispatchAttach kicks off the streaming reader and returns an
// immediate CommandResult{Ok:true}. The reader goroutine emits
// SessionAttachChunk events onto outbound until the session ends or
// ctx is cancelled. Each chunk is already correlated via command_id
// by the attacher, so the subscriber on the other side can dedicate a
// per-attach subscriber slot without reindexing on every frame.
//
// Handshake: caller receives CommandResult{Ok:true} synchronously so
// it knows the attach is live; subsequent AttachChunk events flow
// asynchronously on the same stream. A final SessionEnded chunk is
// the attacher's responsibility.
func (c *StreamClient) dispatchAttach(
	ctx context.Context,
	cmdID string,
	req *pb.AttachSessionCommand,
	outbound chan<- *pb.DaemonEvent,
) *pb.DaemonEvent {
	if c.attacher == nil {
		return commandErr(cmdID, "attacher not wired")
	}
	sessionID := req.GetSessionId()
	if sessionID == "" {
		return commandErr(cmdID, "attach: session_id required")
	}

	ch, err := c.attacher.Attach(ctx, sessionID, cmdID)
	if err != nil {
		return commandErr(cmdID, fmt.Sprintf("attach: %v", err))
	}

	// Run the chunk pump in its own goroutine so the command reader
	// can keep processing subsequent commands while this attach
	// streams.
	safego.Go(c.logger, func() {
		for chunk := range ch {
			select {
			case <-ctx.Done():
				return
			case outbound <- &pb.DaemonEvent{Event: &pb.DaemonEvent_AttachChunk{AttachChunk: chunk}}:
			}
		}
	})

	// Immediate ack — routing is active.
	return commandOK(cmdID, nil)
}

// dispatchCreate kicks off a streaming session creation and returns an
// immediate CommandResult{Ok:true} handshake. The creator goroutine emits
// SessionCreateChunk events (live setup output, then a terminal created/error)
// onto outbound until creation finishes or ctx is cancelled. Each chunk is
// already correlated via command_id by the creator. Mirrors dispatchAttach.
func (c *StreamClient) dispatchCreate(
	ctx context.Context,
	cmdID string,
	req *pb.CreateSessionCommand,
	outbound chan<- *pb.DaemonEvent,
) *pb.DaemonEvent {
	if c.creator == nil {
		return commandErr(cmdID, "creator not wired")
	}

	ch, err := c.creator.Create(ctx, req, cmdID)
	if err != nil {
		return commandErr(cmdID, fmt.Sprintf("create: %v", err))
	}

	// Pump chunks in a background goroutine so the command reader can keep
	// processing subsequent commands while this creation streams.
	safego.Go(c.logger, func() {
		for chunk := range ch {
			select {
			case <-ctx.Done():
				return
			case outbound <- &pb.DaemonEvent{Event: &pb.DaemonEvent_CreateChunk{CreateChunk: chunk}}:
			}
		}
	})

	// Immediate ack — creation is in flight.
	return commandOK(cmdID, nil)
}

// dispatchWakeChat routes the WakeChatCommand to the configured handler
// and packages the (outcome, tmuxName) into a CommandResult{WakeChatResult}
// payload. Failures attach the handler-classified ErrorCode so bosso can
// map back to the right ConnectRPC code (CodeNotFound, etc.) without
// parsing the human-readable `error` string.
func (c *StreamClient) dispatchWakeChat(ctx context.Context, cmdID string, req *pb.WakeChatCommand) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	outcome, tmuxName, reason, errorCode, err := c.commandHandler.WakeChat(ctx, req.GetAgentSessionId(), req.GetForceFresh())
	if err != nil {
		return commandErrCode(cmdID, err.Error(), errorCode)
	}
	return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{
		Result: &pb.CommandResult{
			CommandId: cmdID,
			Ok:        true,
			Payload: &pb.CommandResult_WakeChat{
				WakeChat: &pb.WakeChatResult{
					Outcome:         outcome,
					TmuxSessionName: tmuxName,
					Reason:          reason,
				},
			},
		},
	}}
}

// dispatchSwitchAccount routes the SwitchAccountCommand to the configured
// handler and packages the (resumed, targetLabel, noticeText) into a
// CommandResult{SwitchAccountResult} payload. Failures attach the
// handler-classified ErrorCode so bosso can map back to the right ConnectRPC
// code (CodeFailedPrecondition, CodeNotFound, …) without parsing the
// human-readable `error` string. Mirrors dispatchWakeChat.
func (c *StreamClient) dispatchSwitchAccount(ctx context.Context, cmdID string, req *pb.SwitchAccountCommand) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	resumed, targetLabel, noticeText, errorCode, err := c.commandHandler.SwitchAccount(ctx, req.GetSessionId(), req.GetAgentSessionId(), req.GetAccountId(), req.GetForce())
	if err != nil {
		return commandErrCode(cmdID, err.Error(), errorCode)
	}
	return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{
		Result: &pb.CommandResult{
			CommandId: cmdID,
			Ok:        true,
			Payload: &pb.CommandResult_SwitchAccount{
				SwitchAccount: &pb.SwitchAccountResult{
					Resumed:     resumed,
					TargetLabel: targetLabel,
					NoticeText:  noticeText,
				},
			},
		},
	}}
}

// dispatchMerge routes a MergeSessionCommand to the handler. Dispatched
// asynchronously (BOS-439): a merge can queue behind another merge on the same
// repo inside Server.MergeSession, and the single-threaded command reader must
// keep draining other commands rather than wedge behind that wait. On success
// it emits commandOK(sess); on failure commandErrCode — identical payload/codes
// to the old synchronous path, delivered via outbound (mirrors
// dispatchRemoveSession).
func (c *StreamClient) dispatchMerge(ctx context.Context, cmdID string, req *pb.MergeSessionCommand, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
		sess, err := c.commandHandler.MergeSession(ctx, req.GetSessionId())
		if err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return commandOK(cmdID, sess)
	})
}

func (c *StreamClient) dispatchArchive(ctx context.Context, cmdID string, req *pb.ArchiveSessionCommand) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	sess, err := c.commandHandler.ArchiveSession(ctx, req.GetSessionId())
	if err != nil {
		return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
	}
	return commandOK(cmdID, sess)
}

// dispatchCloseSession routes a CloseSessionCommand to the handler and replies
// with the updated Session (session = 4). Mirrors dispatchArchive.
func (c *StreamClient) dispatchCloseSession(ctx context.Context, cmdID string, req *pb.CloseSessionCommand) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	sess, err := c.commandHandler.CloseSession(ctx, req.GetSessionId())
	if err != nil {
		return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
	}
	return commandOK(cmdID, sess)
}

// dispatchResurrectSession routes a ResurrectSessionCommand to the handler and
// replies with the updated Session (session = 4). Mirrors dispatchArchive.
func (c *StreamClient) dispatchResurrectSession(ctx context.Context, cmdID string, req *pb.ResurrectSessionCommand) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	sess, err := c.commandHandler.ResurrectSession(ctx, req.GetSessionId())
	if err != nil {
		return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
	}
	return commandOK(cmdID, sess)
}

// dispatchRemoveSession routes a RemoveSessionCommand to the handler and replies
// with a success CommandResult carrying no payload. Dispatched asynchronously:
// removing a session tears down its worktree (filesystem-bound), which must not
// block the command reader (mirrors dispatchRemoveRepo).
func (c *StreamClient) dispatchRemoveSession(ctx context.Context, cmdID string, req *pb.RemoveSessionCommand, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
		if err := c.commandHandler.RemoveSession(ctx, req.GetSessionId()); err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return commandOK(cmdID, nil)
	})
}

// dispatchEmptyTrash routes a (session-less) EmptyTrashCommand to the handler and
// wraps the deleted-session count in a CommandResult{empty_trash}. Dispatched
// asynchronously: emptying the trash removes archived worktrees (filesystem-bound)
// and must not block the command reader.
func (c *StreamClient) dispatchEmptyTrash(ctx context.Context, cmdID string, req *pb.EmptyTrashCommand, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
		count, err := c.commandHandler.EmptyTrash(ctx, req.GetOlderThan())
		if err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{Result: &pb.CommandResult{
			CommandId: cmdID, Ok: true,
			Payload: &pb.CommandResult_EmptyTrash{EmptyTrash: &pb.EmptyTrashResult{DeletedCount: count}},
		}}}
	})
}

// dispatchRetrySession / dispatchUpdateSession / dispatchLinkSessionPR are
// synchronous session-scoped mutations (mirroring dispatchArchive):
// they call the handler, echo the updated Session on success, and attach a typed
// error code on failure so bosso maps it back to the right ConnectRPC code.
func (c *StreamClient) dispatchRetrySession(ctx context.Context, cmdID string, req *pb.RetrySessionCommand) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	sess, err := c.commandHandler.RetrySession(ctx, req.GetSessionId())
	if err != nil {
		return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
	}
	return commandOK(cmdID, sess)
}

func (c *StreamClient) dispatchUpdateSession(ctx context.Context, cmdID string, req *pb.UpdateSessionCommand) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	sess, err := c.commandHandler.UpdateSession(ctx, req)
	if err != nil {
		return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
	}
	return commandOK(cmdID, sess)
}

func (c *StreamClient) dispatchLinkSessionPR(ctx context.Context, cmdID string, req *pb.LinkSessionPRCommand) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	sess, err := c.commandHandler.LinkSessionPR(ctx, req.GetSessionId(), req.GetPr())
	if err != nil {
		return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
	}
	return commandOK(cmdID, sess)
}

func (c *StreamClient) dispatchRecordChat(ctx context.Context, cmdID string, req *pb.RecordChatCommand) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	chat, err := c.commandHandler.RecordChat(ctx, req.GetSessionId(), req.GetAgentSessionId(), req.GetTitle(), req.GetResume(), req.GetAgentName())
	if err != nil {
		return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
	}
	return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{
		Result: &pb.CommandResult{
			CommandId: cmdID,
			Ok:        true,
			Payload:   &pb.CommandResult_RecordChat{RecordChat: chat},
		},
	}}
}

func (c *StreamClient) dispatchDeleteChat(ctx context.Context, cmdID string, req *pb.DeleteChatCommand) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	if err := c.commandHandler.DeleteChat(ctx, req.GetSessionId(), req.GetAgentSessionId()); err != nil {
		return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
	}
	return commandOK(cmdID, nil)
}

// dispatchUpdateChatTitle / dispatchReportChatStatus are synchronous chat-scoped
// mutations returning no payload (commandOK(cmdID, nil)), mirroring
// dispatchDeleteChat.
func (c *StreamClient) dispatchUpdateChatTitle(ctx context.Context, cmdID string, req *pb.UpdateChatTitleCommand) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	if err := c.commandHandler.UpdateChatTitle(ctx, req.GetAgentSessionId(), req.GetTitle()); err != nil {
		return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
	}
	return commandOK(cmdID, nil)
}

func (c *StreamClient) dispatchReportChatStatus(ctx context.Context, cmdID string, req *pb.ReportChatStatusCommand) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	if err := c.commandHandler.ReportChatStatus(ctx, req.GetReports()); err != nil {
		return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
	}
	return commandOK(cmdID, nil)
}

// classifyCommandError maps a connect-coded daemon error to the typed
// CommandResult_ErrorCode so bosso can translate it back to the right
// ConnectRPC code without parsing the human-readable message. The adapters
// wrap the daemon's *connect.Error with %w, so connect.CodeOf recovers the
// original code. Codes the reverse-stream protocol doesn't model collapse to
// UNSPECIFIED, which bosso treats as CodeAborted. Mirrors the typed-code path
// dispatchWakeChat already uses.
func classifyCommandError(err error) pb.CommandResult_ErrorCode {
	switch connect.CodeOf(err) {
	case connect.CodeNotFound:
		return pb.CommandResult_ERROR_CODE_NOT_FOUND
	case connect.CodeFailedPrecondition:
		return pb.CommandResult_ERROR_CODE_FAILED_PRECONDITION
	default:
		return pb.CommandResult_ERROR_CODE_UNSPECIFIED
	}
}

// runAsyncCommand executes a blocking, network-bound command handler in a
// background goroutine so the single-threaded command reader (runCommandReader)
// keeps draining subsequent commands instead of wedging behind one slow call.
// The handler's CommandResult is emitted on outbound when it completes, or
// dropped if ctx is cancelled first (bosso has already timed out its
// correlation slot). Mirrors dispatchAttach/dispatchCreate, which spawn
// goroutines for the same reason. Returns nil: there is no synchronous result.
//
// build is invoked off the reader goroutine and must produce the DaemonEvent to
// send (a success result or, via commandErrCode, a typed failure).
func (c *StreamClient) runAsyncCommand(
	ctx context.Context,
	outbound chan<- *pb.DaemonEvent,
	build func() *pb.DaemonEvent,
) *pb.DaemonEvent {
	safego.Go(c.logger, func() {
		ev := build()
		if ev == nil {
			return
		}
		select {
		case <-ctx.Done():
		case outbound <- ev:
		}
	})
	return nil
}

// dispatchListRepos routes a (non-session-scoped) ListReposCommand to the
// handler and wraps the daemon's full Repo set in a CommandResult{list_repos}.
// Dispatched asynchronously so it can't block the command reader.
func (c *StreamClient) dispatchListRepos(ctx context.Context, cmdID string, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
		out, err := c.commandHandler.ListRepos(ctx)
		if err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{
			Result: &pb.CommandResult{
				CommandId: cmdID,
				Ok:        true,
				Payload:   &pb.CommandResult_ListRepos{ListRepos: out},
			},
		}}
	})
}

// dispatchListAgents routes a (non-session-scoped) ListAgentsCommand to the
// handler and wraps the daemon's installed agents in a
// CommandResult{list_agents}. Dispatched asynchronously (plugin GetInfo calls
// can be slow) so it can't block the command reader.
func (c *StreamClient) dispatchListAgents(ctx context.Context, cmdID string, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
		out, err := c.commandHandler.ListAgents(ctx)
		if err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{
			Result: &pb.CommandResult{
				CommandId: cmdID,
				Ok:        true,
				Payload:   &pb.CommandResult_ListAgents{ListAgents: out},
			},
		}}
	})
}

// dispatchListAccounts routes a (session-scoped by bosso) ListAccountsCommand to
// the handler and wraps the daemon's rotation accounts in a
// CommandResult{list_accounts}. Accounts are metadata only — never credentials.
// Dispatched asynchronously so it can't block the command reader (mirrors
// dispatchListAgents).
func (c *StreamClient) dispatchListAccounts(ctx context.Context, cmdID string, req *pb.ListAccountsCommand, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
		out, err := c.commandHandler.ListAccounts(ctx, req.GetProvider())
		if err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{
			Result: &pb.CommandResult{
				CommandId: cmdID,
				Ok:        true,
				Payload:   &pb.CommandResult_ListAccounts{ListAccounts: out},
			},
		}}
	})
}

func (c *StreamClient) dispatchGetRepo(ctx context.Context, cmdID string, req *pb.GetRepoCommand, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
		out, err := c.commandHandler.GetRepo(ctx, req.GetRepoId())
		if err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{Result: &pb.CommandResult{
			CommandId: cmdID, Ok: true,
			Payload: &pb.CommandResult_GetRepo{GetRepo: out},
		}}}
	})
}

func (c *StreamClient) dispatchUpdateRepo(ctx context.Context, cmdID string, req *pb.UpdateRepoCommand, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
		out, err := c.commandHandler.UpdateRepo(ctx, req)
		if err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{Result: &pb.CommandResult{
			CommandId: cmdID, Ok: true,
			Payload: &pb.CommandResult_UpdateRepo{UpdateRepo: out},
		}}}
	})
}

func (c *StreamClient) dispatchRemoveRepo(ctx context.Context, cmdID string, req *pb.RemoveRepoCommand, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
		if err := c.commandHandler.RemoveRepo(ctx, req.GetRepoId()); err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return commandOK(cmdID, nil)
	})
}

// dispatchListRepoPRs routes a ListRepoPRsCommand to the handler and wraps
// the repo's open PRs in a CommandResult{list_repo_prs}. Dispatched
// asynchronously: the handler hits the GitHub API, which must not block the
// command reader.
func (c *StreamClient) dispatchListRepoPRs(ctx context.Context, cmdID string, req *pb.ListRepoPRsCommand, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
		out, err := c.commandHandler.ListRepoPRs(ctx, req.GetRepoId())
		if err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{Result: &pb.CommandResult{
			CommandId: cmdID, Ok: true,
			Payload: &pb.CommandResult_ListRepoPrs{ListRepoPrs: out},
		}}}
	})
}

// dispatchListTrackerIssues routes a ListTrackerIssuesCommand to the handler
// and wraps the tracker issues in a CommandResult{list_tracker_issues}.
// Dispatched asynchronously: the handler hits the Linear/Sentry API via a task
// source plugin, which must not block the command reader. A slow tracker search
// blocking the reader is what wedged the new-session wizard.
func (c *StreamClient) dispatchListTrackerIssues(ctx context.Context, cmdID string, req *pb.ListTrackerIssuesCommand, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
		out, err := c.commandHandler.ListTrackerIssues(ctx, req.GetRepoId(), req.GetQuery(), req.Source)
		if err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{Result: &pb.CommandResult{
			CommandId: cmdID, Ok: true,
			Payload: &pb.CommandResult_ListTrackerIssues{ListTrackerIssues: out},
		}}}
	})
}

// dispatchGetChatTranscript routes a GetChatTranscriptCommand to the handler and
// wraps the transcript in a CommandResult{get_chat_transcript}. Dispatched
// asynchronously: reading a transcript is network/tmux-bound and must not block
// the command reader (mirrors dispatchListRepos).
func (c *StreamClient) dispatchGetChatTranscript(ctx context.Context, cmdID string, req *pb.GetChatTranscriptCommand, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
		out, err := c.commandHandler.GetChatTranscript(ctx, req.GetSessionId(), req.GetAgentSessionId(), req.GetMaxMessages())
		if err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{Result: &pb.CommandResult{
			CommandId: cmdID, Ok: true,
			Payload: &pb.CommandResult_GetChatTranscript{GetChatTranscript: out},
		}}}
	})
}

// dispatchListChats routes a ListChatsCommand to the handler and wraps the
// session's chats in a CommandResult{list_chats}. session_id scopes the read
// for authz. Dispatched asynchronously (mirrors dispatchGetChatTranscript).
func (c *StreamClient) dispatchListChats(ctx context.Context, cmdID string, req *pb.ListChatsCommand, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
		out, err := c.commandHandler.ListChats(ctx, req.GetSessionId())
		if err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{Result: &pb.CommandResult{
			CommandId: cmdID, Ok: true,
			Payload: &pb.CommandResult_ListChats{ListChats: out},
		}}}
	})
}

// dispatchGetSessionStatuses routes a GetSessionStatusesCommand to the handler
// and wraps the sessions' aggregate statuses in a
// CommandResult{get_session_statuses}. Dispatched asynchronously.
func (c *StreamClient) dispatchGetSessionStatuses(ctx context.Context, cmdID string, req *pb.GetSessionStatusesCommand, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
		out, err := c.commandHandler.GetSessionStatuses(ctx, req.GetSessionIds())
		if err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{Result: &pb.CommandResult{
			CommandId: cmdID, Ok: true,
			Payload: &pb.CommandResult_GetSessionStatuses{GetSessionStatuses: out},
		}}}
	})
}

// dispatchListCheckSnapshots routes a ListCheckSnapshotsCommand to the handler
// and wraps the session's recent CI snapshots in a
// CommandResult{list_check_snapshots}. session_id scopes the read for authz.
// Dispatched asynchronously.
func (c *StreamClient) dispatchListCheckSnapshots(ctx context.Context, cmdID string, req *pb.ListCheckSnapshotsCommand, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
		out, err := c.commandHandler.ListCheckSnapshots(ctx, req.GetSessionId(), req.GetLimit())
		if err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{Result: &pb.CommandResult{
			CommandId: cmdID, Ok: true,
			Payload: &pb.CommandResult_ListCheckSnapshots{ListCheckSnapshots: out},
		}}}
	})
}

// dispatchListPlugins routes a (session-less) ListPluginsCommand to the handler
// and wraps the daemon's configured plugins in a CommandResult{list_plugins}.
// Dispatched asynchronously (mirrors dispatchListAgents).
func (c *StreamClient) dispatchListPlugins(ctx context.Context, cmdID string, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
		out, err := c.commandHandler.ListPlugins(ctx)
		if err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{Result: &pb.CommandResult{
			CommandId: cmdID, Ok: true,
			Payload: &pb.CommandResult_ListPlugins{ListPlugins: out},
		}}}
	})
}

// dispatchGetCronJob routes a GetCronJobCommand to the handler and wraps the
// cron job in a CommandResult{get_cron_job}. Dispatched asynchronously.
func (c *StreamClient) dispatchGetCronJob(ctx context.Context, cmdID string, req *pb.GetCronJobCommand, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
		out, err := c.commandHandler.GetCronJob(ctx, req.GetId())
		if err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{Result: &pb.CommandResult{
			CommandId: cmdID, Ok: true,
			Payload: &pb.CommandResult_GetCronJob{GetCronJob: out},
		}}}
	})
}

// dispatchRepairDoctor routes a (session-less) RepairDoctorCommand to the
// handler and wraps the diagnostics report in a CommandResult{repair_doctor}.
// Dispatched asynchronously.
func (c *StreamClient) dispatchRepairDoctor(ctx context.Context, cmdID string, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
		out, err := c.commandHandler.RepairDoctor(ctx)
		if err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{Result: &pb.CommandResult{
			CommandId: cmdID, Ok: true,
			Payload: &pb.CommandResult_RepairDoctor{RepairDoctor: out},
		}}}
	})
}

// dispatchSendChatMessage routes a SendChatMessageCommand to the handler and
// wraps the delivery outcome in a CommandResult{send_chat_message}. Dispatched
// asynchronously: delivering a message (and optionally waking the chat) is
// network/tmux-bound and must not block the command reader.
func (c *StreamClient) dispatchSendChatMessage(ctx context.Context, cmdID string, req *pb.SendChatMessageCommand, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
		out, err := c.commandHandler.SendChatMessage(ctx, req.GetAgentSessionId(), req.GetMessage(), req.GetWakeIfAsleep(), req.GetSubmit())
		if err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{Result: &pb.CommandResult{
			CommandId: cmdID, Ok: true,
			Payload: &pb.CommandResult_SendChatMessage{SendChatMessage: out},
		}}}
	})
}

// dispatchListCronJobs routes a ListCronJobsCommand to the handler and wraps the
// daemon's cron jobs in a CommandResult{list_cron_jobs}. Dispatched
// asynchronously: cron store reads must not block the command reader (mirrors
// dispatchListRepos).
func (c *StreamClient) dispatchListCronJobs(ctx context.Context, cmdID string, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
		out, err := c.commandHandler.ListCronJobs(ctx)
		if err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{Result: &pb.CommandResult{
			CommandId: cmdID, Ok: true,
			Payload: &pb.CommandResult_ListCronJobs{ListCronJobs: out},
		}}}
	})
}

// dispatchCreateCronJob routes a CreateCronJobCommand to the handler and wraps
// the created job in a CommandResult{create_cron_job}. Validation errors from the
// daemon's cron handler surface via CommandResult.error (their connect code
// collapses to UNSPECIFIED, which bosso treats as Aborted). Dispatched async:
// creating a job touches the store and scheduler.
func (c *StreamClient) dispatchCreateCronJob(ctx context.Context, cmdID string, req *pb.CreateCronJobCommand, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
		out, err := c.commandHandler.CreateCronJob(ctx, req)
		if err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{Result: &pb.CommandResult{
			CommandId: cmdID, Ok: true,
			Payload: &pb.CommandResult_CreateCronJob{CreateCronJob: out},
		}}}
	})
}

// dispatchUpdateCronJob routes an UpdateCronJobCommand to the handler and wraps
// the updated job in a CommandResult{update_cron_job}. Dispatched async (store +
// scheduler resync).
func (c *StreamClient) dispatchUpdateCronJob(ctx context.Context, cmdID string, req *pb.UpdateCronJobCommand, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
		out, err := c.commandHandler.UpdateCronJob(ctx, req)
		if err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{Result: &pb.CommandResult{
			CommandId: cmdID, Ok: true,
			Payload: &pb.CommandResult_UpdateCronJob{UpdateCronJob: out},
		}}}
	})
}

// dispatchDeleteCronJob routes a DeleteCronJobCommand to the handler and replies
// with a success CommandResult carrying no payload (mirrors dispatchRemoveRepo).
// Dispatched async.
func (c *StreamClient) dispatchDeleteCronJob(ctx context.Context, cmdID string, req *pb.DeleteCronJobCommand, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
		if err := c.commandHandler.DeleteCronJob(ctx, req.GetId()); err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return commandOK(cmdID, nil)
	})
}

// dispatchRunCronJobNow routes a RunCronJobNowCommand to the handler and wraps
// the spawned session (or skipped reason) in a CommandResult{run_cron_job_now}.
// Dispatched async: firing a job spawns a session, which must not block the
// command reader.
func (c *StreamClient) dispatchRunCronJobNow(ctx context.Context, cmdID string, req *pb.RunCronJobNowCommand, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
		out, err := c.commandHandler.RunCronJobNow(ctx, req.GetId())
		if err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{Result: &pb.CommandResult{
			CommandId: cmdID, Ok: true,
			Payload: &pb.CommandResult_RunCronJobNow{RunCronJobNow: out},
		}}}
	})
}

// dispatchCreateGithubCallback routes a CreateGithubCallbackCommand to the
// handler and wraps the created callback in a
// CommandResult{create_github_callback}. Validation errors from the daemon
// surface via CommandResult.error. Dispatched async: creating a callback touches
// the store. The Message field is a secret and is never logged.
func (c *StreamClient) dispatchCreateGithubCallback(ctx context.Context, cmdID string, cmd *pb.CreateGithubCallbackCommand, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
		out, err := c.commandHandler.CreateGithubCallback(ctx, cmd)
		if err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{Result: &pb.CommandResult{
			CommandId: cmdID, Ok: true,
			Payload: &pb.CommandResult_CreateGithubCallback{CreateGithubCallback: out},
		}}}
	})
}

// dispatchListGithubCallbacks routes a ListGithubCallbacksCommand to the handler
// and wraps the matching callbacks in a CommandResult{list_github_callbacks}.
// Dispatched async: callback store reads must not block the command reader.
func (c *StreamClient) dispatchListGithubCallbacks(ctx context.Context, cmdID string, cmd *pb.ListGithubCallbacksCommand, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
		out, err := c.commandHandler.ListGithubCallbacks(ctx, cmd)
		if err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{Result: &pb.CommandResult{
			CommandId: cmdID, Ok: true,
			Payload: &pb.CommandResult_ListGithubCallbacks{ListGithubCallbacks: out},
		}}}
	})
}

// dispatchDeleteGithubCallback routes a DeleteGithubCallbackCommand to the
// handler and replies with a success CommandResult carrying no payload (mirrors
// dispatchDeleteCronJob). Dispatched async.
func (c *StreamClient) dispatchDeleteGithubCallback(ctx context.Context, cmdID string, cmd *pb.DeleteGithubCallbackCommand, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
		if err := c.commandHandler.DeleteGithubCallback(ctx, cmd.GetId()); err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return commandOK(cmdID, nil)
	})
}

// dispatchAddAccount routes an AddAccountCommand to the handler and wraps the
// created account in a CommandResult{add_account} (metadata only — the inbound
// credential is never echoed). Dispatched async: registering an account touches
// the store and keyring.
func (c *StreamClient) dispatchAddAccount(ctx context.Context, cmdID string, req *pb.AddAccountCommand, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
		out, err := c.commandHandler.AddAccount(ctx, req)
		if err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{Result: &pb.CommandResult{
			CommandId: cmdID, Ok: true,
			Payload: &pb.CommandResult_AddAccount{AddAccount: out},
		}}}
	})
}

// dispatchRefreshAccount routes a RefreshAccountCommand to the handler and wraps
// the updated account (+ optional smoke-test outcome) in a
// CommandResult{refresh_account}. Dispatched async: saving the credential and
// running the optional provider test are store/keyring/network-bound.
func (c *StreamClient) dispatchRefreshAccount(ctx context.Context, cmdID string, req *pb.RefreshAccountCommand, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
		out, err := c.commandHandler.RefreshAccount(ctx, req)
		if err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{Result: &pb.CommandResult{
			CommandId: cmdID, Ok: true,
			Payload: &pb.CommandResult_RefreshAccount{RefreshAccount: out},
		}}}
	})
}

// dispatchUpdateAccount routes an UpdateAccountCommand to the handler and wraps
// the updated account in a CommandResult{update_account}. Only the optional
// fields the command sets are applied by the daemon handler. Dispatched async
// (store write).
func (c *StreamClient) dispatchUpdateAccount(ctx context.Context, cmdID string, req *pb.UpdateAccountCommand, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
		out, err := c.commandHandler.UpdateAccount(ctx, req)
		if err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{Result: &pb.CommandResult{
			CommandId: cmdID, Ok: true,
			Payload: &pb.CommandResult_UpdateAccount{UpdateAccount: out},
		}}}
	})
}

// dispatchRemoveAccount routes a RemoveAccountCommand to the handler and replies
// with a success CommandResult carrying no payload (mirrors dispatchRemoveRepo).
// Dispatched async: removal touches the store and keyring.
func (c *StreamClient) dispatchRemoveAccount(ctx context.Context, cmdID string, req *pb.RemoveAccountCommand, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
		if err := c.commandHandler.RemoveAccount(ctx, req.GetId()); err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return commandOK(cmdID, nil)
	})
}

// dispatchTestAccount routes a TestAccountCommand to the handler and wraps the
// updated account (+ validation outcome) in a CommandResult{test_account}.
// Dispatched async: provider verification is network-bound.
func (c *StreamClient) dispatchTestAccount(ctx context.Context, cmdID string, req *pb.TestAccountCommand, outbound chan<- *pb.DaemonEvent) *pb.DaemonEvent {
	if c.commandHandler == nil {
		return commandErr(cmdID, "command handler not wired")
	}
	return c.runAsyncCommand(ctx, outbound, func() *pb.DaemonEvent {
		out, err := c.commandHandler.TestAccount(ctx, req)
		if err != nil {
			return commandErrCode(cmdID, err.Error(), classifyCommandError(err))
		}
		return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{Result: &pb.CommandResult{
			CommandId: cmdID, Ok: true,
			Payload: &pb.CommandResult_TestAccount{TestAccount: out},
		}}}
	})
}

// commandOK builds a success CommandResult. session may be nil for
// commands whose response doesn't include a session payload (e.g.
// attach start).
func commandOK(cmdID string, session *pb.Session) *pb.DaemonEvent {
	result := &pb.CommandResult{CommandId: cmdID, Ok: true}
	if session != nil {
		result.Payload = &pb.CommandResult_Session{Session: session}
	}
	return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{Result: result}}
}

// commandErr wraps an error message into a failed CommandResult with
// ERROR_CODE_UNSPECIFIED. Use commandErrCode when the failure mode is
// known (e.g. NOT_FOUND) so bosso can map back to the right ConnectRPC
// code without parsing the human-readable message.
func commandErr(cmdID, msg string) *pb.DaemonEvent {
	return commandErrCode(cmdID, msg, pb.CommandResult_ERROR_CODE_UNSPECIFIED)
}

// commandErrCode is commandErr with a typed error_code attached.
func commandErrCode(cmdID, msg string, code pb.CommandResult_ErrorCode) *pb.DaemonEvent {
	return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{
		Result: &pb.CommandResult{CommandId: cmdID, Ok: false, Error: msg, ErrorCode: code},
	}}
}

// webhookAck wraps a webhook response. Distinct from CommandResult so
// the orchestrator's webhook correlator doesn't need to unpack the
// payload oneof.
func webhookAck(cmdID string, ok bool, errMsg string) *pb.DaemonEvent {
	return &pb.DaemonEvent{Event: &pb.DaemonEvent_Ack{
		Ack: &pb.WebhookAck{CommandId: cmdID, Ok: ok, Error: errMsg},
	}}
}
