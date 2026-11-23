package upstream

import (
	"context"
	"sync"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/config"
)

// uploadOnlyReadOnlyReason is the exit an upload-only attach reports when the
// repository's cloud control cannot accept a file.
const uploadOnlyReadOnlyReason = "cloud control is read-only on this machine"

// uploadOnlyAttach stands in for the PTY on a chat reply box's upload
// channel. It spawns nothing, so the agent's tmux window keeps its size and
// nothing reaches its input line; the attach exists only so upload frames
// have a registered chat and the ordinary pump, close and teardown paths
// apply unchanged.
type uploadOnlyAttach struct {
	id     string
	output chan *pb.TerminalDataChunk
	exited chan *pb.TerminalAttachExited
	once   sync.Once
}

func newUploadOnlyAttach(id string) *uploadOnlyAttach {
	return &uploadOnlyAttach{
		id:     id,
		output: make(chan *pb.TerminalDataChunk),
		exited: make(chan *pb.TerminalAttachExited, 1),
	}
}

func (a *uploadOnlyAttach) Output() <-chan *pb.TerminalDataChunk    { return a.output }
func (a *uploadOnlyAttach) Exited() <-chan *pb.TerminalAttachExited { return a.exited }
func (a *uploadOnlyAttach) Input([]byte) error                      { return nil }
func (a *uploadOnlyAttach) Resize(uint32, uint32) error             { return nil }

func (a *uploadOnlyAttach) Close() error {
	a.once.Do(func() {
		a.exited <- &pb.TerminalAttachExited{AttachId: a.id, Reason: "closed"}
		close(a.output)
	})
	return nil
}

// handleUploadOnlyAttach registers a chat's upload channel. It resolves the
// chat's repository exactly as a terminal attach does, refuses unless cloud
// control is full, and never wakes the chat: a file can wait for a send
// whether or not the agent is running.
func (c *TerminalStreamClient) handleUploadOnlyAttach(ctx context.Context, cmd *pb.TerminalAttachCommand, outbound chan<- *pb.TerminalServerMessage) {
	attachID, chatID := cmd.GetAttachId(), cmd.GetChatId()
	if cmd.GetViewerDeviceId() != "" {
		c.sendExited(ctx, outbound, attachID, "upload attach: viewer devices are not supported")
		return
	}
	if c.uploads == nil {
		c.sendExited(ctx, outbound, attachID, "upload: not supported by this daemon")
		return
	}
	if cmd.GetActingUserId() == "" {
		c.sendExited(ctx, outbound, attachID, "upload attach: no acting user")
		return
	}
	repoID, reason := c.attachTargetRepo(ctx, chatID)
	if reason != "" {
		c.sendExited(ctx, outbound, attachID, reason)
		return
	}
	switch repoCloudControlMode(ctx, c.cloudControl, repoID) {
	case config.CloudControlFull:
	case config.CloudControlPrivate:
		c.sendExited(ctx, outbound, attachID, terminalPrivateReason)
		return
	default:
		c.sendExited(ctx, outbound, attachID, uploadOnlyReadOnlyReason)
		return
	}
	attachCtx, cancel := context.WithCancel(ctx)
	state := &activeAttach{
		repoID:        repoID,
		writable:      true,
		uploadOnly:    true,
		actingUserID:  cmd.GetActingUserId(),
		id:            attachID,
		outputStopped: make(chan struct{}),
		chatID:        chatID,
		attach:        newUploadOnlyAttach(attachID),
		cancel:        cancel,
	}
	if start, ok := c.registerAttach(attachCtx, state, outbound); ok {
		start()
	}
	c.logger.Debug().Str("attach_id", attachID).Str("chat_id", chatID).Msg("terminal stream: upload-only attach opened")
}
