package home

import (
	"context"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/api/tui/ui"
	"github.com/recurser/bossalib/displaystatus"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func assertHomeSelection(t *testing.T, h Model) {
	t.Helper()
	rows := h.Table.Rows()
	selected := false
	for i, sess := range h.BoardSessions() {
		row := h.TableCursorForSessionIndex(i)
		wantSelected := row == h.Table.Cursor()
		selected = selected || wantSelected
		if want := h.primaryRow(sess, row, wantSelected); !reflect.DeepEqual(rows[row], want) {
			t.Fatalf("row %d (%s), selected=%v: cells %q, want %q", row, sess.GetId(), wantSelected, rows[row], want)
		}
	}
	if !selected && len(h.BoardSessions()) > 0 {
		t.Fatalf("cursor %d is on an auxiliary row", h.Table.Cursor())
	}
}

func TestHomeArrowAvoidsDataRebuild_Sessions_AC123(t *testing.T) {
	for _, count := range []int{50, 400} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			h := homePerformanceFixture(count)
			layout := h.layout
			for index := 1; index < count; index++ {
				before := h.Table.RenderedRows()
				model, _ := h.Update(tea.KeyPressMsg{Code: tea.KeyDown})
				h = model.(Model)
				if h.SelectedSessionID() != h.BoardSessions()[index].GetId() {
					t.Fatalf("down selected %q, want session %d", h.SelectedSessionID(), index)
				}
				if h.layout != layout {
					t.Fatal("arrow key rebuilt the data layout")
				}
				_ = h.View()
				// Moving across a section can reveal its two header rows as well as a session's sub-rows.
				if delta := h.Table.RenderedRows() - before; delta > 7 {
					t.Fatalf("arrow rendered %d rows, want at most 7", delta)
				}
			}
		})
	}
}

func TestHomeSelectionFollowsPrimaryRows_Sessions_AC123(t *testing.T) {
	h := homePerformanceFixture(12)
	h.Sessions[len(h.Sessions)-1].Attention = &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_DONE}
	h.Sessions[len(h.Sessions)-1].DisplayStatus = pb.DisplayStatus_DISPLAY_STATUS_MERGED
	h.Sessions[len(h.Sessions)-1].DisplayLabel = "✓ merged"
	h.BuildTableRows()
	assertHomeSelection(t, h)
	for _, key := range []rune{tea.KeyDown, tea.KeyUp} {
		for range len(h.Sessions) - 1 {
			model, _ := h.Update(tea.KeyPressMsg{Code: key})
			h = model.(Model)
			assertHomeSelection(t, h)
		}
	}
}

func TestHomeCursorPlacementsRestyle_Sessions_AC123(t *testing.T) {
	for _, highlight := range []bool{false, true} {
		h := homePerformanceFixture(12)
		id := h.BoardSessions()[len(h.BoardSessions())-1].GetId()
		if highlight {
			h.HighlightSessionID = id
		}
		h.restoreTableCursor(id)
		if h.SelectedSessionID() != id {
			t.Fatalf("restored %q, want %q", h.SelectedSessionID(), id)
		}
		assertHomeSelection(t, h)
		h.Filter.SeedApplied("Implement")
		h.rebuildFilteredBoard()
		if h.Table.Cursor() != h.TableCursorForSessionIndex(0) {
			t.Fatal("filter did not place first session")
		}
		assertHomeSelection(t, h)
	}
	h, _ := reorderHome(t)
	h.restoreTableCursor("sess-2")
	h, _ = h.moveSelectedSession(pb.MoveDirection_MOVE_DIRECTION_UP)
	if h.SelectedSessionID() != "sess-2" {
		t.Fatal("reorder lost selected session")
	}
	assertHomeSelection(t, h)
}

