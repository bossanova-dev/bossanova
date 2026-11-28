package clidoc

// bossShowLong is the extended prose for `boss show`, kept beside the main
// registry so registry.go stays inside its file-size budget.
const bossShowLong = "Shows one session's details, then its chats in the same table `boss chats` " +
	"prints -- ID, TITLE, CREATED, STATUS and LAST OUTPUT, with the same reading of " +
	"UNSPECIFIED and of last_output_at. If the status read fails the rows still " +
	"print, STATUS and LAST OUTPUT read `?`, one stderr line explains why, and the " +
	"command exits 0.\n\n" +
	"`--json` emits `{\"session\": {...}}`: every field of a `boss ls " +
	"--json` row, plus the detail the text rendering prints — " +
	"`repo_display_name`, `repo_should_archive_sessions_after_merge`, " +
	"`base_branch`, `worktree_path`, `account_id`, " +
	"`account_label`, `display_status`, `display_label`, `last_check_state`, " +
	"`pr_mergeable`, `merge_block`, `attention_status`, " +
	"`archived_at`, `setup_error`, `last_repair` (an object, or " +
	"`null` when the repair plugin has never run for this session), " +
	"`repair_active` and `repair_queued_at` (RFC3339 timestamp when " +
	"waiting for a repair slot, empty when absent). " +
	"`repo_should_archive_sessions_after_merge` is a boolean, or `null` " +
	"when repository hydration failed. " +
	"`display_status` uses the same vocabulary as the TUI and `boss " +
	"session checks` (`idle`, `checking`, `passing`, `failing`, …), and " +
	"`display_label` carries the daemon's formatted status label verbatim " +
	"(empty when absent). `pr_mergeable` is a boolean, or `null` when " +
	"mergeability is unknown. `merge_block` is `{gate, detail, " +
	"blocking_reviewers, display_status}` with `gate` as the trimmed " +
	"enum name (`CONFLICT`, `NONE`, …) and `blocking_reviewers` always " +
	"an array, or `null` when absent. `attention_status` is " +
	"`{needs_attention, reason, summary, since}` with `reason` as the " +
	"trimmed enum name (`AGENT_AUTH_FAILED`, …) and `since` in RFC3339, " +
	"or `null` when absent. The " +
	"chats table the text rendering prints below the header is " +
	"deliberately NOT in this envelope — `boss chats` owns that shape, " +
	"because it has to join in per-chat status, so drive a machine gate " +
	"off `boss chats --json`. An unknown session id " +
	"exits 1 with the shared JSON error envelope on stdout."
