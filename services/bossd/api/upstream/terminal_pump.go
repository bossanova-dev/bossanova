package upstream

import (
	"context"
	"fmt"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// runAttachPump fans Output() and Exited() onto the per-stream outbound
// channel. Finished attaches retain output authorization until the writer
// sends their queued exit; control lookup excludes them. The done channel
// (closed by safego.Go after this function returns) is what
// closeAllAttaches joins on for shutdown.
func (c *TerminalStreamClient) runAttachPump(ctx context.Context, state *activeAttach, outbound chan<- *pb.TerminalServerMessage) {
	exitSent := false
	var revoked, failed <-chan struct{}
	if state.seal != nil {
		revoked = state.seal.revoked
		failed = state.seal.failed
	}
	defer func() { c.finishAttachPump(ctx, state, outbound, exitSent) }()

	if ctx.Err() != nil {
		return
	}
	if !c.sendAttachWakeNotice(ctx, state, outbound) {
		return
	}
	output := state.attach.Output()
	exited := state.attach.Exited()
	policyRecheck := c.clock.After(terminalCloudControlRecheckInterval)

	for {
		select {
		case <-ctx.Done():
			return
		case <-revoked:
			_ = state.attach.Close()
			state.seal.destroy()
			c.sendExited(ctx, outbound, state.id, viewerNotApproved)
			return
		case <-failed:
			_ = state.attach.Close()
			state.seal.destroy()
			c.sendExited(ctx, outbound, state.id, state.seal.failureReason())
			return
		case <-state.outputStopped:
			return
		case <-policyRecheck:
			if c.recheckIdleAttach(ctx, state) {
				return
			}
			policyRecheck = c.clock.After(terminalCloudControlRecheckInterval)
		case chunk, ok := <-output:
			if c.attachOutputRefused(ctx, state) {
				return
			}
			if !ok {
				exitSent = c.forwardAttachExit(ctx, state, outbound, exited)
				return
			}
			if chunk.GetLost() {
				// Fire and forget — RefreshClient is best-effort. A
				// failure here just means the outer terminals don't
				// repaint immediately; the next user keypress will
				// likely trigger a redraw.
				c.fireRefreshClient(ctx, state.sessionName)
			}
			if !c.sendAttachData(ctx, state, outbound, chunk) {
				return
			}
		}
	}
}

func (c *TerminalStreamClient) sendAttachData(ctx context.Context, state *activeAttach, outbound chan<- *pb.TerminalServerMessage, chunk *pb.TerminalDataChunk) bool {
	if state.seal != nil {
		if err := state.seal.seal(chunk); err != nil {
			_ = state.attach.Close()
			state.seal.destroy()
			c.sendExited(ctx, outbound, state.id, state.seal.failureReason())
			return false
		}
	}
	msg := &pb.TerminalServerMessage{Msg: &pb.TerminalServerMessage_Data{Data: chunk}}
	if state.seal == nil {
		c.sendServerMessage(ctx, outbound, msg)
		return true
	}
	select {
	case outbound <- msg:
		return true
	case <-ctx.Done():
		return false
	case <-state.seal.revoked:
	case <-state.seal.failed:
	}
	_ = state.attach.Close()
	state.seal.destroy()
	c.sendExited(ctx, outbound, state.id, state.seal.failureReason())
	return false
}

func (c *TerminalStreamClient) sendAttachWakeNotice(ctx context.Context, state *activeAttach, outbound chan<- *pb.TerminalServerMessage) bool {
	if state.wakeNotice == "" {
		return true
	}
	if c.attachOutputRefused(ctx, state) {
		return false
	}
	c.logger.Info().Str("attach_id", state.id).Str("reason", state.wakeNotice).Msg("terminal stream: attach woke chat with a fresh pane")
	line := fmt.Sprintf("\r\n[boss] previous conversation could not be resumed (%s); starting a fresh agent.\r\n", sanitizeWakeReason(state.wakeNotice))
	return c.sendAttachData(ctx, state, outbound, &pb.TerminalDataChunk{AttachId: state.id, Data: []byte(line)})
}

func (c *TerminalStreamClient) finishAttachPump(ctx context.Context, state *activeAttach, outbound chan<- *pb.TerminalServerMessage, exitSent bool) {
	draining := exitSent && ctx.Err() == nil && !state.outputRefused.Load()
	if state.seal != nil && !draining {
		defer state.seal.destroy()
	}
	if state.outputRefused.Load() {
		c.logger.Info().Str("attach_id", state.id).Msg("cloud terminal output refused")
		// Close joins the PTY goroutines. The driver can suppress its own
		// exit during Close, so send the fixed policy exit without waiting.
		_ = state.attach.Close()
		if !exitSent {
			c.sendExited(ctx, outbound, state.id, state.policyExitReason())
		}
	}
	c.mu.Lock()
	// Only remove if still ours — handleClose may have already swapped
	// us out and closed the attach.
	if existing, ok := c.attaches[state.id]; ok && existing == state && !draining {
		delete(c.attaches, state.id)
	}
	c.mu.Unlock()
	// BOS-661: this attach is gone, so any partial upload it owns is
	// unfinishable — drop the bytes rather than leave them on disk
	// until the janitor. One call covers every way an attach dies: a
	// browser close, a PTY exit, and stream teardown all end here.
	c.cancelAttachUploads(state.id)
}

func (c *TerminalStreamClient) forwardAttachExit(ctx context.Context, state *activeAttach, outbound chan<- *pb.TerminalServerMessage, exited <-chan *pb.TerminalAttachExited) bool {
	select {
	case ev, ok := <-exited:
		if !ok {
			return false
		}
		if state.seal != nil {
			if reason := state.seal.stopReason(); reason != "" {
				ev = &pb.TerminalAttachExited{AttachId: state.id, ExitCode: -1, Reason: reason}
			}
		}
		state.forwardedExit.Store(ev)
		c.sendServerMessage(ctx, outbound, &pb.TerminalServerMessage{Msg: &pb.TerminalServerMessage_Exited{Exited: ev}})
		return true
	case <-state.outputStopped:
	case <-ctx.Done():
	}
	return false
}
