package ui

import (
	"fmt"
	"testing"

	"charm.land/bubbles/v2/table"
)

func tableMovementBenchmarkFixture(n int) ([]table.Column, []table.Row) {
	cols := []table.Column{CursorColumn, {Title: "NAME", Width: 24}, {Title: "REPO", Width: 20},
		{Title: "BRANCH", Width: 24}, {Title: "STATUS", Width: 12}}
	rows := make([]table.Row, n)
	for i := range rows {
		rows[i] = table.Row{"", fmt.Sprintf("session-%03d", i), "bossanova", "feature/session-list", "working"}
	}
	return cols, rows
}

func BenchmarkTableMoveDownView(b *testing.B) {
	for _, count := range []int{50, 400} {
		b.Run(fmt.Sprintf("rows=%d", count), func(b *testing.B) {
			cols, rows := tableMovementBenchmarkFixture(count)
			m := NewBossTable(cols, rows, 60)
			m.SetWidth(ColumnsWidth(cols))
			_ = m.View()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if m.Cursor() == count-1 {
					m.GotoTop()
				}
				m.MoveDown(1)
				_ = m.View()
			}
		})
	}
}

func BenchmarkBubblesTableMoveDownView(b *testing.B) {
	for _, count := range []int{50, 400} {
		b.Run(fmt.Sprintf("rows=%d", count), func(b *testing.B) {
			cols, rows := tableMovementBenchmarkFixture(count)
			m := bubblesTableOracle(cols, rows, 60)
			m.SetWidth(ColumnsWidth(cols))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if m.Cursor() == count-1 {
					m.GotoTop()
				}
				m.MoveDown(1)
				_ = m.View()
			}
		})
	}
}
