package bossmcp

import pb "github.com/recurser/bossalib/gen/bossanova/v1"

// sendBroadcastOutput is send_broadcast's structured result: the created
// broadcast with its SECRET body already scrubbed, the frozen per-target
// delivery rows, and the resolved target count (zero is a successful send to
// nobody, which is worth reporting explicitly rather than leaving the caller to
// count an empty list).
type sendBroadcastOutput struct {
	Broadcast   *pb.Broadcast           `json:"broadcast"`
	Deliveries  []*pb.BroadcastDelivery `json:"deliveries"`
	TargetCount int                     `json:"target_count"`
}

// SendBroadcastArgs is the typed argument struct for send_broadcast. The
// message is required and is a secret — it is stored verbatim and never
// returned by any tool.
type SendBroadcastArgs struct {
	To            string `json:"to" jsonschema:"the audience selector, e.g. repo:<id>,agent:claude+account:<id>; valid keys are chat, session, repo, agent, account and daemon. Required: an empty selector is rejected, never read as everyone"`
	Message       string `json:"message" jsonschema:"the prompt body delivered to every resolved target (required; stored verbatim and never echoed back)"`
	From          string `json:"from,omitempty" jsonschema:"the originating chat id (agent_session_id); empty = operator-issued with no origin"`
	ExpiresIn     string `json:"expires_in,omitempty" jsonschema:"how long delivery keeps being retried, as a duration like 30m, 24h, 7d, 2w; default 24h, maximum 30d"`
	IncludeOrigin bool   `json:"include_origin,omitempty" jsonschema:"deliver to the origin chat too when the selector matches it; off by default so a coordinator broadcasting to its own repo does not wake itself"`
	CrossDaemon   bool   `json:"cross_daemon,omitempty" jsonschema:"ask bosso to route this broadcast to OTHER daemons as well as resolving against this daemon's chats; off by default because a cross-daemon send wakes chats on machines you are not looking at. When set, bosso fans the broadcast out to the tenant's other live daemons and each one re-resolves the selector against its OWN chats. Delivery is best-effort: a daemon offline at fan-out time never receives it and bosso holds no store-and-forward queue, and a fan-out reaching more than 32 other daemons is REFUSED rather than truncated. Pair it with a repo:/agent:/chat: selector — naming another daemon in the selector is not an equivalent opt-in, because chat rows carry an empty daemon id, so a daemon:<other-id> term resolves to zero targets on every daemon"`
}

// RegisterBroadcastSubscriptionArgs is the typed argument struct for
// register_broadcast_subscription. The message is required and is a secret — it
// is stored verbatim and never returned by any tool.
type RegisterBroadcastSubscriptionArgs struct {
	On        string `json:"on" jsonschema:"the session outcome to wait for; one of completed, errored, settled (either)"`
	To        string `json:"to" jsonschema:"the audience selector resolved at FIRE time, e.g. repo:<id>,agent:claude+account:<id>; valid keys are chat, session, repo, agent, account and daemon. Required: an empty selector is rejected, never read as everyone"`
	Message   string `json:"message" jsonschema:"the prompt body broadcast when the subscription fires (required; stored verbatim and never echoed back)"`
	Session   string `json:"session" jsonschema:"the id of the session whose outcome fires this subscription (required)"`
	From      string `json:"from,omitempty" jsonschema:"the registering chat id (agent_session_id); provenance only — it never narrows the audience"`
	ExpiresIn string `json:"expires_in,omitempty" jsonschema:"how long the rule stands, as a duration like 30m, 24h, 7d, 2w; default 24h, maximum 30d"`
}

