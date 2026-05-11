package views

import (
	"strings"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestChatPickerBuildTableRows_ShowsDeletingStatusForMatchingChat(t *testing.T) {
	m := ChatPickerModel{
		spinner:                newStatusSpinner(),
		deletingAgentSessionID: "agent-1",
		chats: []*pb.ClaudeChat{
			{AgentSessionId: "agent-1", Title: "first", CreatedAt: timestamppb.Now()},
			{AgentSessionId: "agent-2", Title: "second", CreatedAt: timestamppb.Now()},
		},
	}

	m.buildTableRows()

	rows := m.table.Rows()
	if len(rows) != 2 {
		t.Fatalf("table rows = %d, want 2", len(rows))
	}
	if got := rows[0][4]; !strings.Contains(got, "deleting") {
		t.Fatalf("deleting chat STATUS = %q, want deleting", got)
	}
	if got := rows[0][4]; strings.Contains(got, "  deleting") {
		t.Fatalf("deleting chat STATUS = %q, want one space before deleting", got)
	}
	if got := rows[1][4]; strings.Contains(got, "deleting") {
		t.Fatalf("non-deleting chat STATUS = %q, want normal status", got)
	}
}

func TestCronListRebuildTable_ShowsDeletingStatusForMatchingJob(t *testing.T) {
	m := CronListModel{
		spinner:  newStatusSpinner(),
		running:  map[string]bool{},
		deleting: map[string]bool{"cron-1": true},
		repos: map[string]*pb.Repo{
			"repo-1": {Id: "repo-1", DisplayName: "repo"},
		},
		jobs: []*pb.CronJob{
			{Id: "cron-1", RepoId: "repo-1", Name: "first", Schedule: "0 9 * * 1-5", Enabled: true},
			{Id: "cron-2", RepoId: "repo-1", Name: "second", Schedule: "0 10 * * 1-5", Enabled: true},
		},
		table: newBossTable(nil, nil, 0),
	}

	m.rebuildTable()

	rows := m.table.Rows()
	if len(rows) != 2 {
		t.Fatalf("table rows = %d, want 2", len(rows))
	}
	if got := rows[0][7]; !strings.Contains(got, "deleting") {
		t.Fatalf("deleting cron STATUS = %q, want deleting", got)
	}
	if got := rows[1][7]; strings.Contains(got, "deleting") {
		t.Fatalf("non-deleting cron STATUS = %q, want normal status", got)
	}
}

func TestRepoListBuildTable_ShowsDeletingStatusForMatchingRepo(t *testing.T) {
	m := RepoListModel{
		spinner:        newStatusSpinner(),
		deletingRepoID: "repo-1",
		repos: []*pb.Repo{
			{Id: "repo-1", DisplayName: "first", LocalPath: "/tmp/first"},
			{Id: "repo-2", DisplayName: "second", LocalPath: "/tmp/second"},
		},
		table: newBossTable(nil, nil, 0),
	}

	m.buildTable()

	rows := m.table.Rows()
	if len(rows) != 2 {
		t.Fatalf("table rows = %d, want 2", len(rows))
	}
	if len(rows[0]) < 4 {
		t.Fatalf("repo row columns = %d, want STATUS column", len(rows[0]))
	}
	if got := rows[0][3]; !strings.Contains(got, "deleting") {
		t.Fatalf("deleting repo STATUS = %q, want deleting", got)
	}
	if got := rows[1][3]; strings.Contains(got, "deleting") {
		t.Fatalf("non-deleting repo STATUS = %q, want empty status", got)
	}
}
