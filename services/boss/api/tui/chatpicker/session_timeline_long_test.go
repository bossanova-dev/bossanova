package chatpicker

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/api/tui/ui"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func longMultidaySession(width, height int) Model {
	m := detailFixture(pb.AttentionKind_ATTENTION_KIND_UNSPECIFIED, height)
	m.width = width
	m.daemonStatuses = map[string]string{"verify": ui.StatusStopped, "build": ui.StatusStopped}
	m.daemonWaitingReasons = nil
	clock := func(day, hour, minute int) time.Time {
		return time.Date(2026, time.October, day, hour, minute, 0, 0, time.Local)
	}
	m.timeline = nil
	add := func(at time.Time, title, detail string, state pb.TimelineStepState) {
		m.timeline = append(m.timeline, &pb.TimelineStep{At: timestamppb.New(at), Title: title, Detail: detail, State: state})
	}
	done, failed := pb.TimelineStepState_TIMELINE_STEP_STATE_DONE, pb.TimelineStepState_TIMELINE_STEP_STATE_FAILED
	add(clock(1, 0, 3), "Build started", "", done)
	for i := 1; i <= 42; i++ {
		at := clock(1+i/24, i%24, 0)
		switch i % 4 {
		case 1:
			add(at, "Run interrupted by a daemon restart", "", failed)
		case 2:
			add(at, "Resumed", "new run", done)
		case 3:
			add(at, "Hit its usage limit", "", failed)
		default:
			add(at, "Resumed automatically", "new account", done)
		}
	}
	for range 7 {
		add(clock(3, 23, 50), "Run interrupted by a daemon restart", "", failed)
	}
	for range 7 {
		add(clock(3, 23, 51), "Resumed", "new run", done)
	}
	for _, minute := range []int{52, 53} {
		add(clock(3, 23, minute), "Resumed", "new run", done)
	}
	lastDay := clock(4, 10, 0)
	add(lastDay, "Hit its usage limit", "", failed)
	add(lastDay.Add(time.Minute), "Resumed automatically", "new account", done)
	rotation := func(at time.Time) *pb.RotationEvent {
		return &pb.RotationEvent{CreatedAt: timestamppb.New(at), Trigger: pb.RotationTrigger_ROTATION_TRIGGER_USAGE_LIMITED, Outcome: pb.RotationOutcome_ROTATION_OUTCOME_ROTATED}
	}
	m.session.RotationEvents = []*pb.RotationEvent{rotation(lastDay), rotation(clock(2, 4, 30)), rotation(clock(1, 3, 0))}
	m.buildTableRows()
	return m
}

func TestChatPickerTimelineLongMultidayCollapse_ChatPicker_AC234102(t *testing.T) {
	m := longMultidaySession(100, 40)
	steps := m.timelineSteps()
	for _, title := range []string{"Run interrupted by a daemon restart ×7", "Resumed ×7"} {
		found := false
		for _, step := range steps {
			found = found || step.GetTitle() == title
		}
		if !found {
			t.Errorf("missing collapsed row %q among %d timeline entries", title, len(steps))
		}
	}
	if got, want := len(steps), len(m.timeline)-12+1; got != want {
		t.Fatalf("collapsed timeline has %d entries, want %d (two seven-runs and one unnarrated rotation)", got, want)
	}
}

func TestChatPickerTimelineLongMultiday_ChatPicker_AC234101(t *testing.T) {
	check := func(t *testing.T, width, height int) {
		t.Helper()
		m := longMultidaySession(width, height)
		if len(m.timeline) < 54 {
			t.Fatal("long-session fixture must contain at least 54 history steps")
		}
		block := stripANSI(m.timelineBlock())
		shown := 0
		for _, row := range strings.Split(block, "\n") {
			if timelineStepRow.MatchString(row) {
				shown++
			}
		}
		if shown > timelineMaxSteps {
			t.Fatalf("timeline has %d step rows, ceiling %d:\n%s", shown, timelineMaxSteps, block)
		}
		if height == 0 {
			if got, maxRows := lipgloss.Height(block), 1+(timelineMaxSteps+1)*timelineMaxStepRows; got > maxRows {
				t.Fatalf("unsized timeline has %d rows, budget %d", got, maxRows)
			}
		} else {
			screen := ui.RenderBanner(ui.ViewChatPicker, ui.BannerOpts{Session: m.session, Width: width}) + "\n" + m.View().Content
			if got := lipgloss.Height(screen); got > height {
				t.Fatalf("frame has %d rows, budget %d:\n%s", got, height, stripANSI(screen))
			}
			if got, want := m.tableHeight(), m.detailsReservedTableRows(); got < want {
				t.Fatalf("chat table keeps %d rows, needs %d", got, want)
			}
		}
		if block != "" {
			assertTimelineFold(t, m)
		}
		assertLongTimelineOrderAndDates(t, m, block)
	}
	t.Run("unsized", func(t *testing.T) { check(t, 0, 0) })
	for _, width := range []int{80, 100, 120} {
		for height := 18; height <= 60; height++ {
			t.Run(fmt.Sprintf("%dx%d", width, height), func(t *testing.T) { check(t, width, height) })
		}
	}
}