// CreateNoteArgs is the typed argument struct for create_note. repo_id and body
// are required; session_id and chat_id are provenance only. Optional fields are
// left UNSET when omitted rather than sent blank.
type CreateNoteArgs struct {
	RepoID         string   `json:"repo_id" jsonschema:"the repo the note belongs to (required); notes are repo-scoped. Use the daemon-local repo id list_repos/resolve_context return, NOT a git origin URL — an origin URL resolves to NotFound"`
	SessionID      string   `json:"session_id,omitempty" jsonschema:"the session recording the note (provenance only; the note outlives the session)"`
	ChatID         string   `json:"chat_id,omitempty" jsonschema:"the chat recording the note (provenance only)"`
	Body           string   `json:"body" jsonschema:"the note text (required); stored verbatim, must be non-empty and at most 64 KiB"`
	Tags           []string `json:"tags,omitempty" jsonschema:"tags to file the note under; trimmed, lowercased and de-duplicated on write, at most 32 of at most 64 bytes each"`
	IdempotencyKey string   `json:"idempotency_key,omitempty" jsonschema:"optional repo-scoped key for an atomic idempotent create; retry with the same key returns the original note unchanged"`
}

// UpdateNoteArgs is the typed argument struct for update_note. Body and Tags
// are POINTERS on purpose: UpdateNoteRequest.body and .tags are optional, and a
// SET tags list replaces the entire tag set (clearing it when empty) while an
// unset one leaves the stored tags alone. A plain []string cannot express that
// difference, so flattening either field would silently wipe tags on a
// body-only edit.
type UpdateNoteArgs struct {
	RepoID string    `json:"repo_id" jsonschema:"the note's owning repo id (required, even on a local daemon that ignores it; the hosted gateway routes by it). It routes but does NOT scope: the id alone selects the note, and a mismatched repo_id is not checked, so this is not a safety check"`
	ID     string    `json:"id" jsonschema:"the note id to update (required)"`
	Body   *string   `json:"body,omitempty" jsonschema:"replacement note text; omit to leave the body unchanged"`
	Tags   *[]string `json:"tags,omitempty" jsonschema:"REPLACES the note's entire tag set — it does not append. Supply an empty list to clear every tag, or omit the field entirely to leave the existing tags unchanged"`
}

// RegisterGitHubCallbackArgs is the typed argument struct for
// register_github_callback. The message is required and is a secret — it is
// stored verbatim and never returned by any tool.
type RegisterGitHubCallbackArgs struct {
	PR                      string `json:"pr" jsonschema:"the PR to watch: a bare number like 123 (needs repo) or a full github.com PR URL"`
	Trigger                 string `json:"trigger" jsonschema:"PR event: merged, closed, checks_passed, checks_failed, ready_for_review, checks_passed_ready. State-based unless should_require_transition"`
	TargetChatID            string `json:"target_chat_id" jsonschema:"the agent-session (chat) id to deliver the message to when the callback fires"`
	Message                 string `json:"message" jsonschema:"the prompt delivered to the chat when the callback fires (required)"`
	Repo                    string `json:"repo,omitempty" jsonschema:"repository as owner/repo; required to anchor a bare PR number, ignored when pr is a full URL"`
	ExpiresIn               string `json:"expires_in,omitempty" jsonschema:"expiry as a duration: 24h, 7d, 2w"`
	Group                   string `json:"group,omitempty" jsonschema:"optional group id; siblings in ONE group cancel each other on first fire — two groups of one never do"`
	ShouldRequireTransition bool   `json:"should_require_transition,omitempty" jsonschema:"fire only after trigger becomes true"`
	IndependentWatch        bool   `json:"independent_watch,omitempty" jsonschema:"set ONLY when this watch is meant to outlive any sibling; silences notice_text but never changes firing"`
}

// registerGitHubCallbackOutput is register_github_callback's structured result:
// the created callback with its SECRET body already scrubbed, plus the daemon's
// non-fatal advisory. notice_text is empty for an ordinary create and set when
// a mutually exclusive callback is already live for this chat and PR under a
// different group — a shape where neither leg will cancel the other. Returning
// the bare callback here would make the field silently dead with every existing
// test still green.
type registerGitHubCallbackOutput struct {
	Callback   *pb.GitHubCallback `json:"callback"`
	NoticeText string             `json:"notice_text,omitempty"`
}

