package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/recurser/boss/internal/domain/rpc"

	"github.com/recurser/boss/api/tui/ui"

	"github.com/charmbracelet/x/ansi"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestHomeSessionPhase(t *testing.T) {
	h := HomeModel{Spinner: ui.NewStatusSpinner(), DaemonPhases: map[string]string{"s": "reviewing"}}
	for _, label := range []string{"working", "? question", "waiting"} {
		sess := &pb.Session{Id: "s", DisplayLabel: label, DisplaySpinner: true, DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_DANGER}
		got := h.RenderSessionStatus(sess)
		want := ui.RenderDisplayStatus(sess, h.Spinner)
		if label == "working" {
			want = ui.StyleStatusDanger.Render(h.Spinner.View() + "reviewing")
		}
		if got != want {
			t.Errorf("label %q: got %q, want %q", label, got, want)
		}
	}
	h.DaemonPhases["s"] = strings.Repeat("界", 20)
	got := ansi.Strip(h.RenderSessionStatus(&pb.Session{Id: "s", DisplayLabel: "working", DisplaySpinner: true}))
	if ansi.StringWidth(got) > 16 || !strings.HasSuffix(got, "…") {
		t.Errorf("phase must fit 16 cells with ellipsis: %q", got)
	}
}

// Phase polling uses real additive status entries, rather than model-only seeds.
type phasePollingStub struct{ routerClientFixture }

func (s *phasePollingStub) ListChats(context.Context, string) ([]*pb.AgentChat, error) {
	return []*pb.AgentChat{{AgentSessionId: "working", CreatedAt: timestamppb.Now()}}, nil
}

func (s *phasePollingStub) GetChatStatuses(context.Context, string) ([]*pb.ChatStatusEntry, error) {
	return []*pb.ChatStatusEntry{{AgentSessionId: "working", Status: pb.ChatStatus_CHAT_STATUS_WORKING, Phase: "reviewing"}}, nil
}
func (s *phasePollingStub) GetSessionStatuses(context.Context, []string) ([]*pb.SessionStatusEntry, error) {
	return []*pb.SessionStatusEntry{{SessionId: "s", Status: pb.ChatStatus_CHAT_STATUS_WORKING, Phase: "reviewing"}}, nil
}
func (s *phasePollingStub) ListSessionsWithReadFailures(context.Context, *pb.ListSessionsRequest, rpc.SessionReadOptions) ([]*pb.Session, []*pb.OrganizationSessionReadFailure, error) {
	return []*pb.Session{{Id: "s", DisplayLabel: "working", DisplaySpinner: true}}, nil, nil
}
func TestPhasePolling(t *testing.T) {
	c := &phasePollingStub{}
	msg := fetchSessions(c, context.Background(), 0, 0, false)().(sessionListMsg)
	if msg.DaemonPhases["s"] != "reviewing" {
		t.Fatalf("home poll lost phase: %v", msg.DaemonPhases)
	}
	h := NewHomeModel(c, context.Background(), nil)
	updated, _ := h.ApplySessionList(msg)
	h = updated.(HomeModel)
	if got := ansi.Strip(h.RenderSessionStatus(h.Sessions[0])); got != h.Spinner.View()+"reviewing" {
		t.Errorf("home poll phase %q", got)
	}

}
