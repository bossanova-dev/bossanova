package views

import (
	"fmt"
	"testing"

	"charm.land/lipgloss/v2"

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
			got:  (AccountsListModel{accounts: make([]*pb.Account, 100), width: width, height: height}).tableHeight(),
			want: clampedTableHeight(100, height, bannerOverhead+1+actionBarPadY+actionBarLineCount(width,
				[]string{"[e/enter]dit", "[a]dd", "[t]est", "[r]efresh", "[space] toggle", "[d] remove"},
				[]string{"[esc] back"},
			)),
		},
		{
			name: "cron jobs",
			got:  (CronListModel{jobs: make([]*pb.CronJob, 100), width: width, height: height}).tableHeight(),
			want: clampedTableHeight(100, height, bannerOverhead+1+actionBarPadY+actionBarLineCount(width,
				[]string{"[n]ew", "[e/enter]dit", "[d]elete", "[space] toggle", "[r]un now"},
				[]string{"[esc] back"},
			)),
		},
		{
			name: "repositories",
			got:  (RepoListModel{repos: make([]*pb.Repo, 100), width: width, height: height}).tableHeight(),
			want: clampedTableHeight(100, height, bannerOverhead+1+actionBarPadY+actionBarLineCount(width,
				[]string{"[enter] settings", "[d]elete"},
				[]string{"[a]dd"},
				[]string{"[esc] back"},
			)),
		},
		{
			name: "trash",
			got:  (TrashModel{filteredSessions: make([]int, 100), width: width, height: height}).tableHeight(),
			want: clampedTableHeight(100, height, bannerOverhead+1+actionBarPadY+actionBarLineCount(width,
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
		sessions: make([]*pb.Session, 100),
		width:    40,
		height:   24,
	}
	if lines := home.sessionTableFooterLineCount(); lines < 2 {
		t.Fatalf("sessionTableFooterLineCount() = %d, want a folded footer", lines)
	}

	want := clampedTableHeight(home.tableDataRowCount(), home.height,
		bannerOverhead+1+actionBarPadY+home.sessionTableFooterLineCount())
	if got := home.tableHeight(); got != want {
		t.Fatalf("tableHeight() = %d, want %d", got, want)
	}
}

func TestChatPickerTableHeightReservesFoldedFooter(t *testing.T) {
	chats := make([]*pb.AgentChat, 100)
	for i := range chats {
		chats[i] = &pb.AgentChat{AgentSessionId: "stopped"}
	}
	picker := ChatPickerModel{
		chats:           chats,
		daemonStatuses:  map[string]string{"stopped": statusStopped},
		newTabSupported: true,
		sessionID:       "session",
		width:           72,
		height:          24,
	}
	left, middle, back := picker.chatListActionGroups()
	footerLines := actionBarLineCount(picker.width, left, middle, back)
	if footerLines < 2 {
		t.Fatalf("chat footer line count = %d, want folded action bar", footerLines)
	}

	want := clampedTableHeight(len(chats), picker.height,
		bannerOverhead+1+actionBarPadY+footerLines)
	if got := picker.tableHeight(); got != want {
		t.Fatalf("tableHeight() = %d, want %d", got, want)
	}
}

func TestPickerTableHeightsReserveActiveFilterFooter(t *testing.T) {
	filter := listFilter{active: true}
	const (
		width  = 40
		height = 24
	)
	footerLines := prSelectActionBarLineCount(width, filter, true)
	if footerLines < 2 {
		t.Fatalf("filter footer line count = %d, want folded action bar", footerLines)
	}

	model := NewSessionModel{
		prsFiltered:    make([]int, 100),
		issuesFiltered: make([]int, 100),
		prFilter:       filter,
		issueFilter:    filter,
		width:          width,
		height:         height,
	}
	want := clampedTableHeight(100, height, bannerOverhead+5+footerLines+filter.Height())
	if got := model.prTableHeight(); got != want {
		t.Fatalf("prTableHeight() = %d, want %d", got, want)
	}
	if got := model.issueTableHeight(); got != want {
		t.Fatalf("issueTableHeight() = %d, want %d", got, want)
	}
}

func TestRepositoryPickerTableHeightReservesFoldedFooter(t *testing.T) {
	const (
		width  = 20
		height = 24
	)
	footerLines := actionBarLineCount(width, []string{"[enter] select"}, []string{"[esc] back"})
	if footerLines < 2 {
		t.Fatalf("repository picker footer line count = %d, want folded action bar", footerLines)
	}
	model := NewSessionModel{repos: make([]*pb.Repo, 100), width: width, height: height}
	want := clampedTableHeight(len(model.repos), height, bannerOverhead+5+footerLines)
	if got := model.repoTableHeight(); got != want {
		t.Fatalf("repoTableHeight() = %d, want %d", got, want)
	}
}

func TestHomeTableHeightReservesFilter(t *testing.T) {
	for _, applied := range []bool{false, true} {
		h := filteredHome(t)
		h.width, h.height = 40, 24
		h.sessions = nil
		for i := range 50 {
			h.sessions = append(h.sessions, &pb.Session{Id: fmt.Sprint(i), Title: "alpha"})
		}
		h.filter.Activate()
		h.filter.input.SetValue("alpha")
		if applied {
			h.filter.Commit()
		}
		h.buildTableRows()
		if got := bannerOverhead + lipgloss.Height(h.View().Content); got > h.height {
			t.Fatalf("filtered Home renders %d rows in %d-row terminal", got, h.height)
		}
	}
}