// RegisterRepoArgs is the typed argument struct for register_repo.
type RegisterRepoArgs struct {
	LocalPath         string `json:"local_path" jsonschema:"local filesystem path of the git repo"`
	Name              string `json:"name,omitempty" jsonschema:"display name for the repo"`
	SetupScript       string `json:"setup_script,omitempty" jsonschema:"optional setup script to run in new worktrees"`
	DefaultBaseBranch string `json:"default_base_branch,omitempty" jsonschema:"default base branch for sessions"`
}

// CloneAndRegisterRepoArgs is the typed argument struct for clone_and_register_repo.
type CloneAndRegisterRepoArgs struct {
	CloneURL          string `json:"clone_url" jsonschema:"git clone URL"`
	LocalPath         string `json:"local_path" jsonschema:"local filesystem path to clone into"`
	Name              string `json:"name,omitempty" jsonschema:"display name for the repo"`
	DefaultBaseBranch string `json:"default_base_branch,omitempty" jsonschema:"default base branch for sessions"`
	SetupScript       string `json:"setup_script,omitempty" jsonschema:"optional setup script to run in new worktrees"`
}

// UpdateRepoArgs is the typed argument struct for update_repo. Optional pointer
// fields are only applied when present in the request.
type UpdateRepoArgs struct {
	ID                        string  `json:"id" jsonschema:"the repo id"`
	Name                      *string `json:"name,omitempty" jsonschema:"new display name"`
	MergeStrategy             *string `json:"merge_strategy,omitempty" jsonschema:"merge, squash or rebase"`
	SetupScript               *string `json:"setup_script,omitempty" jsonschema:"setup script to run in new worktrees"`
	CanAutoMerge              *bool   `json:"can_auto_merge,omitempty" jsonschema:"mark a passing draft PR ready for review (does not merge)"`
	CanAutoMergeDependabot    *bool   `json:"can_auto_merge_dependabot,omitempty" jsonschema:"allow auto-merge for dependabot PRs"`
	CanAutoRepair             *bool   `json:"can_auto_repair,omitempty" jsonschema:"auto-repair PRs (failing checks, conflicts, review feedback)"`
	ShouldKeepBranchesCurrent *bool   `json:"should_keep_branches_current,omitempty" jsonschema:"rebase in-flight session branches when the base advances (force-push with lease); opt-in, each rebase re-runs CI"`
	LinearAPIKey              *string `json:"linear_api_key,omitempty" jsonschema:"Linear API key for this repo; write-only, never returned"`
	SentryAPIKey              *string `json:"sentry_api_key,omitempty" jsonschema:"Sentry auth token for this repo; write-only, never returned"`
	SentryOrg                 *string `json:"sentry_org,omitempty" jsonschema:"Sentry organization slug (issues are listed org-wide)"`
	WorktreeBaseDir           *string `json:"worktree_base_dir,omitempty" jsonschema:"absolute dir new session worktrees are created under"`
}

// createSessionOutput is the create_session tool's structured result. The
// created (or attached) session's fields are flattened at the top level (so the
// primary agent_session_id stays directly reachable) alongside attached_existing
// and, when the daemon attached to a pre-existing session, a Note explaining the
// supplied prompt was not run.
type createSessionOutput struct {
	*pb.Session
	AttachedExisting bool `json:"attached_existing"`
	// AgentLaunched is true only when THIS create actually started an agent (a
	// genuine create whose session carries a non-empty agent_session_id). It
	// reads false on the idle paths (attended:true, or a prompt-less create) and
	// on the dedup-attach path (attached_existing:true — the daemon attached to a
	// pre-existing session and did NOT run the supplied prompt, so this create
	// launched nothing), so a programmatic caller can detect the previously
	// silent no-agent outcome (BOS-498). On the attach path the Note, not this
	// field, points at the reachable pre-existing chat.
	AgentLaunched bool   `json:"agent_launched"`
	Note          string `json:"note,omitempty"`
	// NextAction is a hint set only on the idle no-agent path telling the caller
	// how to actually start work (re-create detached, or start_chat).
	NextAction string `json:"next_action,omitempty"`
}

