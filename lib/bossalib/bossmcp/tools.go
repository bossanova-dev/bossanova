package bossmcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/githubcallback"
	"google.golang.org/protobuf/proto"
)

// jsonResult marshals v to a single text content block (JSON), which is how
// every tool returns structured data to the model.
func jsonResult(v any) (*mcp.CallToolResult, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal result: %w", err)
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(b)}},
	}, nil
}

// errorResult converts a backend error into a non-protocol tool error result,
// so the model sees the failure rather than the transport breaking.
func errorResult(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
	}
}

// redactRepo returns a copy of r with the write-only secret credentials cleared,
// so no MCP read or mutating tool ever echoes a repo's raw API keys back to the
// caller. The Repo proto carries linear_api_key/sentry_api_key as plain fields,
// and every Repo-returning tool serializes the message directly, so redaction
// must happen at the MCP layer. sentry_org is an org slug, not a secret, and is
// preserved. The source message is never mutated (proto.Clone makes a copy).
func redactRepo(r *pb.Repo) *pb.Repo {
	if r == nil {
		return nil
	}
	clone, ok := proto.Clone(r).(*pb.Repo)
	if !ok {
		// Unreachable (proto.Clone of a *pb.Repo always yields *pb.Repo), but
		// fail closed for a secret scrubber: never return the unredacted source.
		return &pb.Repo{Id: r.GetId(), DisplayName: r.GetDisplayName()}
	}
	clone.LinearApiKey = ""
	clone.SentryApiKey = ""
	return clone
}

// redactRepos applies redactRepo to every element, returning a new slice.
func redactRepos(rs []*pb.Repo) []*pb.Repo {
	out := make([]*pb.Repo, len(rs))
	for i, r := range rs {
		out[i] = redactRepo(r)
	}
	return out
}

// redactCallback returns a copy of cb with the secret message body cleared, so
// no MCP tool ever echoes a callback's delivery prompt back to the caller. The
// GithubCallback proto carries `message` as a plain field and every
// callback-returning tool serializes the message directly, so redaction must
// happen at the MCP layer — exactly like redactRepo does for repo API keys. The
// source message is never mutated (proto.Clone makes a copy).
func redactCallback(cb *pb.GithubCallback) *pb.GithubCallback {
	if cb == nil {
		return nil
	}
	clone, ok := proto.Clone(cb).(*pb.GithubCallback)
	if !ok {
		// Unreachable (proto.Clone of a *pb.GithubCallback always yields
		// *pb.GithubCallback), but fail closed for a secret scrubber: never
		// return the unredacted source.
		return &pb.GithubCallback{Id: cb.GetId()}
	}
	clone.Message = ""
	return clone
}

// redactCallbacks applies redactCallback to every element, returning a new slice.
func redactCallbacks(cbs []*pb.GithubCallback) []*pb.GithubCallback {
	out := make([]*pb.GithubCallback, len(cbs))
	for i, cb := range cbs {
		out[i] = redactCallback(cb)
	}
	return out
}

// redactBroadcast returns a copy of bc with the secret message body cleared, so
// no MCP tool ever echoes a broadcast's delivery prompt back to the caller. The
// Broadcast proto carries `message` as a plain field (populated only on the
// delivering owner's path) and every broadcast-returning tool serializes the
// message directly, so redaction must happen at the MCP layer — exactly like
// redactCallback does for a callback body. The source message is never mutated
// (proto.Clone makes a copy).
func redactBroadcast(bc *pb.Broadcast) *pb.Broadcast {
	if bc == nil {
		return nil
	}
	clone, ok := proto.Clone(bc).(*pb.Broadcast)
	if !ok {
		// Unreachable (proto.Clone of a *pb.Broadcast always yields
		// *pb.Broadcast), but fail closed for a secret scrubber: never return
		// the unredacted source.
		return &pb.Broadcast{Id: bc.GetId()}
	}
	clone.Message = ""
	return clone
}

// redactBroadcasts applies redactBroadcast to every element, returning a new slice.
func redactBroadcasts(bcs []*pb.Broadcast) []*pb.Broadcast {
	out := make([]*pb.Broadcast, len(bcs))
	for i, bc := range bcs {
		out[i] = redactBroadcast(bc)
	}
	return out
}

