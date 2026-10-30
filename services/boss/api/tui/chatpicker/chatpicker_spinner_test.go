package chatpicker

import (
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"charm.land/bubbles/v2/spinner"
	"github.com/recurser/boss/api/tui/ui"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func spinnerPicker() Model {
	return Model{
		spinner: ui.NewStatusSpinner(), width: 140, height: 40,
		table: ui.NewBossTable(nil, nil, 0),
		chats: []*pb.AgentChat{
			{AgentSessionId: "working", Title: "first", CreatedAt: timestamppb.Now()},
			{AgentSessionId: "stopped", Title: "second", CreatedAt: timestamppb.Now()},
		},
		daemonStatuses: map[string]string{"working": ui.StatusWorking},
	}
}

func spinnerTick(m Model) Model {
	updated, _ := m.Update(spinner.TickMsg{ID: m.spinner.ID()})
	return updated.(Model)
}

func TestChatPickerSpinnerRefreshesOnlyVisibleAnimatedRows_ChatPicker_AC18(t *testing.T) {
	for _, branch := range []string{"working", "detail", "deleting", "waiting", "stopped", "offscreen"} {
		t.Run(branch, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m := spinnerPicker()
				switch branch {
				case "detail":
					m.daemonPhases = map[string]string{"working": "building"}
				case "deleting":
					m.daemonStatuses = nil
					m.deletingAgentSessionID = "working"
				case "waiting":
					m.daemonStatuses["working"] = ui.StatusWaiting
				case "stopped":
					m.daemonStatuses = nil
				}
				if branch == "offscreen" {
					m.height = m.chatListOverhead() + m.attentionDetailsHeight() + 2
				}
				m.buildTableRows()
				if branch == "offscreen" {
					m.table.SetCursor(1)
				}
				before := m.table.Rows()
				_ = m.table.View()
				rendered := m.table.RenderedRows()
				m = spinnerTick(m)
				after := m.table.Rows()
				animated := branch != "stopped" && branch != "offscreen"
				if animated {
					if before[0][4] == after[0][4] {
						t.Fatal("visible animated STATUS did not change")
					}
					if !reflect.DeepEqual(before[0][:4], after[0][:4]) {
						t.Fatal("non-status cells changed")
					}
				} else if !reflect.DeepEqual(before, after) {
					t.Fatal("tick changed static or offscreen rows")
				}
				if !reflect.DeepEqual(before[1], after[1]) {
					t.Fatal("stopped row changed")
				}
				_ = m.table.View()
				want := 0
				if animated {
					want = 1
				}
				if got := m.table.RenderedRows() - rendered; got != want {
					t.Fatalf("rendered %d rows after tick, want %d", got, want)
				}
			})
		})
	}
}

func TestChatPickerSpinnerRefreshesRelativeTime_ChatPicker_AC19(t *testing.T) {
	for _, column := range []string{"CREATED", "ACTIVE"} {
		t.Run(column, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m := spinnerPicker()
				boundary := time.Now().Add(-time.Minute + time.Second)
				if column == "CREATED" {
					m.chats[0].CreatedAt = timestamppb.New(boundary)
				} else {
					m.daemonLastOutput = map[string]time.Time{"working": boundary}
				}
				m.buildTableRows()
				index := 2
				if column == "ACTIVE" {
					index = 3
				}
				if !strings.Contains(m.table.Rows()[0][index], "just now") {
					t.Fatal("expected initial relative time")
				}
				time.Sleep(2 * time.Second)
				m = spinnerTick(m)
				if got := m.table.Rows()[0][index]; !strings.Contains(got, "1m ago") {
					t.Fatalf("relative time = %q, want 1m ago", got)
				}
			})
		})
	}
}

func TestChatPickerSpinnerRefitsChangingFrameWidth_ChatPicker_AC18(t *testing.T) {
	m := spinnerPicker()
	m.spinner = spinner.New(spinner.WithSpinner(spinner.Spinner{Frames: []string{".", "a much wider spinner frame"}, FPS: time.Second}))
	m.daemonPhases = map[string]string{"working": "building"}
	m.buildTableRows()
	before := m.table.Columns()
	m = spinnerTick(m)
	if reflect.DeepEqual(before, m.table.Columns()) {
		t.Fatal("STATUS width did not refit for wider frame")
	}
}