// sendChatMessageOutput keeps the raw daemon response flattened while adding
// enum legends that cannot be omitted for zero values. Embedding is safe here
// because SendChatMessageResponse has no MarshalJSON and no top-level JSON
// field collides with the added names. Keep this as jsonResult-only payload:
// registering it as structured output would add a schema to the measured MCP
// tool surface.
type sendChatMessageOutput struct {
	*pb.SendChatMessageResponse
	TurnStartStateName string `json:"turn_start_state_name"`
	DeliveryStateName  string `json:"delivery_state_name"`
}

// CreateSessionArgs is the typed argument struct for create_session. The full
// CreateSessionRequest surface is exposed except tracker_issue, the composite
// message used only for web server-side plan formatting (intentionally omitted).
type CreateSessionArgs struct {
	RepoID           string  `json:"repo_id" jsonschema:"the repo id to create the session under"`
	Prompt           string  `json:"prompt" jsonschema:"the plan/prompt for the session"`
	Title            string  `json:"title,omitempty" jsonschema:"session title; auto-derived from the first line of the prompt when omitted"`
	Agent            string  `json:"agent,omitempty" jsonschema:"agent runner plugin name (empty = server default)"`
	Account          *string `json:"account,omitempty" jsonschema:"Account id or label to run this session under; empty = system default"`
	BaseBranch       string  `json:"base_branch,omitempty" jsonschema:"base branch to create the session from (empty = repo default)"`
	BranchName       *string `json:"branch_name,omitempty" jsonschema:"explicit branch name (e.g. a tracker's suggested branch). If an active session already owns it, create_session attaches because two active sessions cannot share one branch"`
	ForceBranch      bool    `json:"force_branch,omitempty" jsonschema:"remove any existing branch with the same name before creating"`
	Force            bool    `json:"force,omitempty" jsonschema:"bypass tracker-issue dedup and create a second session for a tracker/PR/branch that already has an active one"`
	IsQuickChat      bool    `json:"quick_chat,omitempty" jsonschema:"quick chat session: no worktree, branch, or PR"`
	Detach           bool    `json:"detach,omitempty" jsonschema:"run the initial agent pass headlessly (claude --print / codex exec) instead of leaving the session idle until attach; set true for unattended orchestration"`
	Attended         bool    `json:"attended,omitempty" jsonschema:"create the session idle, launching no agent, for a human to boss attach and drive; without it a prompt-carrying create runs headless"`
	IsTmuxUnattended bool    `json:"tmux_unattended,omitempty" jsonschema:"run the session in a durable tmux-hosted pane that survives a daemon restart and is attach-safe (used by /boss-epic); a distinct autonomous-unattended path from detach's headless runs"`
	DeferPR          bool    `json:"defer_pr,omitempty" jsonschema:"create a worktree-backed session but do NOT open a draft PR up front; a PR is opened at finalize only if the run produces commits. Meaningful only alongside detach/tmux_unattended (which install the finalize hook); use for read-only/planning sessions"`
	Model            string  `json:"model,omitempty" jsonschema:"model id; empty=default"`
	Effort           string  `json:"effort,omitempty" jsonschema:"reasoning effort; empty=default"`
	PRNumber         *int32  `json:"pr_number,omitempty" jsonschema:"target an existing PR. If an active session already owns that PR's branch, create_session ATTACHES to it and returns attached_existing=true WITHOUT running prompt — run the prompt in that session (send_chat_message with the returned agent_session_id). force does NOT bypass this branch attach"`
	TrackerID        *string `json:"tracker_id,omitempty" jsonschema:"external issue id (e.g. FRE-1176). If an active session already owns this tracker id (with no branch collision), create_session fails with AlreadyExists — pass force:true to create a second session for the same tracker"`
	TrackerURL       *string `json:"tracker_url,omitempty" jsonschema:"URL to the issue in the external tracker"`
	TrackerSource    *string `json:"tracker_source,omitempty" jsonschema:"tracker source: linear or sentry"`
}

