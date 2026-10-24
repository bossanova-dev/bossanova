package chatpicker

import (
	"slices"
	"strings"
	"testing"

	"github.com/recurser/boss/api/tui/ui"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"google.golang.org/protobuf/proto"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestAttentionActionDetailsConfirm(t *testing.T) {
	for _, tc := range []struct {
		key    rune
		action pb.AttentionAction
		call   string
	}{
		{'m', pb.AttentionAction_ATTENTION_ACTION_MERGE, "merge:session-1"},
		{'x', pb.AttentionAction_ATTENTION_ACTION_DISMISS, "dismiss:session-1:resume:auth"},
		{'r', pb.AttentionAction_ATTENTION_ACTION_RESUME, "resume:session-1"},
	} {
		t.Run(string(tc.key), func(t *testing.T) {
			c := &attentionStub{}
			m := seedChatPicker(&c.chatPickerStub, ui.StatusStopped)
			m.client = c
			m.session = attentionSession(tc.action)
			model, _ := m.Update(keyPress(tc.key))
			m = model.(Model)
			if !strings.Contains(m.View().Content, "[y/enter]") {
				t.Fatal("missing confirmation")
			}
			model, cmd := m.Update(attentionKey("esc"))
			m = model.(Model)
			if cmd != nil || len(c.calls) != 0 {
				t.Fatal("cancel wrote")
			}
			model, _ = m.Update(keyPress(tc.key))
			m = model.(Model)
			model, cmd = m.Update(attentionKey("enter"))
			m = model.(Model)
			model, duplicate := m.Update(attentionKey("enter"))
			m = model.(Model)
			if duplicate != nil {
				t.Fatal("duplicate confirm")
			}
			if cmd == nil {
				t.Fatal("confirm did not dispatch")
			}
			model, _ = m.Update(cmd())
			m = model.(Model)
			if len(c.calls) != 1 || c.calls[0] != tc.call {
				t.Fatalf("calls=%v", c.calls)
			}
			if tc.key == 'm' && c.head != "rendered-head" {
				t.Fatalf("head=%q", c.head)
			}
			if m.statusNote == "" {
				t.Fatal("missing status")
			}
		})
	}
}

func attentionDetails(c *attentionStub, s *pb.Session) Model {
	m := seedChatPicker(&c.chatPickerStub, ui.StatusStopped)
	m.client = c
	m.session = s
	m.timeline = []*pb.TimelineStep{{Title: "Opened", State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE}}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	return updated.(Model)
}

func TestAttentionFixRowsOfferNoMerge(t *testing.T) {
	fault := attentionSession(pb.AttentionAction_ATTENTION_ACTION_RESUME, pb.AttentionAction_ATTENTION_ACTION_DISMISS)
	fault.Attention.Kind, fault.Attention.ParkCodes = pb.AttentionKind_ATTENTION_KIND_FIX, nil
	rows := map[string]*pb.Session{"fault": fault}
	for _, code := range knownParkCodes {
		if s := parkSession(code); s.GetAttention().GetKind() == pb.AttentionKind_ATTENTION_KIND_FIX {
			rows[code] = s
		}
	}
	for name, s := range rows {
		t.Run(name, func(t *testing.T) {
			s.DisplayStatus = pb.DisplayStatus_DISPLAY_STATUS_PASSING
			if slices.Contains(ui.AttentionFooter(s, true), "[m] merge") {
				t.Fatalf("footer offers merge: %q", ui.AttentionFooter(s, true))
			}
			c := &attentionStub{}

			m := attentionDetails(c, proto.CloneOf(s))
			updated, cmd := m.Update(keyPress('m'))
			m = updated.(Model)
			if cmd != nil || m.confirm != confirmNone || m.attentionAction.Active() {
				t.Fatal("[m] raised a merge on a details fix row")
			}
			if screen := ansi.Strip(m.View().Content); strings.Contains(screen, "[m]erge") {
				t.Fatalf("details footer offers merge:\n%s", screen)
			}
			if len(c.calls) != 0 {
				t.Fatalf("calls = %v", c.calls)
			}
		})
	}
}