// The broadcast tool descriptions are an agent's ONLY documentation at call
// time, so the three facts that make broadcasts safe to use are stated on every
// tool they are true for, from these shared constants rather than by hand (a
// re-typed caveat is a caveat that drifts).
const (
	// broadcastSignalRule is the caveat every broadcast tool carries: a
	// delivered broadcast is a claim, not evidence.
	broadcastSignalRule = "A broadcast is a signal, not proof — a receiving agent must verify any state it claims rather than trusting it."

	// broadcastWakeCost states the cost of a wide audience. Only tools that
	// cause a delivery say it.
	broadcastWakeCost = "Delivery WAKES every target chat, so a broad selector has a real cost."

	// broadcastSelectorGrammar is the one-line selector grammar with an
	// example. Only tools that accept a selector say it.
	broadcastSelectorGrammar = `Selector grammar: inside one clause, comma-separated key:value terms AND across different keys and OR when a key repeats (agent:claude,agent:codex); "+" ORs whole clauses, e.g. repo:<id>,agent:claude+account:<id>. Valid keys are chat, session, repo, agent, account and daemon. An empty selector is an error, never "everyone".`

	// noteRepoIDRouting is the repo_id contract shared by the three id-keyed
	// note tools (get_note, update_note, delete_note), stated from one constant
	// so the three cannot drift apart. It carries all three facts an agent can
	// only learn here, each of which fails in a way no single-daemon test sees:
	// repo_id is REQUIRED even where it is ignored (the generated schema marks
	// it so and the SDK rejects an omitted value before the handler runs); it is
	// the daemon-local repos.id, not an origin URL; and — per
	// ProxyGetNoteRequest's contract in orchestrator.proto — it ROUTES BUT DOES
	// NOT SCOPE, so it must never be presented as a safety check.
	noteRepoIDRouting = "repo_id is REQUIRED even against a local daemon, which resolves the note from the id alone and ignores the value; the hosted gateway needs it to pick the daemon holding the note. It is the daemon-local repo id that list_repos and resolve_context return, NOT a git origin URL — an origin URL resolves to NotFound. It ROUTES BUT DOES NOT SCOPE: the daemon addresses the note by id alone and never checks that the note belongs to repo_id, so naming one repo while passing an id that lives in another acts on that other note. Do not treat repo_id as a safety check — the id is what selects the note."

	// noteRepoIDRoutingField is the same contract, worded for the repo_id field
	// description on those three tools' argument structs. A struct tag must be a
	// literal, so this constant cannot be interpolated into one — it exists to
	// keep the three hand-copied tags reviewable against a single source.
	noteRepoIDRoutingField = "the note's owning repo id (required, even on a local daemon that ignores it; the hosted gateway routes by it). Use the daemon-local repo id list_repos/resolve_context return, NOT a git origin URL — an origin URL resolves to NotFound. It routes but does NOT scope: the id alone selects the note, and a mismatched repo_id is not checked, so this is not a safety check"
)

// boolPtr returns a pointer to b. The MCP SDK's ToolAnnotations.DestructiveHint
// is a *bool (meaningful only when ReadOnlyHint is false), so destructive tools
// must set it explicitly.
func boolPtr(b bool) *bool { return &b }

// RegisterTools installs every bossanova MCP tool on server. When opts.ReadOnly
// is set, only read-only tools are registered; when opts.Only is non-nil, only
// the named tools are registered (see Options.Only).
func RegisterTools(server *mcp.Server, backend Backend, opts Options) {
	registerReadTools(server, backend, opts)
	if opts.ReadOnly {
		return
	}
	registerMutatingTools(server, backend, opts)
	registerDestructiveTools(server, backend, opts)
}

// addTool registers a tool unless opts.Only is non-nil and omits its name. It
// is the single choke point that makes Options.Only effective: every tool
// registration in this package flows through it.
func addTool[In, Out any](s *mcp.Server, opts Options, t *mcp.Tool, h mcp.ToolHandlerFor[In, Out]) {
	if opts.Only != nil && !opts.Only[t.Name] {
		return
	}
	mcp.AddTool(s, t, h)
}

