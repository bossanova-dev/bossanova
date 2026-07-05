package bossmcp

// readOnlyToolNames is the canonical list of read-only (Phase-1 read table)
// tool names, in registration order. It is the single source of truth for
// both the read-only tools/list expectation and ReadOnlyToolNames().
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
	"list_accounts",
	"get_chat_transcript",
}

// writeToolNames is the canonical union of mutating + destructive tool names,
// in registration order. None of these appear under Options{ReadOnly}.
var writeToolNames = []string{
	// mutating
	"register_repo", "clone_and_register_repo", "update_repo", "create_session",
	"stop_session", "pause_session", "resume_session", "retry_session",
	"update_session", "link_session_pr", "record_chat", "update_chat_title",
	"wake_chat", "report_chat_status", "create_cron_job", "update_cron_job",
	"run_cron_job_now", "add_account", "update_account", "test_account",
	"send_chat_message",
	// destructive
	"remove_repo", "close_session", "merge_session", "remove_session",
	"archive_session", "resurrect_session", "delete_chat", "empty_trash",
	"delete_cron_job", "remove_account",
}

// ToolNames returns every MCP tool name this package registers in full
// (non-read-only) mode, read-only tools first then write tools, each in
// registration order. It enumerates the inventory WITHOUT starting an MCP
// server, so callers such as `boss env` can report capabilities cheaply.
// The returned slice is a copy; callers may mutate it freely.
func ToolNames() []string {
	out := make([]string, 0, len(readOnlyToolNames)+len(writeToolNames))
	out = append(out, readOnlyToolNames...)
	out = append(out, writeToolNames...)
	return out
}

// ReadOnlyToolNames returns the read-only tool subset (the tools registered
// under Options{ReadOnly}), in registration order. The returned slice is a copy.
func ReadOnlyToolNames() []string {
	return append([]string{}, readOnlyToolNames...)
}

// WriteToolNames returns the mutating + destructive tool subset (the tools
// omitted under Options{ReadOnly}), in registration order. The returned slice
// is a copy.
func WriteToolNames() []string {
	return append([]string{}, writeToolNames...)
}
