package tui

import (
	"context"

	"github.com/recurser/boss/internal/domain/rpc"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/api/tui/chatpicker"
	"github.com/recurser/boss/api/tui/ui"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

func pickerSessionFixture(id string, session *pb.Session) chatpicker.Model {
	m := chatpicker.NewModel(&routerClientFixture{}, context.Background(), id, "", chatpicker.ExternalActions{ChatTitle: agentChatTitle})
	m.SetSize(100, 40)
	m, _ = updateModel(m, chatpicker.SessionMsg{Session: session})
	m, _ = updateModel(m, chatpicker.ChatsListedMsg{})
	return m
}

func seedChatPicker(c chatpicker.Client, status string) chatpicker.Model {
	m := chatpicker.NewModel(c, context.Background(), "session-1", "", chatpicker.ExternalActions{ChatTitle: agentChatTitle})
	m.SetSize(100, 40)
	m, _ = updateModel(m, chatpicker.ChatsListedMsg{Chats: []*pb.AgentChat{{SessionId: "session-1", AgentSessionId: "agent-1", Title: "Test chat"}}, DaemonStatuses: map[string]string{"agent-1": status}})
	return m
}

// pickerActionFixture enters the same in-flight mode as a confirmed user action,
// leaving its RPC command unexecuted until the test supplies the result.
func pickerActionFixture(id, action string) chatpicker.Model {
	m := pickerSessionFixture(id, &pb.Session{Id: id, PrNumber: proto.Int32(42), DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_PASSING})
	m, _ = updateModel(m, chatpicker.ChatsListedMsg{Chats: []*pb.AgentChat{{SessionId: id, AgentSessionId: "agent-1", Title: "Test chat"}}, DaemonStatuses: map[string]string{"agent-1": ui.StatusWorking}})
	var cmd tea.Cmd
	m, cmd = updateModel(m, keyPress(rune(action[0])))
	if action == "c" && cmd != nil {
		m, _ = updateModel(m, cmd())
		m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	}
	m, _ = updateModel(m, keyPress('y'))
	return m
}

type pickerAgentsFixture struct{ routerClientFixture }

func (*pickerAgentsFixture) ListAgents(context.Context) ([]rpc.AgentInfo, error) {
	return []rpc.AgentInfo{{Name: "claude"}, {Name: "codex"}}, nil
}

// loadPickerAgents executes just the agent-list command from Init; session and
// periodic commands are driven explicitly by the surrounding test.
func loadPickerAgents(m chatpicker.Model) chatpicker.Model {
	cmds := m.Init()().(tea.BatchMsg)
	m, _ = updateModel(m, cmds[1]())
	return m
}