func TestHomeSpinnerRefreshBranches_Sessions_AC124(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*Model, *pb.Session)
	}{
		{"archiving", func(h *Model, s *pb.Session) { h.MarkArchiving(s.Id) }},
		{"merging", func(h *Model, s *pb.Session) { h.MarkMerging(ui.AwaitingMerge(s.Id, "merging")) }},
		{"ending archive label", func(_ *Model, s *pb.Session) { s.DisplayLabel = displaystatus.ArchivingLabel; s.DisplaySpinner = true }},
		{"ending merge label", func(_ *Model, s *pb.Session) { s.DisplayLabel = displaystatus.MergingLabel; s.DisplaySpinner = true }},
		{"working display", func(_ *Model, s *pb.Session) { s.DisplayLabel = ui.StatusWorking; s.DisplaySpinner = true }},
		{"working phase", func(h *Model, s *pb.Session) {
			s.DisplayLabel = ui.StatusWorking
			s.DisplaySpinner = true
			h.DaemonPhases = map[string]string{s.Id: "building"}
		}},
		{"in progress", func(_ *Model, s *pb.Session) {
			s.Attention = &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, ProgressLabel: "building"}
		}},
		{"working review", func(_ *Model, s *pb.Session) {
			s.DisplayLabel = ui.StatusWorking
			s.Attention = &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_REVIEW}
		}},
		{"working fix", func(_ *Model, s *pb.Session) {
			s.DisplayLabel = ui.StatusWorking
			s.Attention = &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_FIX}
		}},
		{"working unverified", func(_ *Model, s *pb.Session) {
			s.DisplayLabel = ui.StatusWorking
			s.Attention = &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_MERGE, IsUnverified: true}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := NewModel(nil, context.Background(), nil)
			h.Loading = false
			h.Width, h.Height = 160, 60
			h.Spinner.Spinner = spinner.Spinner{Frames: []string{"A ", "B "}}
			target := &pb.Session{Id: "animate", Title: "Animation"}
			tc.setup(&h, target)
			h.Sessions = []*pb.Session{target, {Id: "still", Title: "Still"}}
			h.BuildTableRows()
			h.restoreTableCursor(target.Id)
			_ = h.View()
			layout := h.layout
			before := h.Table.Rows()
			rendered := h.Table.RenderedRows()
			model, _ := h.Update(h.Spinner.Tick())
			h = model.(Model)
			if h.layout != layout {
				t.Fatal("spinner rebuilt data layout")
			}
			after := h.Table.Rows()
			targetRow, _ := h.TableCursorForSessionID(target.Id)
			changed := 0
			for row := range before {
				for cell := range before[row] {
					if before[row][cell] != after[row][cell] {
						changed++
						if row != targetRow || h.Table.Columns()[cell].Title != "STATUS" {
							t.Fatalf("spinner changed non-status cell %d,%d", row, cell)
						}
					}
				}
			}
			if changed != 1 {
				t.Fatalf("spinner changed %d cells, want one", changed)
			}
			_ = h.View()
			if delta := h.Table.RenderedRows() - rendered; delta > 1 {
				t.Fatalf("spinner rendered %d rows", delta)
			}
		})
	}
}

func TestHomeSpinnerWithoutVisibleAnimation_Sessions_AC124(t *testing.T) {
	for _, offscreen := range []bool{false, true} {
		h := homePerformanceFixture(50)
		for _, s := range h.Sessions {
			s.Attention = nil
			s.DisplayLabel = "stopped"
		}
		if offscreen {
			h.Sessions[0].DisplayLabel = ui.StatusWorking
			h.Sessions[0].DisplaySpinner = true
		}
		h.BuildTableRows()
		h.restoreTableCursor(h.Sessions[len(h.Sessions)-1].Id)
		_ = h.View()
		if offscreen {
			start, _ := h.Table.VisibleRange()
			if start <= 0 {
				t.Fatal("fixture did not scroll animation out of view")
			}
		}
		before := h.Table.Rows()
		rendered := h.Table.RenderedRows()
		layout := h.layout
		model, _ := h.Update(h.Spinner.Tick())
		h = model.(Model)
		_ = h.View()
		if h.layout != layout {
			t.Fatal("idle spinner rebuilt layout")
		}
		if !reflect.DeepEqual(before, h.Table.Rows()) {
			t.Fatal("idle spinner changed table cells")
		}
		if h.Table.RenderedRows() != rendered {
			t.Fatal("idle spinner rendered table rows")
		}
	}
}

