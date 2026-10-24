package newsession

import (
	"context"
	"fmt"

	"github.com/recurser/boss/internal/domain/rpc"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/api/tui/ui"
	"github.com/recurser/boss/internal/domain/rpcerr"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

type setupWatchStreamMsg struct {
	streamID *streamIdentity
	stream   rpc.WatchSessionSetupStream
	err      error
}

type setupSettledMsg struct {
	streamID *streamIdentity
	err      error
}

func openSetupWatch(c Client, ctx context.Context, sessionID string, id *streamIdentity) tea.Cmd {
	return func() tea.Msg {
		stream, err := c.WatchSessionSetup(ctx, sessionID)
		return setupWatchStreamMsg{streamID: id, stream: stream, err: err}
	}
}

func (m Model) handleSetupWatchStream(msg setupWatchStreamMsg) (tea.Model, tea.Cmd) {
	if msg.streamID != m.streamID || m.cancel {
		return m, nil
	}
	if msg.err != nil {
		return m.handleSetupSettled(setupSettledMsg{streamID: msg.streamID, err: msg.err})
	}
	m.watchStream = msg.stream
	return m, m.readNextSetupFrame()
}

func (m Model) readNextSetupFrame() tea.Cmd {
	if !m.watchingSetup {
		return readNextStreamMsg(m.createStream, m.acceptedSess, m.streamID)
	}
	stream, id := m.watchStream, m.streamID
	return func() tea.Msg {
		for stream.Receive() {
			switch event := stream.Msg().Event.(type) {
			case *pb.WatchSessionSetupResponse_SetupOutput:
				return setupScriptLineMsg{streamID: id, text: event.SetupOutput.GetText()}
			case *pb.WatchSessionSetupResponse_SetupSettled:
				_ = stream.Close()
				var err error
				if text := event.SetupSettled.GetSetupError(); text != "" {
					err = fmt.Errorf("%s", text)
				}
				return setupSettledMsg{streamID: id, err: err}
			}
		}
		_ = stream.Close()
		err := stream.Err()
		if err == nil {
			err = fmt.Errorf("setup stream ended unexpectedly")
		}
		return setupSettledMsg{streamID: id, err: err}
	}
}

func (m Model) handleSetupSettled(msg setupSettledMsg) (tea.Model, tea.Cmd) {
	if msg.streamID != m.streamID || m.cancel {
		return m, nil
	}
	if m.streamCancel != nil {
		m.streamCancel()
	}
	kind := rpcerr.KindOf(msg.err)
	if msg.err == nil || kind == rpcerr.NotFound || kind == rpcerr.Unimplemented {
		sessionID := m.acceptedSess.GetId()
		return m, func() tea.Msg { return ui.SwitchViewMsg{View: ui.ViewChatPicker, SessionID: sessionID} }
	}
	m.err = msg.err
	return m, nil
}