func assertLongTimelineOrderAndDates(t *testing.T, m Model, block string) {
	t.Helper()
	steps := m.timelineSteps()
	for i := 1; i < len(steps); i++ {
		if steps[i].GetAt().AsTime().Before(steps[i-1].GetAt().AsTime()) {
			t.Fatalf("step %d is before its predecessor", i)
		}
	}
	var kept []*pb.TimelineStep
	var rows []string
	next := 0
	for _, row := range strings.Split(block, "\n") {
		if fold := timelineFoldRow.FindStringSubmatch(row); fold != nil {
			var count int
			if _, err := fmt.Sscan(fold[1], &count); err != nil {
				t.Fatal(err)
			}
			next += count
		} else if timelineStepRow.MatchString(row) {
			kept, rows = append(kept, steps[next]), append(rows, row)
			next++
		}
	}
	if len(kept) < 2 || kept[0].GetAt().AsTime().Local().Day() == kept[len(kept)-1].GetAt().AsTime().Local().Day() {
		return
	}
	previousDay := ""
	for i, step := range kept {
		day := step.GetAt().AsTime().Local().Format("Jan _2")
		if day != previousDay && !strings.Contains(rows[i], day) {
			t.Errorf("first row of %s lacks a date: %q", day, rows[i])
		}
		previousDay = day
	}
}

func TestChatPickerTimelineLongMultidayGolden(t *testing.T) {
	m := longMultidaySession(100, 40)
	screen := stripANSI(ui.RenderBanner(ui.ViewChatPicker, ui.BannerOpts{Session: m.session, Width: m.width}) + "\n" + m.View().Content)
	assertChatPickerGolden(t, "chatpicker-timeline-long-multiday", screen)
	for _, row := range strings.Split(screen, "\n") {
		if lipgloss.Width(row) > m.width {
			t.Fatalf("overwide row: %q", row)
		}
	}
}

func TestTimelineLongMultidayDoesNotMutateCache(t *testing.T) {
	m := longMultidaySession(100, 40)
	want := make([]*pb.TimelineStep, len(m.timeline))
	for i, step := range m.timeline {
		want[i] = proto.CloneOf(step)
	}
	m.timelineBlock()
	for i, step := range m.timeline {
		if !proto.Equal(step, want[i]) {
			t.Fatalf("render changed cached step %d", i)
		}
	}
}

func TestCollapseRepeatedSteps(t *testing.T) {
	base := &pb.TimelineStep{
		At:    timestamppb.New(time.Date(2026, time.October, 3, 23, 50, 1, 0, time.Local)),
		Title: "Resumed", Detail: "new run", State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE,
	}
	for _, tt := range []struct {
		name   string
		change func(*pb.TimelineStep)
		merge  bool
	}{
		{"identical", func(*pb.TimelineStep) {}, true},
		{"same displayed minute", func(s *pb.TimelineStep) { s.At.Seconds += 20 }, true},
		{"different minute", func(s *pb.TimelineStep) { s.At.Seconds += 60 }, false},
		{"different day", func(s *pb.TimelineStep) { s.At.Seconds += 24 * 60 * 60 }, false},
		{"different title", func(s *pb.TimelineStep) { s.Title = "Build started" }, false},
		{"different detail", func(s *pb.TimelineStep) { s.Detail = "another run" }, false},
		{"different state", func(s *pb.TimelineStep) { s.State = pb.TimelineStepState_TIMELINE_STEP_STATE_FAILED }, false},
		{"missing timestamp", func(s *pb.TimelineStep) { s.At = nil }, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			second := proto.CloneOf(base)
			tt.change(second)
			input := []*pb.TimelineStep{base, second}
			before := proto.CloneOf(second)
			got := collapseRepeatedSteps(input)
			wantLen := 2
			if tt.merge {
				wantLen = 1
			}
			if len(got) != wantLen {
				t.Fatalf("got %d entries, want %d", len(got), wantLen)
			}
			if tt.merge {
				want := proto.CloneOf(base)
				want.Title += " ×2"
				if !proto.Equal(got[0], want) || got[0] == base {
					t.Fatalf("collapsed entry = %v, want a clone of %v", got[0], want)
				}
			} else if got[0] != base || got[1] != second {
				t.Fatal("distinct single entries changed")
			}
			if input[0] != base || input[1] != second || !proto.Equal(second, before) || base.GetTitle() != "Resumed" {
				t.Fatal("collapse mutated its input")
			}
		})
	}
	for _, state := range []pb.TimelineStepState{pb.TimelineStepState_TIMELINE_STEP_STATE_DONE, pb.TimelineStepState_TIMELINE_STEP_STATE_CURRENT} {
		t.Run(state.String(), func(t *testing.T) {
			step := proto.CloneOf(base)
			step.At, step.State = nil, state
			input := []*pb.TimelineStep{step, step, step, step, step, step, step}
			got := collapseRepeatedSteps(input)
			if state == pb.TimelineStepState_TIMELINE_STEP_STATE_CURRENT {
				if len(got) != len(input) {
					t.Fatal("current steps collapsed")
				}
			} else if len(got) != 1 || got[0].GetTitle() != "Resumed ×7" || got[0].GetAt() != nil {
				t.Fatalf("undated repeated steps did not collapse: %v", got)
			}
		})
	}
	if got := collapseRepeatedSteps([]*pb.TimelineStep{base}); len(got) != 1 || got[0] != base {
		t.Fatal("single entry changed")
	}
}

