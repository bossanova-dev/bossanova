package tui

import (
	"testing"
)

func TestHomeFilterRebuildPreservesAppliedQuery(t *testing.T) {
	a := NewApp(nil, nil)
	a.home = filteredHome(t)
	a.home.Filter.Activate()
	a.home.Filter.Input.SetValue("alpha")
	home := a.newHomeModel()
	home.BuildTableRows()
	if !home.Filter.Applied() || home.TextEntryActive() || home.Filter.Query() != "alpha" || len(home.BoardSessions()) != 2 {
		t.Fatal("rebuild lost applied query")
	}
}
