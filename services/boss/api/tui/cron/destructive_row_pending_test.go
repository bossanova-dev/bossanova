package cron

import (
	"strings"
	"testing"

	"github.com/recurser/boss/api/tui/ui"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestCronListRebuildTable_ShowsDeletingStatusForMatchingJob(t *testing.T) {
	m := ListModel{
		spinner:  ui.NewStatusSpinner(),
		running:  map[string]bool{},
		deleting: map[string]bool{"cron-1": true},
		repos: map[string]*pb.Repo{
			"repo-1": {Id: "repo-1", DisplayName: "repo"},
		},
		jobs: []*pb.CronJob{
			{Id: "cron-1", RepoId: "repo-1", Name: "first", Schedule: "0 9 * * 1-5", IsEnabled: true},
			{Id: "cron-2", RepoId: "repo-1", Name: "second", Schedule: "0 10 * * 1-5", IsEnabled: true},
		},
		table: ui.NewBossTable(nil, nil, 0),
	}

	m.rebuildTable()

	rows := m.table.Rows()
	if len(rows) != 2 {
		t.Fatalf("table rows = %d, want 2", len(rows))
	}
	if got := rows[0][8]; !strings.Contains(got, "deleting") {
		t.Fatalf("deleting cron STATUS = %q, want deleting", got)
	}
	if got := rows[1][8]; strings.Contains(got, "deleting") {
		t.Fatalf("non-deleting cron STATUS = %q, want normal status", got)
	}
}
func TestCronListRebuildTable_ShowsStoredAgentName(t *testing.T) {
	m := ListModel{
		spinner:  ui.NewStatusSpinner(),
		running:  map[string]bool{},
		deleting: map[string]bool{},
		repos: map[string]*pb.Repo{
			"repo-1": {Id: "repo-1", DisplayName: "repo"},
		},
		jobs: []*pb.CronJob{
			{Id: "cron-1", RepoId: "repo-1", Name: "first", Schedule: "@daily", IsEnabled: true, AgentName: "opencode"},
		},
		table: ui.NewBossTable(nil, nil, 0),
	}

	m.rebuildTable()

	rows := m.table.Rows()
	if len(rows) != 1 {
		t.Fatalf("table rows = %d, want 1", len(rows))
	}
	if got := strings.Join(rows[0], " "); !strings.Contains(got, "opencode") {
		t.Fatalf("cron row = %q, want stored agent name", got)
	}
}
