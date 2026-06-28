package bossmcp

import (
	"context"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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
		r, err := jsonResult(out)
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
		r, err := jsonResult(out)
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
		}
		out, err := backend.UpdateRepo(ctx, req)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(out)
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "create_session",
		Description: "Create a new bossanova session for a repo with a prompt; drains setup and returns the final session.",
		Annotations: &mcp.ToolAnnotations{},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args CreateSessionArgs) (*mcp.CallToolResult, any, error) {
		req := &pb.CreateSessionRequest{
			RepoId: args.RepoID,
			Plan:   args.Prompt,
			Title:  deriveSessionTitle(args.Title, args.Prompt),
		}
		if args.Agent != "" {
			req.AgentName = &args.Agent
		}
		out, err := backend.CreateSession(ctx, req)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(out)
		return r, nil, err
	})

	registerSessionStateTool(server, opts, "stop_session", "Stop a running session.", false, backend.StopSession)
	registerSessionStateTool(server, opts, "pause_session", "Pause a session.", true, backend.PauseSession)
	registerSessionStateTool(server, opts, "resume_session", "Resume a paused session.", true, backend.ResumeSession)
	registerSessionStateTool(server, opts, "retry_session", "Retry a failed session.", false, backend.RetrySession)

	addTool(server, opts, &mcp.Tool{
		Name:        "update_session",
		Description: "Rename a session (update its title). Also syncs the linked GitHub PR title when the session has one. PR creation is a separate prior step (e.g. `gh pr create`); to attach a PR use link_session_pr.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args UpdateSessionArgs) (*mcp.CallToolResult, any, error) {
		req := &pb.UpdateSessionRequest{Id: args.ID, Title: args.Title}
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
			RepoId:    args.RepoID,
			Name:      args.Name,
			Prompt:    args.Prompt,
			Schedule:  args.Schedule,
			Timezone:  args.Timezone,
			Enabled:   args.Enabled,
			AgentName: args.AgentName,
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
		Name:        "send_chat_message",
		Description: "Deliver a user message into a chat's live agent, optionally waking it if asleep.",
		Annotations: &mcp.ToolAnnotations{},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args SendChatMessageArgs) (*mcp.CallToolResult, any, error) {
		wakeIfAsleep := true
		if args.WakeIfAsleep != nil {
			wakeIfAsleep = *args.WakeIfAsleep
		}
		req := &pb.SendChatMessageRequest{
			AgentSessionId: args.AgentSessionID,
			Message:        args.Message,
			WakeIfAsleep:   wakeIfAsleep,
		}
		out, err := backend.SendChatMessage(ctx, req)
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
}

// CreateSessionArgs is the typed argument struct for create_session.
type CreateSessionArgs struct {
	RepoID string `json:"repo_id" jsonschema:"the repo id to create the session under"`
	Prompt string `json:"prompt" jsonschema:"the plan/prompt for the session"`
	Title  string `json:"title,omitempty" jsonschema:"session title; auto-derived from the first line of the prompt when omitted"`
	Agent  string `json:"agent,omitempty" jsonschema:"agent runner plugin name (empty = server default)"`
}

// UpdateSessionArgs is the typed argument struct for update_session.
type UpdateSessionArgs struct {
	ID    string  `json:"id" jsonschema:"the id of the session to retitle"`
	Title *string `json:"title,omitempty" jsonschema:"the new session title (also best-effort renames the linked GitHub PR)"`
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
	RepoID    string `json:"repo_id" jsonschema:"the repo id"`
	Name      string `json:"name" jsonschema:"cron job name"`
	Prompt    string `json:"prompt" jsonschema:"the prompt to run each fire"`
	Schedule  string `json:"schedule" jsonschema:"cron schedule expression"`
	Timezone  string `json:"timezone,omitempty" jsonschema:"IANA timezone (empty = daemon-local)"`
	Enabled   bool   `json:"enabled,omitempty" jsonschema:"whether the job is enabled"`
	AgentName string `json:"agent_name,omitempty" jsonschema:"agent runner plugin name (empty = claude)"`
}

// SendChatMessageArgs is the typed argument struct for send_chat_message.
type SendChatMessageArgs struct {
	AgentSessionID string `json:"agent_session_id" jsonschema:"the agent session UUID"`
	Message        string `json:"message" jsonschema:"the user message to deliver"`
	WakeIfAsleep   *bool  `json:"wake_if_asleep,omitempty" jsonschema:"wake the agent if it is currently asleep; defaults to true when omitted"`
}

// UpdateCronJobArgs is the typed argument struct for update_cron_job. Optional
// pointer fields are only applied when present.
type UpdateCronJobArgs struct {
	ID        string  `json:"id" jsonschema:"the cron job id"`
	Name      *string `json:"name,omitempty" jsonschema:"new name"`
	Prompt    *string `json:"prompt,omitempty" jsonschema:"new prompt"`
	Schedule  *string `json:"schedule,omitempty" jsonschema:"new cron schedule expression"`
	Timezone  *string `json:"timezone,omitempty" jsonschema:"new IANA timezone"`
	Enabled   *bool   `json:"enabled,omitempty" jsonschema:"enable or disable the job"`
	AgentName *string `json:"agent_name,omitempty" jsonschema:"new agent runner plugin name"`
}