func TestHomeLayoutMatchesWalkingRows(t *testing.T) {
	for _, mode := range []string{"sections", "legacy", "filter", "empty"} {
		t.Run(mode, func(t *testing.T) {
			h := homePerformanceFixture(12)
			switch mode {
			case "legacy":
				for _, s := range h.Sessions {
					s.Attention = nil
				}
			case "filter":
				h.Filter.SeedApplied("007")
			case "empty":
				h.Filter.SeedApplied("absent")
			}
			h.BuildTableRows()
			sessions := h.BoardSessions()
			sectioned := h.showsSections()
			var expected []int
			row := 0
			for i, s := range sessions {
				row += h.sectionRows(sessions, i, sectioned)
				expected = append(expected, row)
				if got := h.TableCursorForSessionIndex(i); got != row {
					t.Fatalf("session %d row %d, want %d", i, got, row)
				}
				if got, ok := h.TableCursorForSessionID(s.Id); !ok || got != row {
					t.Fatalf("session %s row %d (%v), want %d", s.Id, got, ok, row)
				}
				row += 1 + h.SubRowCount(s)
			}
			if !slices.Equal(expected, h.primarySessionRows()) {
				t.Fatalf("primary rows %v, want %v", h.primarySessionRows(), expected)
			}
			if h.TableDataRowCount() != row || len(h.Table.Rows()) != row {
				t.Fatalf("row count %d, want %d", h.TableDataRowCount(), row)
			}
			if got, ok := h.TableCursorForSessionID("missing"); ok || got != -1 {
				t.Fatalf("missing ID row %d (%v)", got, ok)
			}
		})
	}
}

func TestHomeSpinnerRefreshesCountdownWarnings_Sessions_AC124(t *testing.T) {
	for _, kind := range []string{"repair", "rotation"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := NewModel(nil, context.Background(), nil)
				h.Loading = false
				h.Width, h.Height = 160, 60
				s := &pb.Session{Id: "countdown", Title: "Countdown"}
				switch kind {
				case "repair":
					s.LastRepairAttemptCount = 3
					s.LastRepairExitError = "exit status 1"
					s.LastRepairStartedAt = timestamppb.New(time.Now().Add(-time.Minute))
				case "rotation":
					s.RotationEvents = []*pb.RotationEvent{{Outcome: pb.RotationOutcome_ROTATION_OUTCOME_EXHAUSTED, ResetAt: timestamppb.New(time.Now().Add(3 * time.Minute))}}
				}
				h.Sessions = []*pb.Session{s, {Id: "still", Title: "Unchanged"}}
				h.BuildTableRows()
				if h.SubRowCount(s) != 1 {
					t.Fatal("fixture must contain one countdown warning")
				}
				_ = h.View()
				before := h.Table.Rows()
				rendered := h.Table.RenderedRows()
				layout := h.layout
				time.Sleep(61 * time.Second)
				model, _ := h.Update(h.Spinner.Tick())
				h = model.(Model)
				after := h.Table.Rows()
				if h.layout != layout {
					t.Fatal("countdown tick rebuilt layout")
				}
				if reflect.DeepEqual(before[1], after[1]) {
					t.Fatal("countdown warning did not advance")
				}
				for row := range before {
					if row != 1 && !reflect.DeepEqual(before[row], after[row]) {
						t.Fatalf("countdown changed unrelated row %d", row)
					}
				}
				_ = h.View()
				if delta := h.Table.RenderedRows() - rendered; delta > 1 {
					t.Fatalf("countdown rendered %d rows", delta)
				}
				if kind == "rotation" {
					time.Sleep(3 * time.Minute)
					model, _ = h.Update(h.Spinner.Tick())
					h = model.(Model)
					if h.layout != layout || len(h.Table.Rows()) != len(before) {
						t.Fatal("expired warning shifted the layout")
					}
					if !reflect.DeepEqual(after[2], h.Table.Rows()[2]) {
						t.Fatal("expired warning changed following session")
					}
					for _, cell := range h.Table.Rows()[1] {
						if cell != "" {
							t.Fatalf("expired warning retained cell %q", cell)
						}
					}
				}
			})
		})
	}
}