func registerReadTools(server *mcp.Server, backend Backend, opts Options) {
	addTool(server, opts, &mcp.Tool{
		Name:        "list_sessions",
		Description: "List bossanova sessions, optionally filtered by repo, states, or archived flag.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args ListSessionsArgs) (*mcp.CallToolResult, any, error) {
		req := &pb.ListSessionsRequest{IncludeArchived: args.IncludeArchived}
		if args.RepoID != "" {
			repoID := args.RepoID
			req.RepoId = &repoID
		}
		for _, s := range args.States {
			req.States = append(req.States, pb.SessionState(pb.SessionState_value[s]))
		}
		sessions, err := backend.ListSessions(ctx, req)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(sessions)
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "resolve_context",
		Description: "Resolve the repo and session for a working directory (if inside a registered repo or session worktree).",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args WorkingDirArgs) (*mcp.CallToolResult, any, error) {
		out, err := backend.ResolveContext(ctx, args.WorkingDir)
		if err != nil {
			return errorResult(err), nil, nil
		}
		// resolve_context embeds a *pb.Repo, which the daemon populates with the
		// repo's API keys — redact them like every other Repo-returning tool.
		if out.GetRepo() != nil {
			out.Repo = redactRepo(out.Repo)
		}
		r, err := jsonResult(out)
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "validate_repo_path",
		Description: "Validate that a local path is a usable git repo, returning origin URL and default branch.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args LocalPathArgs) (*mcp.CallToolResult, any, error) {
		out, err := backend.ValidateRepoPath(ctx, args.LocalPath)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(out)
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "list_repos",
		Description: "List every registered repository.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ NoArgs) (*mcp.CallToolResult, any, error) {
		out, err := backend.ListRepos(ctx)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(redactRepos(out))
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "list_repo_prs",
		Description: "List open pull requests for a registered repository.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args RepoIDArgs) (*mcp.CallToolResult, any, error) {
		out, err := backend.ListRepoPRs(ctx, args.RepoID)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(out)
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "list_tracker_issues",
		Description: "List issues from an external tracker (e.g. linear, sentry) for a repo, optionally filtered by query.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args ListTrackerIssuesArgs) (*mcp.CallToolResult, any, error) {
		out, err := backend.ListTrackerIssues(ctx, args.RepoID, args.Query, args.Source)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(out)
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name: "get_session",
		// `state` and `last_check_state` were read as a push signal on a real
		// epic run and moved a driver onto merge rails against a branch that
		// still held only its bootstrap commit. A tool description is the only
		// contract an agent caller ever reads, so the caveat lives here — one
		// sentence plus the oracle, because this text is per-turn rent.
		Description: "Get a session by id. `state`/`last_check_state` carry no push information: they move when " +
			"the daemon re-polls existing checks, while the remote branch is unchanged. Push " +
			"oracle (fetch first): `git rev-list --count origin/<base>..origin/<branch>`.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args IDArgs) (*mcp.CallToolResult, any, error) {
		out, err := backend.GetSession(ctx, args.ID)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(out)
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "list_chats",
		Description: "List all agent chats in a session (each with its agent_session_id) — the siblings you can start with start_chat, message, read, or delete.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args SessionIDArgs) (*mcp.CallToolResult, any, error) {
		out, err := backend.ListChats(ctx, args.SessionID)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(out)
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name: "get_chat_statuses",
		// `last_output_at` reads as agent liveness and is not one: it advances
		// on ANY pane change, and every chat first seen in one poll tick is
		// seeded with the same instant. Name the three fields that DO
		// discriminate rather than only warning about this one.
		Description: "Live status of a session's chats. `last_output_at` is a floor, not liveness: any pane change " +
			"advances it (a spinner redraw keeps it fresh) and chats seeded in one poll tick share an instant, " +
			"so it can be identical across chats. Gate on `spinner_present`, `last_substantive_output_at`, " +
			"`last_output_seeded`.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args SessionIDArgs) (*mcp.CallToolResult, any, error) {
		out, err := backend.GetChatStatuses(ctx, args.SessionID)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(out)
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name: "get_session_statuses",
		// Aggregate-only by construction: SessionStatusEntry carries no
		// timestamps at all. Say so, repeat the floor caveat because this is the
		// tool a driver polling many sessions reaches for first, and leave the
		// three discriminating field names to get_chat_statuses, which is where
		// this points and the only place they can actually be read.
		Description: "Best status across a session's chats; aggregate only — no `last_output_at`, no liveness fields. " +
			"In `get_chat_statuses`, `last_output_at` is a floor, not liveness: a spinner redraw advances it " +
			"and it can be identical across chats — use the liveness fields it returns.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args SessionIDsArgs) (*mcp.CallToolResult, any, error) {
		out, err := backend.GetSessionStatuses(ctx, args.SessionIDs)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(out)
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "get_chat_transcript",
		Description: "Return the conversation transcript and final assistant text for one chat (by agent_session_id) — read or interrogate any chat in a session.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args GetChatTranscriptArgs) (*mcp.CallToolResult, any, error) {
		req := &pb.GetChatTranscriptRequest{
			AgentSessionId: args.AgentSessionID,
			SessionId:      args.SessionID,
			MaxMessages:    args.MaxMessages,
		}
		out, err := backend.GetChatTranscript(ctx, req)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(out)
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "list_check_snapshots",
		Description: "List recent CI check snapshots for a session (most recent first).",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args ListCheckSnapshotsArgs) (*mcp.CallToolResult, any, error) {
		out, err := backend.ListCheckSnapshots(ctx, args.SessionID, args.Limit)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(out)
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "repair_doctor",
		Description: "Run the daemon repair-doctor diagnostics and return the checks and recent agent logs.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ NoArgs) (*mcp.CallToolResult, any, error) {
		out, err := backend.RepairDoctor(ctx)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(out)
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "list_agents",
		Description: "List the agent-runner plugins currently loaded by the daemon.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ NoArgs) (*mcp.CallToolResult, any, error) {
		out, err := backend.ListAgents(ctx)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(out)
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "list_plugins",
		Description: "List every plugin the daemon attempted to load this run, including disabled and failed entries.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ NoArgs) (*mcp.CallToolResult, any, error) {
		out, err := backend.ListPlugins(ctx)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(out)
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "list_cron_jobs",
		Description: "List every scheduled cron job.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ NoArgs) (*mcp.CallToolResult, any, error) {
		out, err := backend.ListCronJobs(ctx)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(out)
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "get_cron_job",
		Description: "Get a single cron job by id.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args IDArgs) (*mcp.CallToolResult, any, error) {
		out, err := backend.GetCronJob(ctx, args.ID)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(out)
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "list_accounts",
		Description: "List registry accounts and cached usage metadata, optionally filtered by provider. Credentials are never returned. Set refresh to force a live usage probe before returning.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args ListAccountsArgs) (*mcp.CallToolResult, any, error) {
		out, err := backend.ListAccounts(ctx, args.Provider, args.Refresh)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(out)
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "get_settings",
		Description: "Get the daemon's global settings: the TUI-editable subset of settings.json (worktree base dir, poll interval, default agent, event/error tracing, PostHog token/host) plus each configured agent's enabled flag and dynamic config map. Per-agent allowed values/types for update_settings are discoverable via list_agents.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ NoArgs) (*mcp.CallToolResult, any, error) {
		out, err := backend.GetSettings(ctx)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(out)
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "list_github_callbacks",
		Description: "List registered GitHub PR callbacks, optionally filtered by target chat, repository, PR number, trigger, or state. The delivery message body is a secret and is never included in the output.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args ListGithubCallbacksArgs) (*mcp.CallToolResult, any, error) {
		req := &pb.ListGithubCallbacksRequest{}
		if args.TargetChatID != "" {
			v := args.TargetChatID
			req.TargetChatId = &v
		}
		if args.RepoOwner != "" {
			v := args.RepoOwner
			req.RepoOwner = &v
		}
		if args.RepoName != "" {
			v := args.RepoName
			req.RepoName = &v
		}
		if args.PRNumber != 0 {
			v := args.PRNumber
			req.PrNumber = &v
		}
		if args.Trigger != "" {
			trigger, err := githubcallback.ValidateTrigger(args.Trigger)
			if err != nil {
				return errorResult(err), nil, nil
			}
			s := string(trigger)
			req.Trigger = &s
		}
		if args.State != "" {
			v := args.State
			req.State = &v
		}
		out, err := backend.ListGithubCallbacks(ctx, req)
		if err != nil {
			return errorResult(err), nil, nil
		}
		// The message body is a secret: scrub it from every returned callback.
		r, err := jsonResult(redactCallbacks(out))
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name: "list_notes",
		// Everything this description used to restate about the individual
		// filters — tags is OR-not-all-of and normalised, search is a literal
		// substring, limit<=0 is unlimited, repo_id is the daemon-local id and
		// not an origin URL — is already carried, per argument, by the
		// ListNotesArgs jsonschema tags below, which the caller reads in the same
		// tool definition. Saying it twice doubles the per-turn rent for nothing.
		// What is kept here is only what no single field can say: the tool's
		// purpose, that bodies come back unredacted, and that a short hosted
		// result is not evidence of absence.
		Description: "List notes — durable free-text later runs can harvest — optionally filtered by repo, provenance, " +
			"tags, or a body substring. " +
			"Bodies are returned IN FULL: a note is not a secret, it is the payload it exists to carry. Every " +
			"filter is optional; a blank value is treated as omitted. Against the " +
			"hosted gateway an omitted repo_id fans the list out across your reachable daemons and SKIPS any " +
			"that are offline or slow, so an empty or short result is not proof no further notes exist.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args ListNotesArgs) (*mcp.CallToolResult, any, error) {
		req := &pb.ListNotesRequest{Tags: args.Tags, Limit: args.Limit}
		if args.RepoID != "" {
			v := args.RepoID
			req.RepoId = &v
		}
		if args.SessionID != "" {
			v := args.SessionID
			req.SessionId = &v
		}
		if args.ChatID != "" {
			v := args.ChatID
			req.ChatId = &v
		}
		if args.Search != "" {
			v := args.Search
			req.Search = &v
		}
		out, err := backend.ListNotes(ctx, req)
		if err != nil {
			return errorResult(err), nil, nil
		}
		// No redaction pass — deliberately. A note body is the payload the
		// caller asked for, not a secret like a callback or broadcast message,
		// so it is returned in full on every read surface.
		r, err := jsonResult(out)
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name: "get_note",
		Description: "Get a single note by id, including its body and normalised tags. The body is returned in " +
			"full — it is the payload the note exists to carry, not a secret. " + noteRepoIDRouting,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args GetNoteArgs) (*mcp.CallToolResult, any, error) {
		out, err := backend.GetNote(ctx, args.RepoID, args.ID)
		if err != nil {
			return errorResult(err), nil, nil
		}
		// No redaction pass: the note body is deliberately returned in full,
		// because it is the payload the caller asked for.
		r, err := jsonResult(out)
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name: "list_broadcasts",
		Description: "List broadcasts — one message addressed at the set of chats a selector resolved to — optionally filtered by lifecycle state (pending, resolved, completed, expired, canceled), the chat that sent them, or a chat they were addressed to. Rows carry ids, the selector, state and timestamps only: the message body is stored verbatim and is a secret, and is never returned by this tool. " +
			broadcastSignalRule +
			" A broadcast's audience was resolved ONCE, at send time, so a chat created afterwards is never among its targets.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args ListBroadcastsArgs) (*mcp.CallToolResult, any, error) {
		req := &pb.ListBroadcastsRequest{Limit: args.Limit}
		if args.State != "" {
			v := args.State
			req.State = &v
		}
		if args.OriginChatID != "" {
			v := args.OriginChatID
			req.OriginChatId = &v
		}
		if args.TargetChatID != "" {
			v := args.TargetChatID
			req.TargetChatId = &v
		}
		out, err := backend.ListBroadcasts(ctx, req)
		if err != nil {
			return errorResult(err), nil, nil
		}
		// The message body is a secret: scrub it from every returned broadcast.
		r, err := jsonResult(redactBroadcasts(out))
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name: "list_broadcast_subscriptions",
		Description: "List standing broadcast subscriptions — rules that broadcast one message when a session completes, errors, or settles — optionally filtered by owner session, the chat that registered them, state, or trigger event. Rows carry ids, trigger and state only: the registered message body is a secret and is never returned by this tool (the record has no body field at all). " +
			broadcastSignalRule +
			" A subscription's audience is resolved at FIRE time, not at registration, and firing wakes every chat it then resolves to.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args ListBroadcastSubscriptionsArgs) (*mcp.CallToolResult, any, error) {
		req := &pb.ListBroadcastSubscriptionsRequest{Limit: args.Limit}
		if args.OwnerSessionID != "" {
			v := args.OwnerSessionID
			req.OwnerSessionId = &v
		}
		if args.OriginChatID != "" {
			v := args.OriginChatID
			req.OriginChatId = &v
		}
		if args.State != "" {
			v := args.State
			req.State = &v
		}
		if args.TriggerEvent != "" {
			v := args.TriggerEvent
			req.TriggerEvent = &v
		}
		out, err := backend.ListBroadcastSubscriptions(ctx, req)
		if err != nil {
			return errorResult(err), nil, nil
		}
		// No redaction pass is possible or needed: BroadcastSubscription
		// deliberately carries no message field, so the body cannot reach here.
		r, err := jsonResult(out)
		return r, nil, err
	})
}

