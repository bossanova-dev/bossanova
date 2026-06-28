package bossmcp

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// readOnlyToolNames is the exact set of read-only tools (the Phase-1 read
// table). It doubles as the expected tools/list output under Options{ReadOnly}.
var readOnlyToolNames = []string{
	"list_sessions",
	"resolve_context",
	"validate_repo_path",
	"list_repos",
	"list_repo_prs",
	"list_tracker_issues",
	"get_session",
	"list_chats",
	"get_chat_statuses",
	"get_session_statuses",
	"list_check_snapshots",
	"repair_doctor",
	"list_agents",
	"list_plugins",
	"list_cron_jobs",
	"get_cron_job",
	"get_chat_transcript",
}

// writeToolNames is the union of mutating + destructive tools. None may appear
// under Options{ReadOnly}.
var writeToolNames = []string{
	// mutating
	"register_repo", "clone_and_register_repo", "update_repo", "create_session",
	"stop_session", "pause_session", "resume_session", "retry_session",
	"update_session", "link_session_pr", "record_chat", "update_chat_title",
	"wake_chat", "report_chat_status", "create_cron_job", "update_cron_job",
	"run_cron_job_now", "send_chat_message",
	// destructive
	"remove_repo", "close_session", "merge_session", "remove_session",
	"archive_session", "resurrect_session", "delete_chat", "empty_trash",
	"delete_cron_job",
}

func listedToolNames(t *testing.T, opts Options) map[string]bool {
	t.Helper()
	cs := newConnectedClient(t, &fakeBackend{}, opts)
	res, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	names := make(map[string]bool, len(res.Tools))
	for _, tool := range res.Tools {
		names[tool.Name] = true
	}
	return names
}

func TestReadOnlyOmitsWriteTools(t *testing.T) {
	names := listedToolNames(t, Options{ReadOnly: true})

	for _, want := range readOnlyToolNames {
		if !names[want] {
			t.Errorf("read-only mode is missing read tool %q", want)
		}
	}
	for _, bad := range writeToolNames {
		if names[bad] {
			t.Errorf("read-only mode must not register write tool %q", bad)
		}
	}
	if len(names) != len(readOnlyToolNames) {
		t.Errorf("read-only tools/list = %d tools, want %d: %v", len(names), len(readOnlyToolNames), names)
	}
}

func TestFullToolSetRegistered(t *testing.T) {
	names := listedToolNames(t, Options{})
	want := append(append([]string{}, readOnlyToolNames...), writeToolNames...)
	for _, w := range want {
		if !names[w] {
			t.Errorf("full mode is missing tool %q", w)
		}
	}
	if len(names) != len(want) {
		t.Errorf("full tools/list = %d tools, want %d: %v", len(names), len(want), names)
	}
}
