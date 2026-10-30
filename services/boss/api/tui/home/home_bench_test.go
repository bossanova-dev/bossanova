package home

import (
	"context"
	"fmt"
	"testing"

	tea "charm.land/bubbletea/v2"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// homePerformanceFixture is also usable on the lazy-table parent for a like-for-like baseline.
func homePerformanceFixture(count int) Model {
	h := NewModel(nil, context.Background(), nil)
	h.Loading, h.LoggedIn, h.RepoCount = false, true, 1
	h.Width, h.Height = 160, 60
	h.Settings.DisplayOpenPorts = true
	for i := range count {
		kind := []pb.AttentionKind{pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, pb.AttentionKind_ATTENTION_KIND_REVIEW, pb.AttentionKind_ATTENTION_KIND_FIX}[i%3]
		s := &pb.Session{Id: fmt.Sprintf("perf-%03d", i), Title: fmt.Sprintf("Implement session %03d", i), RepoDisplayName: "app", BranchName: "feature", PrNumber: i32ForTest(int32(i + 1)), Attention: &pb.SessionAttention{Kind: kind, ProgressLabel: "building"}}
		if i%7 == 0 {
			s.Attention = nil
			s.LastRepairAttemptCount = 3
			s.LastRepairExitError = "exit status 1"
		}
		if i%5 == 0 {
			s.HttpEndpoints = []*pb.HTTPEndpoint{{Port: 3000, Url: "http://localhost:3000"}}
		}
		h.Sessions = append(h.Sessions, s)
	}
	h.BuildTableRows()
	// The first selectable row follows the Needs you header.
	h.Table.SetCursor(h.TableCursorForSessionIndex(0))
	h.BuildTableRows()
	_ = h.View()
	return h
}

func BenchmarkHomeKeyDown(b *testing.B) {
	for _, count := range []int{50, 400} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			h := homePerformanceFixture(count)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if h.Table.Cursor() == h.TableCursorForSessionIndex(count-1) {
					// Wrap with the public key path so the next step starts with valid selection styling.
					model, _ := h.Update(tea.KeyPressMsg{Code: tea.KeyHome})
					h = model.(Model)
				}
				model, _ := h.Update(tea.KeyPressMsg{Code: tea.KeyDown})
				h = model.(Model)
				_ = h.View()
			}
		})
	}
}

func BenchmarkHomeSpinnerTick(b *testing.B) {
	h := homePerformanceFixture(400)
	// In-progress rows animate; keep that group in the viewport.
	h.Table.SetCursor(h.TableCursorForSessionIndex(399))
	_ = h.View()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		model, _ := h.Update(h.Spinner.Tick())
		h = model.(Model)
		_ = h.View()
	}
}