// ListBroadcastsArgs is the typed argument struct for list_broadcasts. Every
// field is an optional filter; an unset field is not constrained. The message
// body is a secret and is never a filter or an output field.
type ListBroadcastsArgs struct {
	State        string `json:"state,omitempty" jsonschema:"only broadcasts in this lifecycle state (pending, resolved, completed, expired, canceled); an unrecognised value is rejected, not treated as matching nothing"`
	OriginChatID string `json:"origin_chat_id,omitempty" jsonschema:"only broadcasts sent from this chat id; operator-issued broadcasts carry no origin and never match"`
	TargetChatID string `json:"target_chat_id,omitempty" jsonschema:"only broadcasts addressed to this chat id — what this chat has been sent"`
	Limit        int32  `json:"limit,omitempty" jsonschema:"max broadcasts to return (zero or negative = unlimited)"`
}

// ListBroadcastSubscriptionsArgs is the typed argument struct for
// list_broadcast_subscriptions. Every field is an optional filter; an unset
// field is not constrained.
type ListBroadcastSubscriptionsArgs struct {
	OwnerSessionID string `json:"owner_session_id,omitempty" jsonschema:"only subscriptions watching this session id"`
	OriginChatID   string `json:"origin_chat_id,omitempty" jsonschema:"only subscriptions registered from this chat id; operator-issued subscriptions carry no origin and never match"`
	State          string `json:"state,omitempty" jsonschema:"only subscriptions in this lifecycle state (active, fired, canceled, expired)"`
	TriggerEvent   string `json:"trigger_event,omitempty" jsonschema:"only subscriptions with exactly this trigger (completed, errored, settled); it does not apply the settled-matches-both rule"`
	Limit          int32  `json:"limit,omitempty" jsonschema:"max subscriptions to return (zero or negative = unlimited)"`
}