func TestHomeSpinnerWithStatusColumnHidden_Sessions_AC124(t *testing.T) {
	h := homePerformanceFixture(12)
	h.Width = 20
	h.BuildTableRows()
	for _, col := range h.Table.Columns() {
		if col.Title == "STATUS" {
			t.Fatal("fixture must drop the status column")
		}
	}
	_ = h.View()
	before := h.Table.Rows()
	rendered := h.Table.RenderedRows()
	model, _ := h.Update(h.Spinner.Tick())
	h = model.(Model)
	_ = h.View()
	if !reflect.DeepEqual(before, h.Table.Rows()) || h.Table.RenderedRows() != rendered {
		t.Fatal("hidden status animation invalidated visible rows")
	}
}

func TestHomeRuntimeAnimationStartsAndSettles_Sessions_AC124(t *testing.T) {
	for _, action := range []string{"merge", "archive"} {
		for _, width := range []int{160, 20} {
			t.Run(action+strconv.Itoa(width), func(t *testing.T) {
				h := NewModel(nil, context.Background(), nil)
				h.Loading = false
				h.Width, h.Height = width, 60
				h.Spinner.Spinner = spinner.Spinner{Frames: []string{"A ", "B ", "C "}}
				s := &pb.Session{Id: "runtime", Title: "Runtime status", Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_MERGE}}
				h.Sessions = []*pb.Session{s, {Id: "still", Title: "Still"}}
				h.BuildTableRows()
				_ = h.View()
				before := h.Table.Rows()
				layout := h.layout
				if action == "merge" {
					h.MarkMerging(ui.AwaitingMerge(s.Id, "pending"))
				} else {
					h.MarkArchiving(s.Id)
				}
				for range 2 {
					rendered := h.Table.RenderedRows()
					model, _ := h.Update(h.Spinner.Tick())
					h = model.(Model)
					if h.layout != layout {
						t.Fatal("runtime animation rebuilt data layout")
					}
					after := h.Table.Rows()
					if width == 160 {
						row, _ := h.TableCursorForSessionID(s.Id)
						if reflect.DeepEqual(before[row], after[row]) {
							t.Fatal("runtime status spinner did not advance")
						}
						if !reflect.DeepEqual(after[row], h.primaryRow(s, row, true)) {
							t.Fatal("runtime status differs from current session status")
						}
					} else if !reflect.DeepEqual(before, after) {
						t.Fatal("hidden status changed visible cells")
					}
					_ = h.View()
					if delta := h.Table.RenderedRows() - rendered; delta > 1 || width == 20 && delta != 0 {
						t.Fatalf("runtime tick rendered %d rows", delta)
					}
					before = after
				}
				if action == "merge" {
					h.ShowActionResult(ui.AttentionActionResultMsg{Action: pb.AttentionAction_ATTENTION_ACTION_MERGE, SessionID: s.Id, Err: context.Canceled})
				} else {
					h.ResolveArchive(s.Id, context.Canceled, false)
				}
				model, _ := h.Update(h.Spinner.Tick())
				h = model.(Model)
				row, _ := h.TableCursorForSessionID(s.Id)
				if !reflect.DeepEqual(h.Table.Rows()[row], h.primaryRow(s, row, true)) {
					t.Fatal("settled runtime status left stale animation")
				}
				if h.layout != layout {
					t.Fatal("settling runtime status rebuilt data layout")
				}
			})
		}
	}
}

func TestHomeOffscreenRuntimeAnimationDoesNotRender_Sessions_AC124(t *testing.T) {
	h := homePerformanceFixture(60)
	for _, s := range h.Sessions {
		s.Attention = nil
		s.DisplayLabel = "stopped"
	}
	target := h.Sessions[0]
	h.BuildTableRows()
	h.moveCursor(h.TableCursorForSessionIndex(len(h.Sessions) - 1))
	_ = h.View()
	targetRow, _ := h.TableCursorForSessionID(target.Id)
	start, _ := h.Table.VisibleRange()
	if targetRow >= start {
		t.Fatal("runtime target must be offscreen")
	}
	before, rendered := h.Table.Rows(), h.Table.RenderedRows()
	h.MarkMerging(ui.AwaitingMerge(target.Id, "pending"))
	h.MarkArchiving(target.Id)
	model, _ := h.Update(h.Spinner.Tick())
	h = model.(Model)
	_ = h.View()
	if !reflect.DeepEqual(before, h.Table.Rows()) || h.Table.RenderedRows() != rendered {
		t.Fatal("offscreen runtime animation invalidated table rows")
	}
}