// UpdateSessionArgs is the typed argument struct for update_session.
type UpdateSessionArgs struct {
	ID         string  `json:"id" jsonschema:"the id of the session to update"`
	Title      *string `json:"title,omitempty" jsonschema:"the new session title (also best-effort renames the linked GitHub PR)"`
	TrackerURL *string `json:"tracker_url,omitempty" jsonschema:"URL to the issue in the external tracker (e.g. a Linear ticket); makes the TUI [l]inear shortcut open it"`
	TrackerID  *string `json:"tracker_id,omitempty" jsonschema:"external tracker identifier (e.g. BOS-123)"`
}

// LinkSessionPRArgs is the typed argument struct for link_session_pr.
type LinkSessionPRArgs struct {
	SessionID string `json:"session_id" jsonschema:"the id of the session to attach the PR to"`
	PR        string `json:"pr" jsonschema:"an existing pull request number or URL (create it first, e.g. with gh pr create)"`
}

// RefreshSessionPRArgs is the typed argument struct for refresh_session_pr.
type RefreshSessionPRArgs struct {
	SessionID string `json:"session_id,omitempty" jsonschema:"session id to refresh; optional with pr_number"`
	PRNumber  *int32 `json:"pr_number,omitempty" jsonschema:"pull request number to refresh; optional with session_id"`
}

// StartChatArgs is the typed argument struct for start_chat. The handler mints
// the agent_session_id itself, so callers supply only the target session and
// optional chat metadata.
type StartChatArgs struct {
	SessionID string `json:"session_id" jsonschema:"the existing session to start the new chat in"`
	Title     string `json:"title,omitempty" jsonschema:"chat title (optional; a default is derived when omitted)"`
	AgentName string `json:"agent_name,omitempty" jsonschema:"agent runner plugin name; empty inherits the session's agent"`
}

// RecordChatArgs is the typed argument struct for record_chat.
type RecordChatArgs struct {
	SessionID      string `json:"session_id" jsonschema:"the session id"`
	AgentSessionID string `json:"agent_session_id" jsonschema:"the agent session UUID"`
	Title          string `json:"title,omitempty" jsonschema:"chat title"`
	AgentName      string `json:"agent_name,omitempty" jsonschema:"agent runner plugin name"`
	Resume         bool   `json:"resume,omitempty" jsonschema:"whether this resumes an existing chat"`
}

// UpdateChatTitleArgs is the typed argument struct for update_chat_title.
type UpdateChatTitleArgs struct {
	AgentSessionID string `json:"agent_session_id" jsonschema:"the agent session UUID"`
	Title          string `json:"title" jsonschema:"new chat title"`
}

// WakeChatArgs is the typed argument struct for wake_chat.
type WakeChatArgs struct {
	SessionID      string `json:"session_id" jsonschema:"the session id (required for remote authz)"`
	AgentSessionID string `json:"agent_session_id" jsonschema:"the agent session UUID"`
	ForceFresh     bool   `json:"force_fresh,omitempty" jsonschema:"force a fresh chat instead of resuming"`
}

// ReportChatStatusArgs is the typed argument struct for report_chat_status.
type ReportChatStatusArgs struct {
	AgentSessionID string `json:"agent_session_id" jsonschema:"the agent session UUID"`
	Status         string `json:"status" jsonschema:"chat status enum name (e.g. CHAT_STATUS_WORKING)"`
}

