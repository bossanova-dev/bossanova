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
			WorktreeBaseDir: args.WorktreeBaseDir,
			LinearApiKey:    args.LinearAPIKey,
			SentryApiKey:    args.SentryAPIKey,
			SentryOrg:       args.SentryOrg,
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
		Description: "Create a new bossanova session for a repo with a prompt; drains setup and returns the final session (its agent_session_id is the primary chat id — no sqlite read needed). The bootstrap runs in the background, so the create continues on the daemon even if this call is cancelled; agent_launched means an agent process launched, not proof the prompt was consumed. This path has no turn-start observation, so verify work began with `get_chat_statuses`. DEDUP: if an active session already owns the target branch or PR (via pr_number/branch_name), the daemon ATTACHES to that existing session instead of creating one — the result then has attached_existing=true and the supplied prompt is NOT run; deliver it via send_chat_message with the returned agent_session_id (force does NOT bypass this branch/PR attach). By DEFAULT a create with a prompt runs unattended (the CLI's implicit --detach): agent_launched=true, env BOSS_UNATTENDED=true so skills run headless; no env arg needed. To instead create the session idle awaiting a human `boss attach`, pass attended:true — the result then reports agent_launched=false with a next_action hint. The composite tracker_issue field is web-only, not exposed here.\nZERO-CHANGE SESSIONS: planning-only work should use a subagent; for visible no-worktree/no-PR conversation use quick_chat. For read-only worktree planning without an up-front draft PR, set defer_pr:true with detach or tmux_unattended.",
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
		if args.Effort != "" {
			req.Effort = &args.Effort
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
		Name:        "refresh_session_pr",
		Description: "Refresh one cached PR status from the VCS provider by session_id, pr_number, or both. Fetch failures return errors without overwriting the cache.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args RefreshSessionPRArgs) (*mcp.CallToolResult, any, error) {
		req := &pb.RefreshSessionPRRequest{}
		if args.SessionID != "" {
			req.Id = &args.SessionID
		}
		if args.PRNumber != nil {
			req.PrNumber = args.PRNumber
		}
		out, err := backend.RefreshSessionPR(ctx, req)
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
		policy, err := cronConcurrencyPolicyArg(args.ConcurrencyPolicy)
		if err != nil {
			return errorResult(err), nil, nil
		}
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
			ConcurrencyPolicy:     policy,
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
		policy, err := cronConcurrencyPolicyArg(args.ConcurrencyPolicy)
		if err != nil {
			return errorResult(err), nil, nil
		}
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
			ConcurrencyPolicy:     policy,
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
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args AddAgentAccountArgs) (*mcp.CallToolResult, any, error) {
		req := &pb.AddAgentAccountRequest{
			Provider:   args.Provider,
			Label:      args.Label,
			Priority:   args.Priority,
			Credential: []byte(args.Credential),
		}
		out, err := backend.AddAgentAccount(ctx, req)
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
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args RefreshAgentAccountArgs) (*mcp.CallToolResult, any, error) {
		req := &pb.RefreshAgentAccountRequest{
			Id:            args.ID,
			Credential:    []byte(args.Credential),
			TestAfterSave: args.TestAfterSave,
		}
		out, err := backend.RefreshAgentAccount(ctx, req)
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
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args UpdateAgentAccountArgs) (*mcp.CallToolResult, any, error) {
		req := &pb.UpdateAgentAccountRequest{
			Id: args.ID,
			// Optional fields map straight through; nil stays unset (present-only).
			Label:         args.Label,
			Priority:      args.Priority,
			Status:        args.Status,
			AllowedModels: args.AllowedModels,
		}
		out, err := backend.UpdateAgentAccount(ctx, req)
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
		out, err := backend.TestAgentAccount(ctx, args.ID)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(out)
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "send_chat_message",
		Description: "Deliver a user message into one chat's live agent (targeted by its agent_session_id, e.g. one returned by start_chat or create_session), optionally waking it if asleep. Omit submit to send it; submit:false only stages it. delivered is a handoff receipt, not proof the agent took the work; read `turn_start_state_name` for the observed turn-start verdict. NOT_OBSERVED does not mean not delivered, and re-sending double-posts the prompt; poll `get_chat_statuses` instead. Prefill-only sends return UNOBSERVABLE.",
		Annotations: &mcp.ToolAnnotations{},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args SendChatMessageArgs) (*mcp.CallToolResult, any, error) {
		wakeIfAsleep := true
		if args.WakeIfAsleep != nil {
			wakeIfAsleep = *args.WakeIfAsleep
		}
		// Default (omitted) => true (BOS-1270). A driver that says "send this"
		// means it: the message is Enter-submitted and the submission verified,
		// for single- and multi-line payloads alike. The old prefill default
		// silently stalled orchestration — messages sat unactioned in the
		// target's composer until a human opened the chat and pressed Enter.
		// Staging a composer message is still available, but it is now the
		// deliberate opt-out (submit=false) rather than what omission buys.
		// This resolution is the public adapter's, not the daemon's: the raw
		// SendChatMessageRequest bool cannot distinguish omitted from false, so
		// an explicit wire-level false keeps prefilling (see
		// services/bossd/internal/server/send_chat_message_test.go).
		submit := true
		if args.Submit != nil {
			submit = *args.Submit
		}
		req := &pb.SendChatMessageRequest{
			AgentSessionId:         args.AgentSessionID,
			Message:                args.Message,
			WakeIfAsleep:           wakeIfAsleep,
			Submit:                 submit,
			ShouldObserveTurnStart: true,
		}
		out, err := backend.SendChatMessage(ctx, req)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(sendChatMessageOutput{
			SendChatMessageResponse: out,
			TurnStartStateName:      out.GetTurnStartState().String(),
			DeliveryStateName:       out.GetDeliveryState().String(),
		})
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "switch_account",
		Description: "Stop the session's live chat, rebind it to the chosen account, and respawn with resume. Pass account_id empty to target the system default (account 0). Omit agent_session_id to target the session's primary live chat. A mid-turn (WORKING) chat is rejected unless force is set; a cooling or disabled target is rejected with a human-readable reason.",
		Annotations: &mcp.ToolAnnotations{},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args SwitchAgentAccountArgs) (*mcp.CallToolResult, any, error) {
		req := &pb.SwitchSessionAgentAccountRequest{
			SessionId: args.SessionID,
			AccountId: args.AccountID,
			Force:     args.Force,
		}
		if args.AgentSessionID != "" {
			req.AgentSessionId = proto.String(args.AgentSessionID)
		}
		out, err := backend.SwitchSessionAgentAccount(ctx, req)
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
		Description: "Register a durable one-shot GitHub PR callback: when the given PR reaches the trigger state, the message is delivered once as a prompt to the target chat. Delivery is a signal, not proof: verify the PR's actual state. The message body is a secret: stored verbatim, never echoed back. Expiry defaults to 24h, max 30 days.",
		Annotations: &mcp.ToolAnnotations{},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args RegisterGitHubCallbackArgs) (*mcp.CallToolResult, any, error) {
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
		req := &pb.CreateGitHubCallbackRequest{
			TargetChatId:            strings.TrimSpace(args.TargetChatID),
			RepoOwner:               ref.Owner,
			RepoName:                ref.Repo,
			PrNumber:                ref.PRNumber,
			Trigger:                 string(trigger),
			Message:                 args.Message,
			ExpiresAt:               timestamppb.New(expiresAt),
			ShouldRequireTransition: &args.ShouldRequireTransition,
		}
		if g := strings.TrimSpace(args.Group); g != "" {
			req.GroupId = &g
		}
		if args.IndependentWatch {
			req.IsIndependentWatch = &args.IndependentWatch
		}
		resp, err := backend.CreateGitHubCallback(ctx, req)
		if err != nil {
			return errorResult(err), nil, nil
		}
		// Never surface the message body: scrub it before returning.
		r, err := jsonResult(registerGitHubCallbackOutput{
			Callback:   redactCallback(resp.GetGithubCallback()),
			NoticeText: resp.GetNoticeText(),
		})
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

	registerOrganizationNoteMutatingTools(server, backend, opts)

	addTool(server, opts, &mcp.Tool{
		Name: "apply_project_config",
		Description: "Converge onto the .boss-project.json in config, with diff_project_config's scope. Only plans " +
			"unless dry_run is false. Never prunes, resolves or mints a secret (those are skips for the CLI). " +
			"Locally the checkout must be on the default branch unless allow_branch.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: boolPtr(false), IdempotentHint: true},
	}, applyProjectConfigHandler(backend))
}

