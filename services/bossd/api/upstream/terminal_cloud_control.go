package upstream

import (
	"context"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/config"
	"github.com/recurser/bossalib/kit/daemoncaps"
)

const (
	terminalPrivateReason               = "cloud control is private on this machine"
	terminalRepoPolicyChangedReason     = "cloud control changed for this repo"
	terminalCloudControlRecheckInterval = time.Second
)

func terminalAttachAllowed(mode config.CloudControlMode, privateSealed bool) bool {
	return mode != config.CloudControlPrivate || privateSealed
}

func (s *activeAttach) policyExitReason() string {
	if s.policyRevoked.Load() {
		return terminalRepoPolicyChangedReason
	}
	return terminalPrivateReason
}

func (c *TerminalStreamClient) refuseTerminalControl(ctx context.Context, msg *pb.TerminalClientMessage, outbound chan<- *pb.TerminalServerMessage) bool {
	var attachID, uploadID string
	switch m := msg.GetMsg().(type) {
	case *pb.TerminalClientMessage_UploadStart:
		attachID, uploadID = m.UploadStart.GetAttachId(), m.UploadStart.GetUploadId()
	case *pb.TerminalClientMessage_UploadChunk:
		attachID, uploadID = m.UploadChunk.GetAttachId(), m.UploadChunk.GetUploadId()
	case *pb.TerminalClientMessage_UploadFinish:
		attachID, uploadID = m.UploadFinish.GetAttachId(), m.UploadFinish.GetUploadId()
	case *pb.TerminalClientMessage_UploadCancel:
		attachID, uploadID = m.UploadCancel.GetAttachId(), m.UploadCancel.GetUploadId()
	}
	if attachID != "" && c.refuseSealedUpload(ctx, outbound, attachID, uploadID) {
		return true
	}

	if msg.GetAttach() != nil {
		return false
	}
	if attachID == "" {
		if input := msg.GetInput(); input != nil {
			attachID = input.GetAttachId()
		}
		if resize := msg.GetResize(); resize != nil {
			attachID = resize.GetAttachId()
		}
	}
	state := c.lookupAttach(attachID)
	if state == nil && msg.GetUploadStart() != nil {
		return false
	}
	if msg.GetInput() != nil && state != nil && state.seal != nil {
		return false
	}
	mode := c.attachMode(ctx, state)
	if daemoncaps.CloudControlAllowsTerminal(targetModeProto(mode), msg) {
		return false
	}

	switch m := msg.GetMsg().(type) {
	case *pb.TerminalClientMessage_Input:
		if state := c.lookupAttach(m.Input.GetAttachId()); state != nil {
			state.inputRefused.Do(func() { c.logger.Info().Str("attach_id", state.id).Msg("cloud terminal input refused") })
		}
	case *pb.TerminalClientMessage_UploadStart:
		c.refuseCloudUpload(ctx, outbound, m.UploadStart.GetAttachId(), m.UploadStart.GetUploadId())
	case *pb.TerminalClientMessage_UploadChunk:
		c.refuseCloudUpload(ctx, outbound, m.UploadChunk.GetAttachId(), m.UploadChunk.GetUploadId())
	case *pb.TerminalClientMessage_UploadFinish:
		c.refuseCloudUpload(ctx, outbound, m.UploadFinish.GetAttachId(), m.UploadFinish.GetUploadId())
	case *pb.TerminalClientMessage_UploadCancel:
		if c.uploads != nil {
			c.uploads.Cancel(m.UploadCancel.GetAttachId(), m.UploadCancel.GetUploadId())
		}
	}
	return true
}

func (c *TerminalStreamClient) refuseCloudUpload(ctx context.Context, outbound chan<- *pb.TerminalServerMessage, attachID, uploadID string) {
	if c.uploads != nil {
		c.uploads.Cancel(attachID, uploadID)
	}
	c.failUpload(ctx, outbound, attachID, uploadID, "upload: cloud control is read-only on this machine", false)
}

// handleInput routes a TerminalInputCommand to the matching attach. Drops
// silently if the attach_id is unknown — bosso may have reordered messages
// past a Close, or the attach exited on the daemon side before the input
// landed.
func (c *TerminalStreamClient) handleInput(cmd *pb.TerminalInputCommand) {
	state := c.lookupAttach(cmd.GetAttachId())
	if state == nil {
		c.logger.Debug().Str("attach_id", cmd.GetAttachId()).Msg("terminal stream: input for unknown attach; dropping")
		return
	}
	data := cmd.GetData()
	if state.seal != nil {
		var err error
		data, err = state.seal.input(cmd)
		if err != nil {
			state.seal.fail(terminalFrameInvalid)
			return
		}
	}
	if c.attachMode(context.Background(), state) != config.CloudControlFull || state.outputRefused.Load() {
		return
	}
	if err := state.attach.Input(data); err != nil {
		c.logger.Debug().Err(err).Str("attach_id", cmd.GetAttachId()).Msg("terminal stream: input write failed")
	}
}

