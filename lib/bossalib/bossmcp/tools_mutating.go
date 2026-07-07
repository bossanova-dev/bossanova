package bossmcp

import (
	"context"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/protobuf/proto"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// maxDerivedTitleLen bounds titles auto-derived from a prompt so a long
// first line does not become an unwieldy session title.
const maxDerivedTitleLen = 72

// deriveSessionTitle returns the explicit title when set, otherwise derives a
// title from the first non-blank line of the prompt. bossd requires a
// non-empty title on create_session; deriving one keeps the MCP tool usable
// without forcing callers to supply a title separately.
func deriveSessionTitle(title, prompt string) string {
	if t := strings.TrimSpace(title); t != "" {
		return t
	}
	for line := range strings.SplitSeq(prompt, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if r := []rune(line); len(r) > maxDerivedTitleLen {
			return string(r[:maxDerivedTitleLen])
		}
		return line
	}
	return ""
}

// registerMutatingTools installs the non-destructive write tools. Idempotent
// operations (pause/resume and the update_* family) set IdempotentHint.
func registerMutatingTools(server *mcp.Server, backend Backend, opts Options) {
	addTool(server, opts, &mcp.Tool{
		Name:        "register_repo",
		Description: "Register an existing local git repository with bossanova.",
		Annotations: &mcp.ToolAnnotations{},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args RegisterRepoArgs) (*mcp.CallToolResult, any, error) {
		req := &pb.RegisterRepoRequest{
			LocalPath:         args.LocalPath,
			DisplayName:       args.Name,
			DefaultBaseBranch: args.DefaultBaseBranch,
		}
		if args.SetupScript != "" {
			req.SetupScript = &args.SetupScript
		}
		out, err := backend.RegisterRepo(ctx, req)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(redactRepo(out))
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "clone_and_register_repo",
		Description: "Clone a remote git repository and register it with bossanova.",
		Annotations: &mcp.ToolAnnotations{},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args CloneAndRegisterRepoArgs) (*mcp.CallToolResult, any, error) {
		req := &pb.CloneAndRegisterRepoRequest{
			CloneUrl:          args.CloneURL,
			LocalPath:         args.LocalPath,
			DisplayName:       args.Name,
			DefaultBaseBranch: args.DefaultBaseBranch,
		}
		if args.SetupScript != "" {
			req.SetupScript = &args.SetupScript
		}
		out, err := backend.CloneAndRegisterRepo(ctx, req)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(redactRepo(out))
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "update_repo",
		Description: "Update settings on a registered repository.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args UpdateRepoArgs) (*mcp.CallToolResult, any, error) {
		req := &pb.UpdateRepoRequest{
			Id:                     args.ID,
			DisplayName:            args.Name,
			MergeStrategy:          args.MergeStrategy,
			SetupScript:            args.SetupScript,
			CanAutoMerge:           args.CanAutoMerge,
			CanAutoMergeDependabot: args.CanAutoMergeDependabot,
			CanAutoRepair:          args.CanAutoRepair,
			// Optional *string args map straight through; nil stays unset.
			LinearApiKey: args.LinearAPIKey,
			SentryApiKey: args.SentryAPIKey,
			SentryOrg:    args.SentryOrg,
		}
		out, err := backend.UpdateRepo(ctx, req)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(redactRepo(out))
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "create_session",
		Description: "Create a new bossanova session for a repo with a prompt; drains setup and returns the final session (its agent_session_id is the primary chat id — no sqlite read needed). DEDUP: if an active session already owns the target branch or PR (via pr_number/branch_name), the daemon ATTACHES to that existing session instead of creating one — the result then has attached_existing=true and the supplied prompt is NOT run; deliver it yourself via send_chat_message with the returned agent_session_id (force does NOT bypass this branch/PR attach — two active sessions cannot share one branch). If instead an active session already owns the same tracker_id with no branch collision, the create fails with AlreadyExists; pass force:true to create a second session for that tracker. Supports running the initial agent pass headlessly (detach) or in a durable tmux-hosted pane that survives a daemon restart (tmux_unattended, used by /boss-epic) under a chosen model (model), under a specific rotation account (account, an account id or label; empty = system default), attaching to an existing PR (pr_number), quick chats (quick_chat), explicit base/branch names, and linking an external tracker issue (tracker_id/tracker_url/tracker_source). The composite tracker_issue field is web-only and not exposed here.",
		Annotations: &mcp.ToolAnnotations{},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args CreateSessionArgs) (*mcp.CallToolResult, any, error) {
		req := &pb.CreateSessionRequest{
			RepoId:      args.RepoID,
			Plan:        args.Prompt,
			Title:       deriveSessionTitle(args.Title, args.Prompt),
			BaseBranch:  args.BaseBranch,
			ForceBranch: args.ForceBranch,
			QuickChat:   args.QuickChat,
			// Force bypasses the BOS-236 tracker-issue dedup guard so a caller
			// can intentionally create a second session for a tracker/PR/branch
			// that already has an active one.
			Force: args.Force,
			// Detach runs the initial agent pass headlessly (claude --print /
			// codex exec) instead of leaving the session idle until attach —
			// what an unattended /boss-epic fan-out needs.
			Detach: args.Detach,
			// TmuxUnattended runs the session in a durable tmux-hosted pane that
			// survives a daemon restart and is attach-safe — the /boss-epic fan-out
			// path, distinct from Detach's headless runs.
			TmuxUnattended: args.TmuxUnattended,
			// Optional pointer args map straight through; nil stays unset.
			BranchName:    args.BranchName,
			PrNumber:      args.PRNumber,
			TrackerId:     args.TrackerID,
			TrackerUrl:    args.TrackerURL,
			TrackerSource: args.TrackerSource,
		}
		if args.Agent != "" {
			req.AgentName = &args.Agent
		}
		// account carries flag PRESENCE: a present-empty "" is an explicit
		// account-0 opt-out (skips the daemon's default-account policy), distinct
		// from an omitted arg (nil) which lets the policy run.
		if args.Account != nil {
			req.AccountId = args.Account
		}
		if args.Model != "" {
			req.Model = &args.Model
		}
		out, err := backend.CreateSession(ctx, req)
		if err != nil {
			return errorResult(err), nil, nil
		}
		payload := createSessionOutput{Session: out.Session, AttachedExisting: out.AttachedExisting}
		if out.AttachedExisting {
			// The daemon deduped: an active session already owned the target
			// branch/PR/tracker, so it attached instead of creating a new one and
			// the supplied prompt was NOT run. Tell the caller to deliver the
			// prompt to that session itself.
			if out.Session.GetAgentSessionId() != "" {
				payload.Note = "This branch/PR already had an active session, so create_session ATTACHED to it and did NOT run the supplied prompt. To run your prompt, send it to the existing session with send_chat_message using the returned agent_session_id. (force does not help here: whenever an active session already owns the target branch the daemon attaches — two active sessions cannot share one branch.)"
			} else {
				// An interactive session that has not been attached yet has no
				// agent chat (agent_session_id is empty), so send_chat_message
				// cannot reach it. The caller must open/attach the session first
				// to start its chat, then deliver the prompt.
				payload.Note = "This branch/PR already had an active session, so create_session ATTACHED to it and did NOT run the supplied prompt. That session has not started its agent chat yet (agent_session_id is empty), so send_chat_message cannot reach it — open/attach the session (e.g. in the boss TUI, or via wake_chat once it has an agent_session_id) to start its chat, then deliver your prompt with send_chat_message. (force does not help here: whenever an active session already owns the target branch the daemon attaches — two active sessions cannot share one branch.)"
			}
		}
		r, err := jsonResult(payload)
		return r, nil, err
	})

	registerSessionStateTool(server, opts, "stop_session", "Stop a running session.", false, backend.StopSession)
	registerSessionStateTool(server, opts, "pause_session", "Pause a session.", true, backend.PauseSession)
	registerSessionStateTool(server, opts, "resume_session", "Resume a paused session.", true, backend.ResumeSession)
	registerSessionStateTool(server, opts, "retry_session", "Retry a failed session.", false, backend.RetrySession)

	addTool(server, opts, &mcp.Tool{
		Name:        "update_session",
		Description: "Update a session: rename it (title, which also syncs the linked GitHub PR title) and/or link it to an external tracker issue (tracker_url and tracker_id, e.g. a Linear ticket). Each field is optional; supply only what you want to change. Linking a tracker_url makes the TUI [l]inear shortcut open the ticket. PR creation is a separate prior step (e.g. `gh pr create`); to attach a PR use link_session_pr.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args UpdateSessionArgs) (*mcp.CallToolResult, any, error) {
		req := &pb.UpdateSessionRequest{
			Id:         args.ID,
			Title:      args.Title,
			TrackerUrl: args.TrackerURL,
			TrackerId:  args.TrackerID,
		}
		out, err := backend.UpdateSession(ctx, req)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(out)
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "link_session_pr",
		Description: "Attach an existing pull request to a session. Create the PR first (e.g. `gh pr create`) — this does not create one. Pass the session id and the PR number or URL. Pair with update_session to also title the session.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args LinkSessionPRArgs) (*mcp.CallToolResult, any, error) {
		out, err := backend.LinkSessionPR(ctx, args.SessionID, args.PR)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(out)
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "record_chat",
		Description: "Record (or resume) an agent chat against a session.",
		Annotations: &mcp.ToolAnnotations{},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args RecordChatArgs) (*mcp.CallToolResult, any, error) {
		out, err := backend.RecordChat(ctx, args.SessionID, args.AgentSessionID, args.Title, args.AgentName, args.Resume)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(out)
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "update_chat_title",
		Description: "Update the title of an agent chat.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args UpdateChatTitleArgs) (*mcp.CallToolResult, any, error) {
		if err := backend.UpdateChatTitle(ctx, args.AgentSessionID, args.Title); err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(map[string]string{"agent_session_id": args.AgentSessionID, "title": args.Title})
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "wake_chat",
		Description: "Ask the daemon to bring a stopped chat back online.",
		Annotations: &mcp.ToolAnnotations{},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args WakeChatArgs) (*mcp.CallToolResult, any, error) {
		out, err := backend.WakeChat(ctx, args.SessionID, args.AgentSessionID, args.ForceFresh)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(out)
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "report_chat_status",
		Description: "Report the live status of an agent chat (working/idle/stopped/question).",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args ReportChatStatusArgs) (*mcp.CallToolResult, any, error) {
		report := &pb.ChatStatusReport{
			AgentSessionId: args.AgentSessionID,
			Status:         pb.ChatStatus(pb.ChatStatus_value[args.Status]),
		}
		if err := backend.ReportChatStatus(ctx, []*pb.ChatStatusReport{report}); err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(map[string]string{"agent_session_id": args.AgentSessionID, "status": args.Status})
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "create_cron_job",
		Description: "Create a scheduled cron job for a repo.",
		Annotations: &mcp.ToolAnnotations{},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args CreateCronJobArgs) (*mcp.CallToolResult, any, error) {
		req := &pb.CreateCronJobRequest{
			RepoId:      args.RepoID,
			Name:        args.Name,
			Prompt:      args.Prompt,
			Schedule:    args.Schedule,
			Timezone:    args.Timezone,
			Enabled:     args.Enabled,
			AgentName:   args.AgentName,
			Model:       args.Model,
			GateCommand: args.GateCommand,
			// *bool: nil stays unset so the server applies its own default.
			RunSetupCommand: args.RunSetupCommand,
		}
		out, err := backend.CreateCronJob(ctx, req)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(out)
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "update_cron_job",
		Description: "Update an existing cron job.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args UpdateCronJobArgs) (*mcp.CallToolResult, any, error) {
		req := &pb.UpdateCronJobRequest{
			Id:        args.ID,
			Name:      args.Name,
			Prompt:    args.Prompt,
			Schedule:  args.Schedule,
			Timezone:  args.Timezone,
			Enabled:   args.Enabled,
			AgentName: args.AgentName,
			// Optional fields map straight through; nil stays unset (present-only).
			Model:           args.Model,
			GateCommand:     args.GateCommand,
			RunSetupCommand: args.RunSetupCommand,
		}
		out, err := backend.UpdateCronJob(ctx, req)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(out)
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "run_cron_job_now",
		Description: "Fire a cron job immediately, returning the spawned session (or skip reason).",
		Annotations: &mcp.ToolAnnotations{},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args IDArgs) (*mcp.CallToolResult, any, error) {
		out, err := backend.RunCronJobNow(ctx, args.ID)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(out)
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "add_account",
		Description: "Register an agent account. The credential blob is stored in the keyring and never echoed back; the response returns metadata only.",
		Annotations: &mcp.ToolAnnotations{},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args AddAccountArgs) (*mcp.CallToolResult, any, error) {
		req := &pb.AddAccountRequest{
			Provider:   args.Provider,
			Label:      args.Label,
			Email:      args.Email,
			Priority:   args.Priority,
			Credential: []byte(args.Credential),
		}
		out, err := backend.AddAccount(ctx, req)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(out)
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "refresh_account",
		Description: "Replace an existing account credential in place. The credential is stored in the keyring and never echoed back; the response returns metadata only.",
		Annotations: &mcp.ToolAnnotations{},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args RefreshAccountArgs) (*mcp.CallToolResult, any, error) {
		req := &pb.RefreshAccountRequest{
			Id:            args.ID,
			Credential:    []byte(args.Credential),
			TestAfterSave: args.TestAfterSave,
		}
		out, err := backend.RefreshAccount(ctx, req)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(out)
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "update_account",
		Description: "Update account metadata. Optional fields are applied only when present; allowed_models replaces the set when non-empty.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args UpdateAccountArgs) (*mcp.CallToolResult, any, error) {
		req := &pb.UpdateAccountRequest{
			Id: args.ID,
			// Optional fields map straight through; nil stays unset (present-only).
			Label:         args.Label,
			Email:         args.Email,
			Priority:      args.Priority,
			Status:        args.Status,
			AllowedModels: args.AllowedModels,
		}
		out, err := backend.UpdateAccount(ctx, req)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(out)
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "test_account",
		Description: "Validate an account's credential and, when provider verification is available, run a trivial CLI invocation; records the outcome.",
		Annotations: &mcp.ToolAnnotations{},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args IDArgs) (*mcp.CallToolResult, any, error) {
		out, err := backend.TestAccount(ctx, args.ID)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(out)
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "send_chat_message",
		Description: "Deliver a user message into a chat's live agent, optionally waking it if asleep.",
		Annotations: &mcp.ToolAnnotations{},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args SendChatMessageArgs) (*mcp.CallToolResult, any, error) {
		wakeIfAsleep := true
		if args.WakeIfAsleep != nil {
			wakeIfAsleep = *args.WakeIfAsleep
		}
		// Default (omitted) => false: a driver prefills the composer without
		// submitting. Set submit=true to reliably submit a single-line message
		// (Enter + verifier); a multi-line message is paste-only regardless.
		submit := false
		if args.Submit != nil {
			submit = *args.Submit
		}
		req := &pb.SendChatMessageRequest{
			AgentSessionId: args.AgentSessionID,
			Message:        args.Message,
			WakeIfAsleep:   wakeIfAsleep,
			Submit:         submit,
		}
		out, err := backend.SendChatMessage(ctx, req)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(out)
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "switch_account",
		Description: "Stop the session's live chat, rebind it to the chosen account, and respawn with resume. Pass account_id empty to target the system default (account 0). Omit agent_session_id to target the session's primary live chat. A mid-turn (WORKING) chat is rejected unless force is set; a cooling or disabled target is rejected with a human-readable reason.",
		Annotations: &mcp.ToolAnnotations{},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args SwitchAccountArgs) (*mcp.CallToolResult, any, error) {
		req := &pb.SwitchSessionAccountRequest{
			SessionId: args.SessionID,
			AccountId: args.AccountID,
			Force:     args.Force,
		}
		if args.AgentSessionID != "" {
			req.AgentSessionId = proto.String(args.AgentSessionID)
		}
		out, err := backend.SwitchSessionAccount(ctx, req)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(out)
		return r, nil, err
	})
}

// registerSessionStateTool installs a simple id-keyed session lifecycle tool.
func registerSessionStateTool(server *mcp.Server, opts Options, name, desc string, idempotent bool, fn func(context.Context, string) (*pb.Session, error)) {
	addTool(server, opts, &mcp.Tool{
		Name:        name,
		Description: desc,
		Annotations: &mcp.ToolAnnotations{IdempotentHint: idempotent},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args IDArgs) (*mcp.CallToolResult, any, error) {
		out, err := fn(ctx, args.ID)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(out)
		return r, nil, err
	})
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
	ID                     string  `json:"id" jsonschema:"the repo id"`
	Name                   *string `json:"name,omitempty" jsonschema:"new display name"`
	MergeStrategy          *string `json:"merge_strategy,omitempty" jsonschema:"merge strategy (e.g. squash, merge, rebase)"`
	SetupScript            *string `json:"setup_script,omitempty" jsonschema:"setup script to run in new worktrees"`
	CanAutoMerge           *bool   `json:"can_auto_merge,omitempty" jsonschema:"mark a passing draft PR ready for review (does not merge)"`
	CanAutoMergeDependabot *bool   `json:"can_auto_merge_dependabot,omitempty" jsonschema:"allow auto-merge for dependabot PRs"`
	CanAutoRepair          *bool   `json:"can_auto_repair,omitempty" jsonschema:"allow the repair plugin to auto-repair PRs (failing checks, conflicts, review feedback)"`
	LinearAPIKey           *string `json:"linear_api_key,omitempty" jsonschema:"Linear API key for this repo; write-only, never returned by any read tool"`
	SentryAPIKey           *string `json:"sentry_api_key,omitempty" jsonschema:"Sentry auth token for this repo; write-only, never returned by any read tool"`
	SentryOrg              *string `json:"sentry_org,omitempty" jsonschema:"Sentry organization slug (issues are listed org-wide)"`
}

// createSessionOutput is the create_session tool's structured result. The
// created (or attached) session's fields are flattened at the top level (so the
// primary agent_session_id stays directly reachable) alongside attached_existing
// and, when the daemon attached to a pre-existing session, a Note explaining the
// supplied prompt was not run.
type createSessionOutput struct {
	*pb.Session
	AttachedExisting bool   `json:"attached_existing"`
	Note             string `json:"note,omitempty"`
}

// CreateSessionArgs is the typed argument struct for create_session. The full
// CreateSessionRequest surface is exposed except tracker_issue, the composite
// message used only for web server-side plan formatting (intentionally omitted).
type CreateSessionArgs struct {
	RepoID         string  `json:"repo_id" jsonschema:"the repo id to create the session under"`
	Prompt         string  `json:"prompt" jsonschema:"the plan/prompt for the session"`
	Title          string  `json:"title,omitempty" jsonschema:"session title; auto-derived from the first line of the prompt when omitted"`
	Agent          string  `json:"agent,omitempty" jsonschema:"agent runner plugin name (empty = server default)"`
	Account        *string `json:"account,omitempty" jsonschema:"Account id or label to run this session under; empty = system default"`
	BaseBranch     string  `json:"base_branch,omitempty" jsonschema:"base branch to create the session from (empty = repo default)"`
	BranchName     *string `json:"branch_name,omitempty" jsonschema:"explicit branch name (e.g. a tracker's suggested branch)"`
	ForceBranch    bool    `json:"force_branch,omitempty" jsonschema:"remove any existing branch with the same name before creating"`
	Force          bool    `json:"force,omitempty" jsonschema:"bypass tracker-issue dedup and create a second session for a tracker/PR/branch that already has an active one"`
	QuickChat      bool    `json:"quick_chat,omitempty" jsonschema:"quick chat session: no worktree, branch, or PR"`
	Detach         bool    `json:"detach,omitempty" jsonschema:"run the initial agent pass headlessly (claude --print / codex exec) instead of leaving the session idle until attach; set true for unattended orchestration"`
	TmuxUnattended bool    `json:"tmux_unattended,omitempty" jsonschema:"run the session in a durable tmux-hosted pane that survives a daemon restart and is attach-safe (used by /boss-epic); a distinct autonomous-unattended path from detach's headless runs"`
	Model          string  `json:"model,omitempty" jsonschema:"opaque agent model id to run this session under (e.g. an Opus id); empty = the agent plugin's default"`
	PRNumber       *int32  `json:"pr_number,omitempty" jsonschema:"target an existing PR. If an active session already owns that PR's branch, create_session ATTACHES to it and returns attached_existing=true WITHOUT running prompt — run the prompt in that session (send_chat_message with the returned agent_session_id). force does NOT bypass this branch attach"`
	TrackerID      *string `json:"tracker_id,omitempty" jsonschema:"external issue id (e.g. FRE-1176). If an active session already owns this tracker id (with no branch collision), create_session fails with AlreadyExists — pass force:true to create a second session for the same tracker"`
	TrackerURL     *string `json:"tracker_url,omitempty" jsonschema:"URL to the issue in the external tracker"`
	TrackerSource  *string `json:"tracker_source,omitempty" jsonschema:"tracker source: linear or sentry"`
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
	RepoID          string `json:"repo_id" jsonschema:"the repo id"`
	Name            string `json:"name" jsonschema:"cron job name"`
	Prompt          string `json:"prompt" jsonschema:"the prompt to run each fire"`
	Schedule        string `json:"schedule" jsonschema:"cron schedule expression"`
	Timezone        string `json:"timezone,omitempty" jsonschema:"IANA timezone (empty = daemon-local)"`
	Enabled         bool   `json:"enabled,omitempty" jsonschema:"whether the job is enabled"`
	AgentName       string `json:"agent_name,omitempty" jsonschema:"agent runner plugin name (empty = claude)"`
	Model           string `json:"model,omitempty" jsonschema:"opaque agent model id (empty = plugin default)"`
	GateCommand     string `json:"gate_command,omitempty" jsonschema:"command run before each fire; non-zero exit skips the run, empty = no gate"`
	RunSetupCommand *bool  `json:"run_setup_command,omitempty" jsonschema:"run the repo setup script before the agent; omitted = server default"`
}

// SendChatMessageArgs is the typed argument struct for send_chat_message.
type SendChatMessageArgs struct {
	AgentSessionID string `json:"agent_session_id" jsonschema:"the agent session UUID"`
	Message        string `json:"message" jsonschema:"the user message to deliver"`
	WakeIfAsleep   *bool  `json:"wake_if_asleep,omitempty" jsonschema:"wake the agent if it is currently asleep; defaults to true when omitted"`
	Submit         *bool  `json:"submit,omitempty" jsonschema:"submit a single-line message (press Enter and verify) instead of only prefilling the composer; a multi-line message is never auto-submitted; defaults to false (prefill) when omitted"`
}

// SwitchAccountArgs is the typed argument struct for switch_account.
type SwitchAccountArgs struct {
	SessionID      string `json:"session_id" jsonschema:"the session whose live chat to switch"`
	AccountID      string `json:"account_id" jsonschema:"account id or provider-scoped label (unique within the session's provider); empty for system default"`
	AgentSessionID string `json:"agent_session_id,omitempty" jsonschema:"optional specific chat; defaults to the session's primary live chat"`
	Force          bool   `json:"force,omitempty" jsonschema:"interrupt a mid-turn chat"`
}

// UpdateCronJobArgs is the typed argument struct for update_cron_job. Optional
// pointer fields are only applied when present.
type UpdateCronJobArgs struct {
	ID              string  `json:"id" jsonschema:"the cron job id"`
	Name            *string `json:"name,omitempty" jsonschema:"new name"`
	Prompt          *string `json:"prompt,omitempty" jsonschema:"new prompt"`
	Schedule        *string `json:"schedule,omitempty" jsonschema:"new cron schedule expression"`
	Timezone        *string `json:"timezone,omitempty" jsonschema:"new IANA timezone"`
	Enabled         *bool   `json:"enabled,omitempty" jsonschema:"enable or disable the job"`
	AgentName       *string `json:"agent_name,omitempty" jsonschema:"new agent runner plugin name"`
	Model           *string `json:"model,omitempty" jsonschema:"new opaque agent model id (empty = plugin default)"`
	GateCommand     *string `json:"gate_command,omitempty" jsonschema:"new gate command; empty = no gate"`
	RunSetupCommand *bool   `json:"run_setup_command,omitempty" jsonschema:"run the repo setup script before the agent"`
}

// AddAccountArgs is the typed argument struct for add_account. It maps 1:1 onto
// pb.AddAccountRequest. The credential is inbound only — no response ever
// returns it.
type AddAccountArgs struct {
	Provider   string `json:"provider" jsonschema:"account provider (claude|codex)"`
	Label      string `json:"label" jsonschema:"human label, unique per provider"`
	Email      string `json:"email,omitempty" jsonschema:"optional informational account email"`
	Priority   int32  `json:"priority,omitempty" jsonschema:"sort order; lower = preferred"`
	Credential string `json:"credential,omitempty" jsonschema:"credential blob (Claude setup-token string or Codex auth.json contents); stored in the keyring, never returned"`
}

// RefreshAccountArgs is the typed argument struct for refresh_account. It maps
// 1:1 onto pb.RefreshAccountRequest. The credential is inbound only — no
// response ever returns it.
type RefreshAccountArgs struct {
	ID            string `json:"id" jsonschema:"the account id"`
	Credential    string `json:"credential,omitempty" jsonschema:"new credential blob; stored in the keyring, never returned"`
	TestAfterSave bool   `json:"test_after_save,omitempty" jsonschema:"validate the refreshed credential after saving"`
}

// UpdateAccountArgs is the typed argument struct for update_account. Optional
// pointer fields are only applied when present; allowed_models replaces the set
// when non-empty. It maps 1:1 onto pb.UpdateAccountRequest.
type UpdateAccountArgs struct {
	ID            string   `json:"id" jsonschema:"the account id"`
	Label         *string  `json:"label,omitempty" jsonschema:"new label"`
	Email         *string  `json:"email,omitempty" jsonschema:"new account email"`
	Priority      *int32   `json:"priority,omitempty" jsonschema:"new priority (lower = preferred)"`
	Status        *string  `json:"status,omitempty" jsonschema:"new status (active|disabled)"`
	AllowedModels []string `json:"allowed_models,omitempty" jsonschema:"replace the allowed-models set when non-empty"`
}
