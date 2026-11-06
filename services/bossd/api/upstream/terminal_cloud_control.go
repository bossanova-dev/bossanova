package upstream

import (
	"context"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/config"
	"github.com/recurser/bossalib/kit/daemoncaps"
)

// refuseTerminalControl checks each control frame afresh, including after a
// local revocation on an already-attached terminal. No input bytes are logged.
func (c *TerminalStreamClient) refuseTerminalControl(ctx context.Context, msg *pb.TerminalClientMessage, outbound chan<- *pb.TerminalServerMessage) bool {
	access, ok := daemoncaps.TerminalMessageAccess(msg)
	if ok && access != daemoncaps.AccessControl {
		return false
	}
	if cloudControlMode(c.cloudControl) == config.CloudControlFull {
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
	if err := state.attach.Input(cmd.GetData()); err != nil {
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