// handleResize routes a TerminalResizeCommand to the matching attach.
func (c *TerminalStreamClient) handleResize(cmd *pb.TerminalResizeCommand) {
	state := c.lookupAttach(cmd.GetAttachId())
	if state == nil {
		c.logger.Debug().Str("attach_id", cmd.GetAttachId()).Msg("terminal stream: resize for unknown attach; dropping")
		return
	}
	if c.attachMode(context.Background(), state) != config.CloudControlFull || state.outputRefused.Load() {
		return
	}
	if err := state.attach.Resize(cmd.GetCols(), cmd.GetRows()); err != nil {
		c.logger.Debug().Err(err).Str("attach_id", cmd.GetAttachId()).Msg("terminal stream: resize failed")
	}
}

func (s *activeAttach) refuseOutput() {
	if s.outputRefused.CompareAndSwap(false, true) {
		if s.outputStopped != nil {
			close(s.outputStopped)
		}
	}
}

// runWriter drains outbound onto the stream. Returns when outbound is
// closed (the openStream teardown path) or stream.Send fails.
func (c *TerminalStreamClient) runWriter(ctx context.Context, stream terminalBidiStream, outbound <-chan *pb.TerminalServerMessage) {
	refused := make(map[string]string)
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-outbound:
			if !ok {
				return
			}
			if data := msg.GetData(); data != nil && c.refuseTerminalData(ctx, data, refused) {
				continue
			}
			exit := msg.GetExited()
			if exit != nil && refused[exit.GetAttachId()] != "" {
				reason := refused[exit.GetAttachId()]
				delete(refused, exit.GetAttachId())
				msg = &pb.TerminalServerMessage{Msg: &pb.TerminalServerMessage_Exited{Exited: &pb.TerminalAttachExited{
					AttachId: exit.GetAttachId(), ExitCode: -1, Reason: reason,
				}}}
			}
			if err := stream.Send(msg); err != nil {
				c.logger.Debug().Err(err).Msg("terminal stream send failed; writer exiting")
				return
			}
			if exit != nil {
				c.retireWrittenAttach(exit)
			}
		}
	}
}

func repoCloudControlMode(ctx context.Context, policy CloudControlPolicy, repoID string) config.CloudControlMode {
	if policy == nil {
		return config.CloudControlReadOnly
	}
	mode, _ := policy.RepoMode(ctx, repoID)
	return config.ResolveCloudControl(string(mode))
}

func (c *TerminalStreamClient) attachMode(ctx context.Context, state *activeAttach) config.CloudControlMode {
	if state == nil {
		return config.CloudControlReadOnly
	}
	mode := repoCloudControlMode(ctx, c.cloudControl, state.repoID)
	state.applyCloudControlMode(mode)
	return mode
}

func (s *activeAttach) applyCloudControlMode(mode config.CloudControlMode) {
	if (s.writable && mode != config.CloudControlFull) || !terminalAttachAllowed(mode, s.privateSealed) {
		if mode != config.CloudControlPrivate {
			s.policyRevoked.Store(true)
		}
		s.refuseOutput()
	}
}

func (c *TerminalStreamClient) recheckIdleAttach(ctx context.Context, state *activeAttach) bool {
	if policy, ok := c.cloudControl.(interface {
		idleRepoMode(context.Context, string) config.CloudControlMode
	}); ok {
		state.applyCloudControlMode(policy.idleRepoMode(ctx, state.repoID))
		return state.outputRefused.Load()
	}
	return c.attachOutputRefused(ctx, state)
}

func (c *TerminalStreamClient) attachOutputRefused(ctx context.Context, state *activeAttach) bool {
	if state.outputRefused.Load() {
		return true
	}
	c.attachMode(ctx, state)
	return state.outputRefused.Load()
}

func (c *TerminalStreamClient) refuseTerminalData(ctx context.Context, data *pb.TerminalDataChunk, refused map[string]string) bool {

	id := data.GetAttachId()
	state := c.lookupOutputAttach(id)
	if state == nil {
		return true
	}
	if state.seal != nil {
		if reason := state.seal.stopReason(); reason != "" {
			refused[id] = reason
			state.refuseOutput()
			return true
		}
	}
	mode := c.attachMode(ctx, state)
	if refused[id] != "" || state.policyRevoked.Load() || state.outputRefused.Load() || !terminalAttachAllowed(mode, data.GetIsSealed()) {
		refused[id] = state.policyExitReason()
		if state := c.lookupAttach(id); state != nil {
			state.refuseOutput()
		}
		return true
	}
	return false
}

func (c *TerminalStreamClient) retireWrittenAttach(exit *pb.TerminalAttachExited) {
	c.mu.Lock()
	state := c.attaches[exit.GetAttachId()]
	if state == nil || state.forwardedExit.Load() != exit {
		c.mu.Unlock()
		return
	}
	delete(c.attaches, state.id)
	c.mu.Unlock()
	if state.seal != nil {
		state.seal.destroy()
	}
}
