package tui

import (
	"context"
	"fmt"
	"testing"

	"github.com/recurser/boss/api/tui/repos"

	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/api/tui/accounts"
	"github.com/recurser/boss/api/tui/cron"
	"github.com/recurser/boss/api/tui/trash"
	"github.com/recurser/boss/api/tui/ui"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestTableHeightsReserveWrappedActionBars(t *testing.T) {
	const (
		width  = 72
		height = 24
	)

	tests := []struct {
		name string
		got  int
		want int
	}{
		{
			name: "accounts",
			got:  accountsTableHeightForTest(width, height),
			want: ui.ClampedTableHeight(100, height, ui.BannerOverhead+1+ui.ActionBarPadY+ui.ActionBarLineCount(width,
				[]string{"[e/enter]dit", "[a]dd", "[t]est", "[r]efresh", "[space] toggle", "[d] remove"},
				[]string{"[esc] back"},
			)),
		},
		{
			name: "cron jobs",
			got:  cronTableHeightForTest(width, height),
			want: ui.ClampedTableHeight(100, height, ui.BannerOverhead+1+ui.ActionBarPadY+ui.ActionBarLineCount(width,
				[]string{"[n]ew", "[e/enter]dit", "[d]elete", "[space] toggle", "[r]un now"},
				[]string{"[esc] back"},
			)),
		},
		{
			name: "repositories",
			got:  repoTableHeightForTest(width, height),
			want: ui.ClampedTableHeight(100, height, ui.BannerOverhead+1+ui.ActionBarPadY+ui.ActionBarLineCount(width,
				[]string{"[enter] settings", "[d]elete"},
				[]string{"[a]dd"},
				[]string{"[esc] back"},
			)),
		},
		{
			name: "trash",
			got:  trashTableHeightForTest(width, height),
			want: ui.ClampedTableHeight(100, height, ui.BannerOverhead+1+ui.ActionBarPadY+ui.ActionBarLineCount(width,
				[]string{"[d]elete", "[a] delete all", "[r]estore", "[/] search"},
				[]string{"[esc] back"},
			)),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Fatalf("tableHeight() = %d, want %d", tt.got, tt.want)
			}
		})
	}
}

func TestHomeTableHeightReservesFoldedFooter(t *testing.T) {
	home := HomeModel{
		Sessions: make([]*pb.Session, 100),
		Width:    40,
		Height:   24,
	}
	if lines := home.SessionTableFooterLineCount(); lines < 2 {
		t.Fatalf("sessionTableFooterLineCount() = %d, want a folded footer", lines)
	}

	want := ui.ClampedTableHeight(home.TableDataRowCount(), home.Height,
		ui.BannerOverhead+1+ui.ActionBarPadY+home.SessionTableFooterLineCount())
	if got := home.TableHeight(); got != want {
		t.Fatalf("tableHeight() = %d, want %d", got, want)
	}
}

func TestHomeTableHeightReservesFilter(t *testing.T) {
	for _, applied := range []bool{false, true} {
		h := filteredHome(t)
		h.Width, h.Height = 40, 24
		h.Sessions = nil
		for i := range 50 {
			h.Sessions = append(h.Sessions, &pb.Session{Id: fmt.Sprint(i), Title: "alpha"})
		}
		h.Filter.Activate()
		h.Filter.Input.SetValue("alpha")
		if applied {
			h.Filter.Commit()
		}
		h.BuildTableRows()
		if got := ui.BannerOverhead + lipgloss.Height(h.View().Content); got > h.Height {
			t.Fatalf("filtered Home renders %d rows in %d-row terminal", got, h.Height)
		}
	}
}

func cronTableHeightForTest(width, height int) int {
	m := NewCronListModel(nil, context.Background())
	m.SetSize(width, height)
	jobs := make([]*pb.CronJob, 100)
	for i := range jobs {
		jobs[i] = &pb.CronJob{Id: fmt.Sprint(i)}
	}
	v, _ := m.Update(cron.JobsLoadedMsg{Jobs: jobs})
	return v.(CronListModel).TableHeight()
}
func trashTableHeightForTest(width, height int) int {
	m := NewTrashModel(nil, context.Background())
	m.SetSize(width, height)
	sessions := make([]*pb.Session, 100)
	for i := range sessions {
		sessions[i] = &pb.Session{Id: fmt.Sprint(i)}
	}
	v, _ := m.Update(trash.ListMsg{Sessions: sessions})
	return v.(TrashModel).TableHeight()
}

func accountsTableHeightForTest(width, height int) int {
	m := accounts.NewAgentAccountsListModel(nil, context.Background())
	m.SetSize(width, height)
	m, _ = updateModel(m, accounts.AgentAccountsLoadedMsg{Accounts: make([]*pb.AgentAccount, 100)})
	return m.TableHeight()
}

func repoTableHeightForTest(w, h int) int {
	m := repos.NewRepoListModel(nil, context.Background())
	m.SetSize(w, h)
	items := make([]*pb.Repo, 100)
	for i := range items {
		items[i] = &pb.Repo{}
	}
	v, _ := m.Update(repos.RepoListLoadedMsg{Repos: items})
	return v.(repos.RepoListModel).TableHeight()
}
