package chatpicker

import (
	"strings"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// PR 3162: the session entered its fixing state at 13:18, verify parked the
// rebased head at 14:12, and the operator's message started a run at 17:15.
// The waiting step takes the park's time and place, not the state's start
// drawn last below a later row.
func TestChatPickerTimelineWaitKeepsTheDaemonsTimeAndPlace(t *testing.T) {
	now := time.Now()
	clock := func(hour, minute int) time.Time {
		return time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, time.Local)
	}
	m := detailFixture(pb.AttentionKind_ATTENTION_KIND_FIX, 60)
	m.session.Attention.Since = timestamppb.New(clock(13, 18))
	m.timeline = []*pb.TimelineStep{
		{At: timestamppb.New(clock(11, 0)), Title: "Build started", State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE},
		{At: timestamppb.New(clock(13, 40)), Title: "Run ended unexpectedly", State: pb.TimelineStepState_TIMELINE_STEP_STATE_FAILED},
		{At: timestamppb.New(clock(14, 12)), Title: "Parked for you · UI change", State: pb.TimelineStepState_TIMELINE_STEP_STATE_CURRENT},
		{At: timestamppb.New(clock(17, 15)), Title: "Resumed", Detail: "new run", State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE},
	}
	block := stripANSI(m.timelineBlock())
	t.Logf("timeline:\n%s", block)
	rows := strings.Split(block, "\n")[1:]
	want := []string{
		"  11:00  ✓ Build started",
		"  13:40  ✗ Run ended unexpectedly",
		"  14:12  ✓ Parked for you · UI change",
		"  14:12  ● Waiting for a fix",
		"  17:15  ✓ Resumed",
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %d, want %d:\n%s", len(rows), len(want), block)
	}
	for i, row := range rows {
		if !strings.HasPrefix(row, want[i]) {
			t.Errorf("row %d = %q, want prefix %q", i, row, want[i])
		}
	}
}

func TestChatPickerTimelineKeepsFixPark_ChatPicker_AC14_AC221201(t *testing.T) {
	for _, tt := range []struct{ title, code, reason string }{
		{"Parked for you", "partial-build", "only part of the ticket was finished, so the PR is held"},
		{"Parked for you · partial build", "partial-build", "only part of the ticket was finished, so the PR is held"},
		{"Parked for you", "ledger-open", "the PR checklist still has unfinished items"},
	} {
		t.Run(tt.code+"/"+tt.title, func(t *testing.T) {
			m := detailFixture(pb.AttentionKind_ATTENTION_KIND_FIX, 60)
			m.width = 160
			title, reason := tt.title, tt.reason
			m.session.Attention.ParkCodes = []string{tt.code}
			m.session.Attention.Sentence = reason
			at := m.session.Attention.Since
			m.timeline = []*pb.TimelineStep{
				{At: at, Title: "Build started", State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE},
				{At: at, Title: "CI passed", State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE},
				{At: at, Title: title, Detail: reason, State: pb.TimelineStepState_TIMELINE_STEP_STATE_CURRENT},
			}
			rows := timelineRowsOf(m)
			t.Logf("timeline:\n%s", strings.Join(rows, "\n"))
			stamp := "  " + at.AsTime().Local().Format("15:04") + "  "
			expectTimelineRows(t, rows, []string{stamp + "✓ Build started", stamp + "✓ CI passed", stamp + "✓ " + title, stamp + "● Waiting for a fix"})
			if block := strings.Join(rows, "\n"); strings.Count(block, "●") != 1 || strings.Contains(block, "Waiting for merge") {
				t.Fatalf("wrong waiting point:\n%s", block)
			}
			steps := m.timelineSteps()
			if steps[2].GetDetail() != reason || steps[3].GetAt() != at {
				t.Fatalf("park detail or wait time lost: %v", steps)
			}
			if m.timeline[2].GetState() != pb.TimelineStepState_TIMELINE_STEP_STATE_CURRENT {
				t.Fatal("rendering mutated the fetched timeline")
			}
		})
	}
}

func TestChatPickerTimelineReviewDoesNotDuplicatePark_ChatPicker_AC221201(t *testing.T) {
	for _, title := range []string{"Parked for you", "Parked for you · UI change"} {
		t.Run(title, func(t *testing.T) {
			m := detailFixture(pb.AttentionKind_ATTENTION_KIND_REVIEW, 60)
			m.session.Attention.Sentence = "UI change"
			m.timeline = []*pb.TimelineStep{{At: m.session.Attention.Since, Title: title, Detail: "UI change", State: pb.TimelineStepState_TIMELINE_STEP_STATE_CURRENT}}
			rows := timelineRowsOf(m)
			t.Logf("timeline:\n%s", strings.Join(rows, "\n"))
			expectTimelineRows(t, rows, []string{"  " + m.session.Attention.Since.AsTime().Local().Format("15:04") + "  ● Parked for you"})
			if block := strings.Join(rows, "\n"); !strings.Contains(block, "UI change") || strings.Contains(block, "✓ Parked for you") {
				t.Fatalf("wrong sign-off park:\n%s", block)
			}
		})
	}
}
