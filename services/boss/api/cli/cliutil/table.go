package cliutil

import (
	"fmt"

	"charm.land/bubbles/v2/table"
	"github.com/recurser/boss/internal/domain/display"
	"github.com/spf13/cobra"
)

func RenderCLITable(cmd *cobra.Command, titles []string, rows [][]string) {
	cells := make([][]string, len(titles))
	tableRows := make([]table.Row, len(rows))
	for i, row := range rows {
		tableRows[i] = row
		for col, cell := range row {
			cells[col] = append(cells[col], cell)
		}
	}
	cols := make([]table.Column, len(titles))
	for i, title := range titles {
		cols[i] = table.Column{Title: title, Width: display.MaxColWidth(title, cells[i], 0)}
	}
	t := table.New(
		table.WithColumns(cols),
		table.WithRows(tableRows),
		table.WithHeight(len(tableRows)+1),
		table.WithWidth(display.CLIColumnsWidth(cols)),
		table.WithStyles(display.CLITableStyles()),
		table.WithFocused(false),
	)
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), t.View())
}
