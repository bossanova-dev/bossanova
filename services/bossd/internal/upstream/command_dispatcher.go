package upstream

import (
	"context"
	"fmt"

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

// commandErr wraps an error message into a failed CommandResult. Kept
// as a helper so every dispatcher path produces the same shape.
func commandErr(cmdID, msg string) *pb.DaemonEvent {
	return &pb.DaemonEvent{Event: &pb.DaemonEvent_Result{
		Result: &pb.CommandResult{CommandId: cmdID, Ok: false, Error: msg},
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
