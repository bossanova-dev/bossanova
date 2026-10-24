package chatpicker

import (
	"context"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type phasePollingStub struct{ chatPickerStub }

func (s *phasePollingStub) ListChats(context.Context, string) ([]*pb.AgentChat, error) {
	return []*pb.AgentChat{{AgentSessionId: "working", CreatedAt: timestamppb.Now()}}, nil
}
func (s *phasePollingStub) GetChatStatuses(context.Context, string) ([]*pb.ChatStatusEntry, error) {
	return []*pb.ChatStatusEntry{{AgentSessionId: "working", Status: pb.ChatStatus_CHAT_STATUS_WORKING, Phase: "reviewing"}}, nil
}

func TestChatPickerPhasePolling(t *testing.T) {
	c := &phasePollingStub{}
	m := newTestModel(c, context.Background(), "s", "")
	listed := m.listChats()().(chatPickerReply).msg.(ChatsListedMsg)
	if listed.DaemonPhases["working"] != "reviewing" {
		t.Errorf("picker list poll lost phase: %v", listed.DaemonPhases)
	}
	refreshed := m.refreshStatuses()().(chatPickerReply).msg.(RefreshMsg)
	if refreshed.DaemonPhases["working"] != "reviewing" {
		t.Errorf("picker refresh poll lost phase: %v", refreshed.DaemonPhases)
	}
}