// ListAccountsArgs is the typed argument struct for list_accounts.
type ListAccountsArgs struct {
	Provider string `json:"provider,omitempty" jsonschema:"optional provider filter (claude|codex); empty returns all"`
	Refresh  bool   `json:"refresh,omitempty" jsonschema:"force a live usage probe of each account before returning (default: cached values plus age)"`
}

// ListSessionsArgs is the typed argument struct for list_sessions.
type ListSessionsArgs struct {
	RepoID          string   `json:"repo_id,omitempty" jsonschema:"the repo id to filter by"`
	States          []string `json:"states,omitempty" jsonschema:"session state enum names to filter by (e.g. SESSION_STATE_RUNNING)"`
	IncludeArchived bool     `json:"include_archived,omitempty" jsonschema:"include archived sessions"`
}

// ListGithubCallbacksArgs is the typed argument struct for list_github_callbacks.
// Every field is an optional filter; an unset field is not constrained. The
// delivery message body is a secret and is never a filter or an output field.
type ListGithubCallbacksArgs struct {
	TargetChatID string `json:"target_chat_id,omitempty" jsonschema:"only callbacks targeting this chat id"`
	RepoOwner    string `json:"repo_owner,omitempty" jsonschema:"only callbacks on this repository owner (lowercase)"`
	RepoName     string `json:"repo_name,omitempty" jsonschema:"only callbacks on this repository name (lowercase)"`
	PRNumber     int32  `json:"pr_number,omitempty" jsonschema:"only callbacks watching this PR number"`
	Trigger      string `json:"trigger,omitempty" jsonschema:"only callbacks with this trigger; one of merged, closed, checks_passed, checks_failed, ready_for_review, checks_passed_ready"`
	State        string `json:"state,omitempty" jsonschema:"only callbacks in this lifecycle state (e.g. active, delivered, expired)"`
}

