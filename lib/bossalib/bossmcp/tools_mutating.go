package bossmcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/recurser/bossalib/broadcast"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/githubcallback"
	"github.com/recurser/bossalib/models"
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
			Id:                        args.ID,
			DisplayName:               args.Name,
			MergeStrategy:             args.MergeStrategy,
			SetupScript:               args.SetupScript,
			CanAutoMerge:              args.CanAutoMerge,
			CanAutoMergeDependabot:    args.CanAutoMergeDependabot,
			CanAutoRepair:             args.CanAutoRepair,
			ShouldKeepBranchesCurrent: args.ShouldKeepBranchesCurrent,
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
		Description: "Create a new bossanova session for a repo with a prompt; drains setup and returns the final session (its agent_session_id is the primary chat id — no sqlite read needed). The bootstrap runs in the background, so the create continues on the daemon even if this call is cancelled; this tool still awaits the settled session, so agent_launched is accurate. DEDUP: if an active session already owns the target branch or PR (via pr_number/branch_name), the daemon ATTACHES to that existing session instead of creating one — the result then has attached_existing=true and the supplied prompt is NOT run; deliver it yourself via send_chat_message with the returned agent_session_id (force does NOT bypass this branch/PR attach — two active sessions cannot share one branch). If an active session already owns the same tracker_id with no branch collision, the create fails with AlreadyExists; pass force:true to create a second session for that tracker. Supports detach (headless initial agent pass), tmux_unattended (durable tmux pane surviving a daemon restart; used by /boss-epic), model, account (an id or label; empty = system default), pr_number, quick_chat, explicit base/branch names, and tracker_id/tracker_url/tracker_source. By DEFAULT, a create with a non-empty prompt launches the agent headless (mirroring the CLI's implicit --detach) so the work actually runs; the result reports agent_launched=true. To instead create the session idle awaiting a human `boss attach` (no agent started), pass attended:true — the result then reports agent_launched=false with a next_action hint. Planning-only work should use a subagent; for a visible no-worktree/no-PR conversation use quick_chat, not tmux_unattended. For a read-only/planning worktree session that should NOT get an up-front draft PR, set defer_pr:true (best paired with detach/tmux_unattended, which open a PR at finalize only if commits land) The composite tracker_issue field is web-only, not exposed here.",
		Annotations: &mcp.ToolAnnotations{},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args CreateSessionArgs) (*mcp.CallToolResult, any, error) {
		// BOS-498: mirror the CLI's implicit --detach (services/boss/cmd
		// newDetachRequest). A programmatic MCP caller that supplies a prompt but
		// no attended opt-in wants the agent to actually run, not an idle session
		// awaiting a human attach — the previously silent default. Default such a
		// create to headless. A prompt-less create legitimately stays idle, and an
		// explicit attended:true / detach / tmux_unattended is never overridden.
		// This lives at the MCP handler layer only, leaving bossd's
		// Detach-means-headless semantics (and the bossanova.v1 proto surface)
		// untouched.
		detach := args.Detach
		if args.Prompt != "" && !args.Attended && !args.Detach && !args.IsTmuxUnattended {
			detach = true
		}
		req := &pb.CreateSessionRequest{
			RepoId:      args.RepoID,
			Plan:        args.Prompt,
			Title:       deriveSessionTitle(args.Title, args.Prompt),
			BaseBranch:  args.BaseBranch,
			ForceBranch: args.ForceBranch,
			IsQuickChat: args.IsQuickChat,
			// Force bypasses the BOS-236 tracker-issue dedup guard so a caller
			// can intentionally create a second session for a tracker/PR/branch
			// that already has an active one.
			Force: args.Force,
			// Detach runs the initial agent pass headlessly (claude --print /
			// codex exec) instead of leaving the session idle until attach —
			// what an unattended /boss-epic fan-out needs. Defaulted true above
			// for a prompt-carrying create with no attended opt-in.
			Detach: detach,
			// IsTmuxUnattended runs the session in a durable tmux-hosted pane that
			// survives a daemon restart and is attach-safe — the /boss-epic fan-out
			// path, distinct from Detach's headless runs.
			IsTmuxUnattended: args.IsTmuxUnattended,
			// DeferPR skips the up-front draft PR; the finalize EnsurePR hook opens
			// one only if commits land. Meaningful only alongside detach/tmux
			// (which install that hook) — for read-only/planning sessions.
			DeferPr: args.DeferPR,
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
		// BOS-498: report loudly whether THIS create actually started an agent.
		// headless/tmux create a chat synchronously (a non-empty
		// agent_session_id); an idle session (attended or prompt-less) does not.
		// The dedup-attach path (attached_existing) launched nothing and did not
		// run the supplied prompt even when the pre-existing session already has a
		// chat, so it reports agent_launched:false with the attach Note carrying
		// the reachable id — never a false "your prompt is running" signal. A
		// caller that sees agent_launched:false opted into idle mode (or was
		// deduped) and must start/reach the session itself — the hint says how.
		payload := createSessionOutput{
			Session:          out.Session,
			AttachedExisting: out.AttachedExisting,
			AgentLaunched:    !out.AttachedExisting && out.Session.GetAgentSessionId() != "",
		}
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
		} else if !payload.AgentLaunched {
			// A genuine create that started no agent: the session is idle awaiting
			// attach (attended:true, or a prompt-less create). Say so loudly and
			// point at the ways to start work.
			payload.NextAction = "session is idle awaiting attach; no agent was launched. Re-create with detach:true or tmux_unattended:true to run the prompt unattended, or call start_chat to start a chat in this session."
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
		Name:        "start_chat",
		Description: "Start a new live agent chat inside an EXISTING session. Mints a fresh agent_session_id, spawns a live agent in the session's existing worktree/branch (a sibling of any current chats), and returns the new chat including its agent_session_id — the handle you then pass to send_chat_message, get_chat_transcript, and delete_chat. Use this to run additional agents in a session you already have; it does NOT create a new session (use create_session for that). To resume or register a chat whose agent_session_id you already hold, use record_chat instead.",
		// Not read-only, not idempotent: each call creates a distinct new chat.
		Annotations: &mcp.ToolAnnotations{},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args StartChatArgs) (*mcp.CallToolResult, any, error) {
		// Mint the agent_session_id here (matching daemon + web) and reuse the
		// existing RecordChat(resume=false) primitive, which spawns a fresh live
		// tmux agent in the session's worktree.
		id := uuid.NewString()
		out, err := backend.RecordChat(ctx, args.SessionID, id, args.Title, args.AgentName, false)
		if err != nil {
			return errorResult(err), nil, nil
		}
		// RecordChat still succeeds when the daemon host has no tmux available:
		// its spawn step no-ops, persisting a chat row but never starting a live
		// agent, and the resulting tmux_session_name comes back empty. start_chat
		// promises a live chat the caller can immediately send_chat_message to, so
		// a missing pane is a failure — reject rather than hand back an
		// agent_session_id with nothing listening behind it.
		if out.GetTmuxSessionName() == "" {
			return errorResult(fmt.Errorf("start_chat could not spawn a live agent: no tmux session was created for chat %s (is tmux available on the daemon host?)", id)), nil, nil
		}
		r, err := jsonResult(out)
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "record_chat",
		Description: "Register or resume an agent chat whose agent_session_id you already have (e.g. one an agent plugin generated). Requires a valid agent_session_id. To START a brand-new chat in an existing session, use start_chat instead.",
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
		Description: "Update the title of one agent chat (targeted by its agent_session_id).",
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
			IsEnabled:   args.IsEnabled,
			AgentName:   args.AgentName,
			Model:       args.Model,
			GateCommand: args.GateCommand,
			// *bool: nil stays unset so the server applies its own default.
			ShouldRunSetupCommand: args.ShouldRunSetupCommand,
			IsZeroOutput:          args.IsZeroOutput,
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
			IsEnabled: args.IsEnabled,
			AgentName: args.AgentName,
			// Optional fields map straight through; nil stays unset (present-only).
			Model:                 args.Model,
			GateCommand:           args.GateCommand,
			ShouldRunSetupCommand: args.ShouldRunSetupCommand,
			IsZeroOutput:          args.IsZeroOutput,
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
		Description: "Deliver a user message into one chat's live agent (targeted by its agent_session_id, e.g. one returned by start_chat or create_session), optionally waking it if asleep.",
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

	addTool(server, opts, &mcp.Tool{
		Name:        "update_settings",
		Description: "Update the daemon's global settings (settings.json) with a partial update: only the fields you supply are changed. Scalars (worktree_base_dir, poll_interval_seconds, default_agent, event/error tracing, PostHog token/host) are validated exactly as the boss settings TUI validates them; per-agent updates upsert config keys (an empty value deletes the key) and toggle the agent's enabled flag. Invalid input returns an error and does not modify the file. Discover an agent's settable keys, types, and allowed values via list_agents. Some changes (poll interval, plugin enable/disable) persist immediately but only take effect on daemon restart.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args UpdateSettingsArgs) (*mcp.CallToolResult, any, error) {
		req := &pb.UpdateSettingsRequest{
			// Optional pointer scalars map straight through; nil stays unset (present-only).
			WorktreeBaseDir:      args.WorktreeBaseDir,
			PollIntervalSeconds:  args.PollIntervalSeconds,
			DefaultAgent:         args.DefaultAgent,
			EventTracingEnabled:  args.EventTracingEnabled,
			ErrorTrackingEnabled: args.ErrorTrackingEnabled,
			PosthogProjectToken:  args.PostHogProjectToken,
			PosthogHost:          args.PostHogHost,
		}
		for _, a := range args.Agents {
			req.Agents = append(req.Agents, &pb.AgentSettingsUpdate{
				Name:    a.Name,
				Enabled: a.Enabled,
				Config:  a.Config,
			})
		}
		out, err := backend.UpdateSettings(ctx, req)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(out)
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "start_repair_workflow",
		Description: "(Re-)arm the daemon's auto-repair workflow (equivalent to `boss repair start`). Returns already_running and a one-line detail. A RUNNING workflow is left untouched; a PAUSED one is left for the operator to resume.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ NoArgs) (*mcp.CallToolResult, any, error) {
		out, err := backend.StartRepairWorkflow(ctx)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(out)
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "register_github_callback",
		Description: "Register a durable one-shot GitHub PR callback: when the given PR reaches the trigger state, the message is delivered once as a prompt to the target chat. Delivery is a signal, not proof — the receiving agent must still verify the PR's actual state. The message body is stored verbatim and is a secret: it is never echoed back in this tool's output. Expiry defaults to 24h and may not exceed 30 days.",
		Annotations: &mcp.ToolAnnotations{},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args RegisterGithubCallbackArgs) (*mcp.CallToolResult, any, error) {
		if strings.TrimSpace(args.Message) == "" {
			return errorResult(fmt.Errorf("message is required (the prompt delivered to the chat when the callback fires)")), nil, nil
		}
		if strings.TrimSpace(args.TargetChatID) == "" {
			return errorResult(fmt.Errorf("target_chat_id is required (the chat to notify when the callback fires)")), nil, nil
		}
		trigger, err := githubcallback.ValidateTrigger(args.Trigger)
		if err != nil {
			return errorResult(err), nil, nil
		}
		// Resolve repository context from the optional owner/repo slug so a bare
		// PR number can be anchored; a full github.com URL needs no context.
		var ctxOwner, ctxRepo string
		if strings.TrimSpace(args.Repo) != "" {
			ctxOwner, ctxRepo, err = githubcallback.SplitRepo(args.Repo)
			if err != nil {
				return errorResult(err), nil, nil
			}
		}
		ref, err := githubcallback.ParsePRRef(args.PR, ctxOwner, ctxRepo)
		if err != nil {
			return errorResult(err), nil, nil
		}
		expiresAt, err := githubcallback.ParseExpiresIn(args.ExpiresIn, time.Now().UTC())
		if err != nil {
			return errorResult(err), nil, nil
		}
		req := &pb.CreateGithubCallbackRequest{
			TargetChatId: strings.TrimSpace(args.TargetChatID),
			RepoOwner:    ref.Owner,
			RepoName:     ref.Repo,
			PrNumber:     ref.PRNumber,
			Trigger:      string(trigger),
			Message:      args.Message,
			ExpiresAt:    timestamppb.New(expiresAt),
		}
		if g := strings.TrimSpace(args.Group); g != "" {
			req.GroupId = &g
		}
		cb, err := backend.CreateGithubCallback(ctx, req)
		if err != nil {
			return errorResult(err), nil, nil
		}
		// Never surface the message body: scrub it before returning.
		r, err := jsonResult(redactCallback(cb))
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name: "send_broadcast",
		Description: "Send one message to every chat the selector resolves to — the way an agent addresses its siblings. The message body is stored verbatim and is a SECRET: it is never echoed back by this or any other tool. " +
			broadcastSignalRule +
			" The audience is resolved ONCE, at send time: a chat created afterwards is never included. " +
			broadcastWakeCost + " " + broadcastSelectorGrammar +
			" The origin chat is dropped from its own audience unless include_origin is set. By default the audience is THIS daemon's chats only; set cross_daemon to also route the broadcast to other daemons through bosso. Expiry (how long delivery keeps being retried) defaults to 24h and may not exceed 30d.",
		Annotations: &mcp.ToolAnnotations{},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args SendBroadcastArgs) (*mcp.CallToolResult, any, error) {
		if strings.TrimSpace(args.Message) == "" {
			return errorResult(fmt.Errorf("message is required (the prompt body delivered to every resolved target; stored verbatim and never echoed back)")), nil, nil
		}
		selector, res := parseBroadcastSelector(args.To)
		if res != nil {
			return res, nil, nil
		}
		req := &pb.SendBroadcastRequest{
			Selector:      broadcast.SelectorToProto(selector),
			OriginChatId:  strings.TrimSpace(args.From),
			Message:       args.Message,
			IncludeOrigin: args.IncludeOrigin,
			// Without this the daemon resolves local chats only and never
			// publishes the egress event, so a fleet-wide selector silently
			// means "this daemon's chats".
			CrossDaemon: args.CrossDaemon,
		}
		// expires_in is a DURATION passed straight through: the daemon owns the
		// 24h default and the 30d cap, so converting it to a timestamp here
		// would fork that policy across two surfaces.
		if e := strings.TrimSpace(args.ExpiresIn); e != "" {
			req.ExpiresIn = &e
		}
		out, err := backend.SendBroadcast(ctx, req)
		if err != nil {
			return errorResult(err), nil, nil
		}
		// Never surface the message body: scrub it before returning.
		deliveries := out.GetDeliveries()
		r, err := jsonResult(sendBroadcastOutput{
			Broadcast:   redactBroadcast(out.GetBroadcast()),
			Deliveries:  deliveries,
			TargetCount: len(deliveries),
		})
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name: "register_broadcast_subscription",
		Description: "Register a standing rule that broadcasts one message when a session reaches an outcome (on: completed, errored, or settled — either). The message body is stored verbatim and is a SECRET: it is never echoed back by this or any other tool, and the subscription record carries no body field at all. " +
			broadcastSignalRule +
			" The audience is resolved at FIRE time, not now, so the chats that exist when the session settles are the ones addressed. " +
			broadcastWakeCost + " " + broadcastSelectorGrammar +
			" The registering chat is NOT self-excluded: name it in the selector and it is told. Expiry bounds how long the rule stands (default 24h, maximum 30d), not the delivery window of the broadcast it fires.",
		Annotations: &mcp.ToolAnnotations{},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args RegisterBroadcastSubscriptionArgs) (*mcp.CallToolResult, any, error) {
		if strings.TrimSpace(args.Message) == "" {
			return errorResult(fmt.Errorf("message is required (the prompt body broadcast when the subscription fires; stored verbatim and never echoed back)")), nil, nil
		}
		if strings.TrimSpace(args.Session) == "" {
			return errorResult(fmt.Errorf("session is required (the id of the session whose outcome fires this subscription)")), nil, nil
		}
		// The trigger vocabulary comes from models, not a literal list here, so
		// this surface cannot drift from the evaluator that classifies outcomes.
		trigger, err := models.ParseBroadcastTrigger(strings.TrimSpace(args.On))
		if err != nil {
			return errorResult(fmt.Errorf("on names the outcome to wait for: %w (valid: %s)", err, broadcastTriggerList())), nil, nil
		}
		selector, res := parseBroadcastSelector(args.To)
		if res != nil {
			return res, nil, nil
		}
		req := &pb.CreateBroadcastSubscriptionRequest{
			OwnerSessionId: strings.TrimSpace(args.Session),
			TriggerEvent:   trigger.String(),
			Selector:       broadcast.SelectorToProto(selector),
			OriginChatId:   strings.TrimSpace(args.From),
			Message:        args.Message,
		}
		// A duration, passed through verbatim — see send_broadcast.
		if e := strings.TrimSpace(args.ExpiresIn); e != "" {
			req.ExpiresIn = &e
		}
		sub, err := backend.CreateBroadcastSubscription(ctx, req)
		if err != nil {
			return errorResult(err), nil, nil
		}
		// No scrub is possible or needed: BroadcastSubscription deliberately
		// carries no message field, so the body cannot reach the caller.
		r, err := jsonResult(sub)
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "create_note",
		Description: "Record a note against a repository — durable free-text later runs can harvest. Notes are REPO-scoped: session_id and chat_id are provenance only, so deleting that session never removes the note. Body: verbatim, non-empty, max 64 KiB. Tags are normalised on write (max 32 x 64 bytes). The body is NOT a secret and is returned in full by this and every read tool.",
		Annotations: &mcp.ToolAnnotations{},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args CreateNoteArgs) (*mcp.CallToolResult, any, error) {
		req := &pb.CreateNoteRequest{
			RepoId: args.RepoID,
			Body:   args.Body,
			Tags:   args.Tags,
		}
		if args.SessionID != "" {
			v := args.SessionID
			req.SessionId = &v
		}
		if args.ChatID != "" {
			v := args.ChatID
			req.ChatId = &v
		}
		if args.IdempotencyKey != "" {
			v := args.IdempotencyKey
			req.IdempotencyKey = &v
		}
		note, err := backend.CreateNote(ctx, req)
		if err != nil {
			return errorResult(err), nil, nil
		}
		// Returned unredacted: the body is the payload, not a secret.
		r, err := jsonResult(note)
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "update_note",
		Description: "Update a note's body and/or tags; each field is optional and an omitted one is left alone. SUPPLYING tags REPLACES the note's entire tag set — it does not append to it — and supplying an EMPTY list CLEARS every tag, so omit tags entirely to leave the existing ones untouched. A supplied body replaces the stored body verbatim (non-empty, 64 KiB cap); tags are normalised on write (trimmed, lowercased, de-duplicated). The updated note, body included, is returned in full. " + noteRepoIDRouting,
		// Idempotent like every other update_* tool: replaying the same body/tag
		// set converges on the same note.
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args UpdateNoteArgs) (*mcp.CallToolResult, any, error) {
		req := &pb.UpdateNoteRequest{Id: args.ID}
		if args.Body != nil {
			v := *args.Body
			req.Body = &v
		}
		// The pointer is the only thing carrying "leave the tags alone" (nil)
		// versus "replace the whole set with this list, clearing it when the
		// list is empty" (set), so it must not be flattened to a plain slice.
		if args.Tags != nil {
			req.Tags = &pb.NoteTagSet{Tags: *args.Tags}
		}
		note, err := backend.UpdateNote(ctx, args.RepoID, req)
		if err != nil {
			return errorResult(err), nil, nil
		}
		// Returned unredacted: the body is the payload, not a secret.
		r, err := jsonResult(note)
		return r, nil, err
	})
}

// parseBroadcastSelector turns the textual `to` argument into a validated
// Selector, or returns the error result to hand back to the model. Both
// broadcast-creating tools share it so neither hand-rolls the grammar and
// neither dispatches to the daemon on an audience the shared parser refuses.
//
// The blank check is separate from Parse's own empty-input error because it
// must name the ARGUMENT the agent has to fix; every other failure surfaces the
// parser's message verbatim, since it already names the offending token and the
// valid keys.
func parseBroadcastSelector(to string) (broadcast.Selector, *mcp.CallToolResult) {
	if strings.TrimSpace(to) == "" {
		return broadcast.Selector{}, errorResult(fmt.Errorf(
			"to is required (the selector naming the audience, e.g. repo:<id>,agent:claude+account:<id>); an empty selector is never everyone"))
	}
	selector, err := broadcast.Parse(to)
	if err != nil {
		return broadcast.Selector{}, errorResult(err)
	}
	if err := selector.Validate(); err != nil {
		return broadcast.Selector{}, errorResult(err)
	}
	return selector, nil
}

// broadcastTriggerList renders the canonical trigger vocabulary for an error
// message, derived from models so it cannot drift from what is accepted.
func broadcastTriggerList() string {
	triggers := models.BroadcastTriggers()
	names := make([]string, 0, len(triggers))
	for _, t := range triggers {
		names = append(names, t.String())
	}
	return strings.Join(names, ", ")
}

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
	RepoID string    `json:"repo_id" jsonschema:"the note's owning repo id (required, even on a local daemon that ignores it; the hosted gateway routes by it). Use the daemon-local repo id list_repos/resolve_context return, NOT a git origin URL — an origin URL resolves to NotFound. It routes but does NOT scope: the id alone selects the note, and a mismatched repo_id is not checked, so this is not a safety check"`
	ID     string    `json:"id" jsonschema:"the note id to update (required)"`
	Body   *string   `json:"body,omitempty" jsonschema:"replacement note text; omit to leave the body unchanged"`
	Tags   *[]string `json:"tags,omitempty" jsonschema:"REPLACES the note's entire tag set — it does not append. Supply an empty list to clear every tag, or omit the field entirely to leave the existing tags unchanged"`
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

// RegisterGithubCallbackArgs is the typed argument struct for
// register_github_callback. The message is required and is a secret — it is
// stored verbatim and never returned by any tool.
type RegisterGithubCallbackArgs struct {
	PR           string `json:"pr" jsonschema:"the PR to watch: a bare number like 123 (requires repo context) or a full https://github.com/owner/repo/pull/123 URL"`
	Trigger      string `json:"trigger" jsonschema:"the PR event to fire on; one of merged, closed, checks_passed, checks_failed, ready_for_review (open and not a draft), checks_passed_ready (green and not a draft). Triggers match on PR state, not on transitions: arming one against a PR that already satisfies it fires on the next evaluation"`
	TargetChatID string `json:"target_chat_id" jsonschema:"the agent-session (chat) id to deliver the message to when the callback fires"`
	Message      string `json:"message" jsonschema:"the prompt delivered to the chat when the callback fires (required; stored verbatim and never echoed back)"`
	Repo         string `json:"repo,omitempty" jsonschema:"repository as owner/repo; required to anchor a bare PR number, ignored when pr is a full URL"`
	ExpiresIn    string `json:"expires_in,omitempty" jsonschema:"expiry as a duration like 30m, 24h, 7d, 2w; default 24h, maximum 30d"`
	Group        string `json:"group,omitempty" jsonschema:"optional group id; siblings in a group cancel each other on first fire"`
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
	MergeStrategy             *string `json:"merge_strategy,omitempty" jsonschema:"merge strategy (e.g. squash, merge, rebase)"`
	SetupScript               *string `json:"setup_script,omitempty" jsonschema:"setup script to run in new worktrees"`
	CanAutoMerge              *bool   `json:"can_auto_merge,omitempty" jsonschema:"mark a passing draft PR ready for review (does not merge)"`
	CanAutoMergeDependabot    *bool   `json:"can_auto_merge_dependabot,omitempty" jsonschema:"allow auto-merge for dependabot PRs"`
	CanAutoRepair             *bool   `json:"can_auto_repair,omitempty" jsonschema:"allow the repair plugin to auto-repair PRs (failing checks, conflicts, review feedback)"`
	ShouldKeepBranchesCurrent *bool   `json:"should_keep_branches_current,omitempty" jsonschema:"proactively rebase in-flight session branches onto the base branch whenever a merge advances it (force-push with lease); opt-in, every rebase re-runs CI"`
	LinearAPIKey              *string `json:"linear_api_key,omitempty" jsonschema:"Linear API key for this repo; write-only, never returned by any read tool"`
	SentryAPIKey              *string `json:"sentry_api_key,omitempty" jsonschema:"Sentry auth token for this repo; write-only, never returned by any read tool"`
	SentryOrg                 *string `json:"sentry_org,omitempty" jsonschema:"Sentry organization slug (issues are listed org-wide)"`
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
	BranchName       *string `json:"branch_name,omitempty" jsonschema:"explicit branch name (e.g. a tracker's suggested branch)"`
	ForceBranch      bool    `json:"force_branch,omitempty" jsonschema:"remove any existing branch with the same name before creating"`
	Force            bool    `json:"force,omitempty" jsonschema:"bypass tracker-issue dedup and create a second session for a tracker/PR/branch that already has an active one"`
	IsQuickChat      bool    `json:"quick_chat,omitempty" jsonschema:"quick chat session: no worktree, branch, or PR"`
	Detach           bool    `json:"detach,omitempty" jsonschema:"run the initial agent pass headlessly (claude --print / codex exec) instead of leaving the session idle until attach; set true for unattended orchestration"`
	Attended         bool    `json:"attended,omitempty" jsonschema:"opt into the idle-until-attach behavior: create the session but do NOT launch an agent, awaiting a human boss attach. By default a prompt-carrying create launches headless (mirroring the CLI's implicit --detach); set attended:true only when a human will attach and drive the session interactively"`
	IsTmuxUnattended bool    `json:"tmux_unattended,omitempty" jsonschema:"run the session in a durable tmux-hosted pane that survives a daemon restart and is attach-safe (used by /boss-epic); a distinct autonomous-unattended path from detach's headless runs"`
	DeferPR          bool    `json:"defer_pr,omitempty" jsonschema:"create a worktree-backed session but do NOT open a draft PR up front; a PR is opened at finalize only if the run produces commits. Meaningful only alongside detach/tmux_unattended (which install the finalize hook); use for read-only/planning sessions"`
	Model            string  `json:"model,omitempty" jsonschema:"opaque agent model id to run this session under (e.g. an Opus id); empty = the agent plugin's default"`
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
}

// SendChatMessageArgs is the typed argument struct for send_chat_message.
type SendChatMessageArgs struct {
	AgentSessionID string `json:"agent_session_id" jsonschema:"the agent session UUID"`
	Message        string `json:"message" jsonschema:"the user message to deliver"`
	WakeIfAsleep   *bool  `json:"wake_if_asleep,omitempty" jsonschema:"wake the agent if it is currently asleep; defaults to true when omitted"`
	Submit         *bool  `json:"submit,omitempty" jsonschema:"submit the message (press Enter and verify) instead of only prefilling the composer; works for single- and multi-line messages alike; defaults to false (prefill) when omitted"`
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
}

// AddAccountArgs is the typed argument struct for add_account. It maps 1:1 onto
// pb.AddAccountRequest. The credential is inbound only — no response ever
// returns it.
type AddAccountArgs struct {
	Provider   string `json:"provider" jsonschema:"account provider (claude|codex)"`
	Label      string `json:"label" jsonschema:"human label, unique per provider"`
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