func TestCollapseRepeatedStepsKeepCountVisible(t *testing.T) {
	for _, tt := range []struct {
		name  string
		width int
		title string
	}{
		{"narrow restart", 40, "Run interrupted by a daemon restart"},
		{"long custom title", 80, strings.Repeat("Renamed automation chat ", 5)},
		{"wide characters", 40, strings.Repeat("再起動", 12)},
		{"unsized", 0, "Run interrupted by a daemon restart"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := longMultidaySession(tt.width, 40)
			m.session.RotationEvents = nil
			m.timeline = []*pb.TimelineStep{{
				At:    timestamppb.New(time.Date(2026, time.October, 2, 0, 3, 0, 0, time.Local)),
				Title: "Build started", State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE,
			}}
			for range 7 {
				m.timeline = append(m.timeline, &pb.TimelineStep{
					At:    timestamppb.New(time.Date(2026, time.October, 3, 23, 50, 0, 0, time.Local)),
					Title: tt.title, Detail: "new run", State: pb.TimelineStepState_TIMELINE_STEP_STATE_FAILED,
				})
			}
			block := stripANSI(m.timelineBlock())
			if !strings.Contains(block, "×7") {
				t.Fatalf("collapsed count is hidden at width %d:\n%s", tt.width, block)
			}
			for _, row := range strings.Split(block, "\n") {
				if tt.width > 0 && lipgloss.Width(row) > m.chatPickerProseAvailWidth() {
					t.Fatalf("overwide timeline row: %q", row)
				}
			}
		})
	}
}

func TestCollapseRepeatedStepsKeepCountAfterHeightTrim(t *testing.T) {
	for _, count := range []int{7, 12} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			m := longMultidaySession(80, 40)
			m.session.RotationEvents = nil
			at := time.Date(2026, time.October, 3, 23, 50, 0, 0, time.Local)
			m.timeline = []*pb.TimelineStep{{At: timestamppb.New(at.Add(-time.Minute)), Title: "Build started", State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE}}
			for range count {
				m.timeline = append(m.timeline, &pb.TimelineStep{
					At: timestamppb.New(at), Title: strings.Repeat("Renamed automation chat ", 5), Detail: "failed to start", State: pb.TimelineStepState_TIMELINE_STEP_STATE_FAILED,
				})
			}
			m.height = m.chatListOverhead() + m.detailsReservedTableRows() + 3
			if got := m.timelineRowBudget(); got != 2 {
				t.Fatalf("timeline budget = %d, want one fold row and one step row", got)
			}
			block := stripANSI(m.timelineBlock())
			if !strings.Contains(block, fmt.Sprintf("×%d", count)) {
				t.Fatalf("height trimming hid the collapsed count:\n%s", block)
			}
			if got := lipgloss.Height(block); got > 3 {
				t.Fatalf("timeline has %d rows, want heading plus two budgeted rows", got)
			}
			for _, row := range strings.Split(block, "\n") {
				if lipgloss.Width(row) > m.chatPickerProseAvailWidth() {
					t.Fatalf("overwide timeline row: %q", row)
				}
			}
		})
	}
}

func TestCollapseRepeatedStepsKeepCountInNarrowTimeline(t *testing.T) {
	for _, tt := range []struct {
		width, count int
		visible      bool
	}{
		{16, 7, false},
		{17, 7, true},
		{18, 12, true},
		{17, 12, false},
		{19, 7, true},
	} {
		t.Run(fmt.Sprintf("width%d/count%d", tt.width, tt.count), func(t *testing.T) {
			m := longMultidaySession(tt.width, 40)
			m.session.RotationEvents = nil
			m.timeline = nil
			step := &pb.TimelineStep{
				At: timestamppb.New(time.Date(2026, time.October, 3, 23, 50, 0, 0, time.Local)), Title: "Repeated failed chat", State: pb.TimelineStepState_TIMELINE_STEP_STATE_FAILED,
			}
			for range tt.count {
				m.timeline = append(m.timeline, step)
			}
			block := stripANSI(m.timelineBlock())
			if tt.visible && !strings.Contains(block, fmt.Sprintf("×%d", tt.count)) {
				t.Fatalf("count fits but is hidden:\n%s", block)
			}
			if !tt.visible && strings.Contains(block, "×") {
				t.Fatalf("count does not fit but a partial count is shown:\n%s", block)
			}
			for _, row := range strings.Split(block, "\n") {
				if lipgloss.Width(row) > m.chatPickerProseAvailWidth() {
					t.Fatalf("overwide timeline row: %q", row)
				}
			}
			if step.GetTitle() != "Repeated failed chat" {
				t.Fatal("render mutated the cached title")
			}
		})
	}
}