// ListNotesArgs is the typed argument struct for list_notes. Every field is an
// optional filter. The daemon applies repo_id/session_id/chat_id whenever the
// request field is SET — including when set to the empty string, where it
// matches nothing. This tool therefore marshals only non-empty strings into the
// request's optional pointers and leaves a blank one unset, so a blank argument
// behaves as an omitted filter rather than a fail-closed one.
type ListNotesArgs struct {
	RepoID    string   `json:"repo_id,omitempty" jsonschema:"only notes owned by this repo id — the daemon-local repo id list_repos/resolve_context return, NOT a git origin URL. Omit it to leave the repo unconstrained; on the hosted gateway an omitted repo_id fans the list out across your reachable daemons"`
	SessionID string   `json:"session_id,omitempty" jsonschema:"only notes recorded by this session id (provenance; the session may no longer exist)"`
	ChatID    string   `json:"chat_id,omitempty" jsonschema:"only notes recorded by this chat id (provenance)"`
	Tags      []string `json:"tags,omitempty" jsonschema:"only notes carrying ANY of these tags (OR, not all-of); entries are trimmed and lowercased to match the stored form"`
	Search    string   `json:"search,omitempty" jsonschema:"literal substring match on the note body (SQL wildcards are matched literally; ASCII-only case folding); blank means no search"`
	Limit     int32    `json:"limit,omitempty" jsonschema:"max notes to return (zero or negative = unlimited)"`
}