// CreateCronJobArgs is the typed argument struct for create_cron_job.
type CreateCronJobArgs struct {
	RepoID                string `json:"repo_id" jsonschema:"the repo id"`
	Name                  string `json:"name" jsonschema:"cron job name"`
	Prompt                string `json:"prompt" jsonschema:"the prompt to run each fire"`
	Schedule              string `json:"schedule" jsonschema:"cron schedule expression"`
	Timezone              string `json:"timezone,omitempty" jsonschema:"IANA timezone (empty = daemon-local)"`
	IsEnabled             bool   `json:"enabled,omitempty" jsonschema:"whether the job is enabled"`
	AgentName             string `json:"agent_name,omitempty" jsonschema:"agent runner plugin name (empty = claude)"`
	Model                 string `json:"model,omitempty" jsonschema:"opaque agent model id (empty = plugin default)"`
	GateCommand           string `json:"gate_command,omitempty" jsonschema:"command run before each fire; non-zero exit skips the run, empty = no gate"`
	ShouldRunSetupCommand *bool  `json:"run_setup_command,omitempty" jsonschema:"run the repo setup script before the agent; omitted = server default"`
	IsZeroOutput          *bool  `json:"zero_output,omitempty" jsonschema:"run with no worktree, branch, or PR; for jobs expected to change nothing in the repo. omitted = false"`
	ConcurrencyPolicy     string `json:"concurrency_policy,omitempty" jsonschema:"what a fire does while the previous run is still working: skip (default), cancel_in_progress, or allow_concurrent"`
}

// SendChatMessageArgs is the typed argument struct for send_chat_message.
type SendChatMessageArgs struct {
	AgentSessionID string `json:"agent_session_id" jsonschema:"the agent session UUID"`
	Message        string `json:"message" jsonschema:"the user message to deliver"`
	WakeIfAsleep   *bool  `json:"wake_if_asleep,omitempty" jsonschema:"wake the agent if it is currently asleep; defaults to true when omitted"`
	Submit         *bool  `json:"submit,omitempty" jsonschema:"press Enter and verify the submission; defaults to true; pass false to only prefill the composer"`
}

// SwitchAgentAccountArgs is the typed argument struct for switch_account.
type SwitchAgentAccountArgs struct {
	SessionID      string `json:"session_id" jsonschema:"the session whose live chat to switch"`
	AccountID      string `json:"account_id" jsonschema:"account id or provider-scoped label (unique within the session's provider); empty for system default"`
	AgentSessionID string `json:"agent_session_id,omitempty" jsonschema:"optional specific chat; defaults to the session's primary live chat"`
	Force          bool   `json:"force,omitempty" jsonschema:"interrupt a mid-turn chat"`
}

// UpdateCronJobArgs is the typed argument struct for update_cron_job. Optional
// pointer fields are only applied when present.
type UpdateCronJobArgs struct {
	ID                    string  `json:"id" jsonschema:"the cron job id"`
	Name                  *string `json:"name,omitempty" jsonschema:"new name"`
	Prompt                *string `json:"prompt,omitempty" jsonschema:"new prompt"`
	Schedule              *string `json:"schedule,omitempty" jsonschema:"new cron schedule expression"`
	Timezone              *string `json:"timezone,omitempty" jsonschema:"new IANA timezone"`
	IsEnabled             *bool   `json:"enabled,omitempty" jsonschema:"enable or disable the job"`
	AgentName             *string `json:"agent_name,omitempty" jsonschema:"new agent runner plugin name"`
	Model                 *string `json:"model,omitempty" jsonschema:"new opaque agent model id (empty = plugin default)"`
	GateCommand           *string `json:"gate_command,omitempty" jsonschema:"new gate command; empty = no gate"`
	ShouldRunSetupCommand *bool   `json:"run_setup_command,omitempty" jsonschema:"run the repo setup script before the agent"`
	IsZeroOutput          *bool   `json:"zero_output,omitempty" jsonschema:"run with no worktree, branch, or PR; for jobs expected to change nothing in the repo"`
	ConcurrencyPolicy     string  `json:"concurrency_policy,omitempty" jsonschema:"skip, cancel_in_progress, or allow_concurrent"`
}

