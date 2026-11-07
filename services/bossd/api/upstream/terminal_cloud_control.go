package upstream

import (
	"context"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/config"
	"github.com/recurser/bossalib/kit/daemoncaps"
)

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
	if msg.GetAttach().GetViewerDeviceId() != "" {
		return false
	}
	if input := msg.GetInput(); input != nil {
		if state := c.lookupAttach(input.AttachId); state != nil && state.seal != nil {
			return false
		}
	}
	if daemoncaps.CloudControlAllowsTerminal(cloudControlProto(c.cloudControl), msg) {
		return false
	}
	switch m := msg.GetMsg().(type) {
	case *pb.TerminalClientMessage_Attach:
		c.sendExited(ctx, outbound, m.Attach.GetAttachId(), "cloud control is private on this machine")
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
		if cloudControlMode(c.cloudControl) != config.CloudControlFull {
			return
		}
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
	if err := state.attach.Resize(cmd.GetCols(), cmd.GetRows()); err != nil {
		c.logger.Debug().Err(err).Str("attach_id", cmd.GetAttachId()).Msg("terminal stream: resize failed")
	}
}

func (s *activeAttach) refuseOutput() {
	if s.outputRefused.CompareAndSwap(false, true) {
		close(s.outputStopped)
	}
}

// runWriter drains outbound onto the stream. Returns when outbound is
// closed (the openStream teardown path) or stream.Send fails.
func (c *TerminalStreamClient) runWriter(ctx context.Context, stream terminalBidiStream, outbound <-chan *pb.TerminalServerMessage) {
	refused := make(map[string]bool)
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-outbound:
			if !ok {
				return
			}
			if data := msg.GetData(); data != nil {
				id := data.GetAttachId()
				if refused[id] || (!data.GetIsSealed() && cloudControlMode(c.cloudControl) == config.CloudControlPrivate) {
					refused[id] = true
					if state := c.lookupAttach(id); state != nil {
						state.refuseOutput()
					}
					continue
				}
			}
			if exit := msg.GetExited(); exit != nil && refused[exit.GetAttachId()] {
				delete(refused, exit.GetAttachId())
				msg = &pb.TerminalServerMessage{Msg: &pb.TerminalServerMessage_Exited{Exited: &pb.TerminalAttachExited{
					AttachId: exit.GetAttachId(), ExitCode: -1, Reason: "cloud control is private on this machine",
				}}}
			}
			if err := stream.Send(msg); err != nil {
				c.logger.Debug().Err(err).Msg("terminal stream send failed; writer exiting")
				return
			}
		}
	}
}