// GetNoteArgs is the typed argument struct for get_note. repo_id is the owning
// repository: the hosted gateway routes the read by it, while the local socket
// adapter ignores it (its own daemon owns every note, so the id resolves it).
// It carries no `omitempty`, so the generated schema marks it REQUIRED and an
// omitted value fails validation before the handler runs — hence "required" in
// the field description, which must not read as optional just because the local
// adapter ignores the value. Keep the tag in step with noteRepoIDRoutingField.
type GetNoteArgs struct {
	RepoID string `json:"repo_id" jsonschema:"the note's owning repo id (required, even on a local daemon that ignores it; the hosted gateway routes by it). Use the daemon-local repo id list_repos/resolve_context return, NOT a git origin URL — an origin URL resolves to NotFound. It routes but does NOT scope: the id alone selects the note, and a mismatched repo_id is not checked, so this is not a safety check"`
	ID     string `json:"id" jsonschema:"the note id"`
}

// NoArgs is the typed argument struct for tools that take no input.
type NoArgs struct{}

// IDArgs is the typed argument struct for tools keyed by a single id.
type IDArgs struct {
	ID string `json:"id" jsonschema:"the resource id"`
}

// RepoIDArgs is the typed argument struct for tools keyed by a repo id.
type RepoIDArgs struct {
	RepoID string `json:"repo_id" jsonschema:"the repo id"`
}

// SessionIDArgs is the typed argument struct for tools keyed by a session id.
type SessionIDArgs struct {
	SessionID string `json:"session_id" jsonschema:"the session id"`
}

// SessionIDsArgs is the typed argument struct for tools keyed by a list of session ids.
type SessionIDsArgs struct {
	SessionIDs []string `json:"session_ids" jsonschema:"the session ids to query"`
}

// WorkingDirArgs is the typed argument struct for resolve_context.
type WorkingDirArgs struct {
	WorkingDir string `json:"working_dir" jsonschema:"the working directory to resolve"`
}

// LocalPathArgs is the typed argument struct for validate_repo_path.
type LocalPathArgs struct {
	LocalPath string `json:"local_path" jsonschema:"the local filesystem path to validate"`
}

// ListTrackerIssuesArgs is the typed argument struct for list_tracker_issues.
type ListTrackerIssuesArgs struct {
	RepoID string `json:"repo_id" jsonschema:"the repo id"`
	Query  string `json:"query,omitempty" jsonschema:"optional search query"`
	Source string `json:"source,omitempty" jsonschema:"tracker source, e.g. linear or sentry"`
}

// ListCheckSnapshotsArgs is the typed argument struct for list_check_snapshots.
type ListCheckSnapshotsArgs struct {
	SessionID string `json:"session_id" jsonschema:"the session id"`
	Limit     int32  `json:"limit,omitempty" jsonschema:"max snapshots to return (0 = server default)"`
}

// GetChatTranscriptArgs is the typed argument struct for get_chat_transcript.
type GetChatTranscriptArgs struct {
	AgentSessionID string `json:"agent_session_id" jsonschema:"the agent session UUID"`
	SessionID      string `json:"session_id,omitempty" jsonschema:"the session id (for authz when available)"`
	MaxMessages    int32  `json:"max_messages,omitempty" jsonschema:"max messages to return (0 = server default)"`
}
