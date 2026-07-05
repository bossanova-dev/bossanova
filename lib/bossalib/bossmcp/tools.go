package bossmcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
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
		Name:        "get_session",
		Description: "Get a single session by id.",
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
		Description: "List agent chats for a session.",
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
		Name:        "get_chat_statuses",
		Description: "Get the live status of every chat in a session.",
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
		Name:        "get_session_statuses",
		Description: "Get the best live status across chats for each of the given sessions.",
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
		Description: "Return the conversation transcript and final assistant text for a chat.",
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
		Description: "List registry accounts (agent credentials), optionally filtered by provider. Credentials are never returned.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args ListAccountsArgs) (*mcp.CallToolResult, any, error) {
		out, err := backend.ListAccounts(ctx, args.Provider)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(out)
		return r, nil, err
	})
}

// ListAccountsArgs is the typed argument struct for list_accounts.
type ListAccountsArgs struct {
	Provider string `json:"provider,omitempty" jsonschema:"optional provider filter (claude|codex); empty returns all"`
}

// ListSessionsArgs is the typed argument struct for list_sessions.
type ListSessionsArgs struct {
	RepoID          string   `json:"repo_id,omitempty" jsonschema:"the repo id to filter by"`
	States          []string `json:"states,omitempty" jsonschema:"session state enum names to filter by (e.g. SESSION_STATE_RUNNING)"`
	IncludeArchived bool     `json:"include_archived,omitempty" jsonschema:"include archived sessions"`
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