// registerOrganizationNoteMutatingTools installs create_organization_note and
// update_organization_note. A quota refusal comes back as the API's
// ResourceExhausted with the decoded usage and reset time appended.
func registerOrganizationNoteMutatingTools(server *mcp.Server, backend Backend, opts Options) {
	addTool(server, opts, &mcp.Tool{
		Name: "create_organization_note",
		Description: "Write a note to an organization's cloud store, visible to every member. Spends one unit of the " +
			"organization's hourly write quota; a spent quota is ResourceExhausted with usage and reset time. " +
			"The note expires 90 days after creation. The body is not a secret and is returned in full.",
		Annotations: &mcp.ToolAnnotations{},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args CreateOrganizationNoteArgs) (*mcp.CallToolResult, any, error) {
		note, err := backend.CreateOrganizationNote(ctx, &pb.CreateOrganizationNoteRequest{
			OrganizationId: args.OrganizationID,
			Body:           args.Body,
			Tags:           args.Tags,
			RepoOriginUrl:  optionalString(args.RepoOriginURL),
			SessionId:      optionalString(args.SessionID),
			ChatId:         optionalString(args.ChatID),
			IdempotencyKey: optionalString(args.IdempotencyKey),
		})
		if err != nil {
			return organizationNoteErrorResult(err), nil, nil
		}
		r, err := jsonResult(note)
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name: "update_organization_note",
		Description: "Edit an organization note's body and/or tags; omitted fields are left alone. Only the author or " +
			"an organization owner may. A synced note is FailedPrecondition — edit it at its source daemon. " +
			"A change spends one quota unit; expires_at is never extended.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args UpdateOrganizationNoteArgs) (*mcp.CallToolResult, any, error) {
		req := &pb.UpdateOrganizationNoteRequest{OrganizationId: args.OrganizationID, Id: args.ID}
		if args.Body != nil {
			v := *args.Body
			req.Body = &v
		}
		// nil leaves the tags alone; a set-but-empty list clears them.
		if args.Tags != nil {
			req.Tags = &pb.NoteTagSet{Tags: *args.Tags}
		}
		note, err := backend.UpdateOrganizationNote(ctx, req)
		if err != nil {
			return organizationNoteErrorResult(err), nil, nil
		}
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

// cronConcurrencyPolicyArg maps the cron tools' concurrency_policy argument
// onto the request field. Empty means omitted and stays nil (create resolves
// it to skip, update leaves the stored value alone). Matching is lenient —
// case-insensitive, '-' accepted for '_' — and anything else is an error
// naming the valid values rather than a silent default.
func cronConcurrencyPolicyArg(raw string) (*pb.CronJobConcurrencyPolicy, error) {
	if raw == "" {
		return nil, nil
	}
	policy, ok := models.ParseCronJobConcurrencyPolicyInput(raw)
	if !ok {
		return nil, fmt.Errorf("invalid concurrency_policy %q: want one of skip, cancel_in_progress, allow_concurrent", raw)
	}
	return models.CronJobConcurrencyPolicyToProto(policy).Enum(), nil
}