// AddAgentAccountArgs is the typed argument struct for add_account. It maps 1:1 onto
// pb.AddAgentAccountRequest. The credential is inbound only — no response ever
// returns it.
type AddAgentAccountArgs struct {
	Provider   string `json:"provider" jsonschema:"account provider (claude|codex)"`
	Label      string `json:"label" jsonschema:"human label, unique per provider"`
	Priority   int32  `json:"priority,omitempty" jsonschema:"sort order; lower = preferred"`
	Credential string `json:"credential,omitempty" jsonschema:"credential blob (Claude setup-token string or Codex auth.json contents); stored in the keyring, never returned"`
}

// RefreshAgentAccountArgs is the typed argument struct for refresh_account. It maps
// 1:1 onto pb.RefreshAgentAccountRequest. The credential is inbound only — no
// response ever returns it.
type RefreshAgentAccountArgs struct {
	ID            string `json:"id" jsonschema:"the account id"`
	Credential    string `json:"credential,omitempty" jsonschema:"new credential blob; stored in the keyring, never returned"`
	TestAfterSave bool   `json:"test_after_save,omitempty" jsonschema:"validate the refreshed credential after saving"`
}

// UpdateAgentAccountArgs is the typed argument struct for update_account. Optional
// pointer fields are only applied when present; allowed_models replaces the set
// when non-empty. It maps 1:1 onto pb.UpdateAgentAccountRequest.
type UpdateAgentAccountArgs struct {
	ID            string   `json:"id" jsonschema:"the account id"`
	Label         *string  `json:"label,omitempty" jsonschema:"new label"`
	Priority      *int32   `json:"priority,omitempty" jsonschema:"new priority (lower = preferred)"`
	Status        *string  `json:"status,omitempty" jsonschema:"new status (active|disabled)"`
	AllowedModels []string `json:"allowed_models,omitempty" jsonschema:"replace the allowed-models set when non-empty"`
}

// UpdateSettingsArgs is the typed argument struct for update_settings. Optional
// pointer fields are applied only when present (partial update); agents carries
// per-agent enabled/config updates. It maps 1:1 onto pb.UpdateSettingsRequest.
type UpdateSettingsArgs struct {
	WorktreeBaseDir      *string                   `json:"worktree_base_dir,omitempty" jsonschema:"new worktree base directory; must be a non-empty path that exists on disk"`
	PollIntervalSeconds  *int32                    `json:"poll_interval_seconds,omitempty" jsonschema:"PR display poll interval in seconds; must be >= 0 (0 clears it, applying the 2-minute default)"`
	DefaultAgent         *string                   `json:"default_agent,omitempty" jsonschema:"default agent runner; must be the name of a currently-enabled agent"`
	EventTracingEnabled  *bool                     `json:"event_tracing_enabled,omitempty" jsonschema:"enable PostHog event tracing"`
	ErrorTrackingEnabled *bool                     `json:"error_tracking_enabled,omitempty" jsonschema:"enable Sentry error tracking"`
	PostHogProjectToken  *string                   `json:"posthog_project_token,omitempty" jsonschema:"PostHog project (ingestion) token"`
	PostHogHost          *string                   `json:"posthog_host,omitempty" jsonschema:"PostHog host; empty falls back to the first-party default when tracing is enabled"`
	Agents               []AgentSettingsUpdateArgs `json:"agents,omitempty" jsonschema:"per-agent partial updates"`
}

// AgentSettingsUpdateArgs is a single per-agent update within update_settings.
// enabled is applied only when present; each config entry is an upsert where an
// empty value deletes the key. Enum values are validated against the agent's
// advertised allowed values (see list_agents).
type AgentSettingsUpdateArgs struct {
	Name    string            `json:"name" jsonschema:"the agent/plugin name (e.g. claude, codex)"`
	Enabled *bool             `json:"enabled,omitempty" jsonschema:"enable or disable this agent"`
	Config  map[string]string `json:"config,omitempty" jsonschema:"per-agent config keys to upsert; an empty value deletes the key (bool keys use \"true\"/\"false\")"`
}
