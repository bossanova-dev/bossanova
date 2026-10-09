package bossmcp

import (
	"context"
	"fmt"
	"sort"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

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
	"get_settings",
	"list_github_callbacks",
	"list_notes",
	"get_note",
	"list_broadcasts",
	"list_broadcast_subscriptions",
	"list_organization_notes",
	"get_organization_note",
	"get_organization_note_quota",
}

// writeToolNames is the canonical union of mutating + destructive tool names,
// in registration order. None of these appear under Options{ReadOnly}.
var writeToolNames = []string{
	// mutating
	"register_repo", "clone_and_register_repo", "update_repo", "create_session",
	"stop_session", "pause_session", "resume_session", "retry_session",
	"update_session", "link_session_pr", "refresh_session_pr", "start_chat", "record_chat",
	"update_chat_title", "wake_chat", "report_chat_status", "create_cron_job", "update_cron_job",
	"run_cron_job_now", "add_account", "refresh_account", "update_account",
	"test_account", "send_chat_message", "switch_account", "update_settings",
	"start_repair_workflow", "register_github_callback",
	"send_broadcast", "register_broadcast_subscription",
	"create_note", "update_note",
	"create_organization_note", "update_organization_note",
	// destructive
	"remove_repo", "close_session", "merge_session", "remove_session",
	"archive_session", "resurrect_session", "delete_chat", "empty_trash",
	"delete_cron_job", "remove_account", "delete_github_callback",
	"delete_broadcast", "delete_broadcast_subscription",
	"delete_note",
	"delete_organization_note",
}

// hostedToolNames is the hosted-only tool tier, in registration order: read
// tools first, then mutating, then destructive, each grouped by family
// (triggers, then session webhooks). A family registers only under
// Options{IncludeHostedTools} with a backend that implements its interface
// (TriggerBackend, SessionWebhookBackend), so these are NOT part of
// ToolNames(), the default surface, or its size ratchet.
var hostedToolNames = []string{
	// read
	"get_trigger_catalog", "list_triggers", "get_trigger",
	"list_session_webhook_event_types", "list_session_webhooks",
	"list_session_webhook_deliveries", "get_session_webhook_delivery",
	// mutating
	"save_trigger", "test_trigger",
	"save_session_webhook", "test_session_webhook",
	// destructive
	"delete_trigger",
	"delete_session_webhook",
}

// hostedReadOnlyToolNames is the subset of hostedToolNames that registers
// under Options{ReadOnly}.
var hostedReadOnlyToolNames = []string{
	"get_trigger_catalog", "list_triggers", "get_trigger",
	"list_session_webhook_event_types", "list_session_webhooks",
	"list_session_webhook_deliveries", "get_session_webhook_delivery",
}

// HostedToolNames returns the hosted-only tool names (see
// Options.IncludeHostedTools), in registration order. ToolNames() never
// includes them. The returned slice is a copy.
func HostedToolNames() []string {
	return append([]string{}, hostedToolNames...)
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

// ToolDefinitions returns the full definitions — name, description, input
// schema and annotations — of every tool RegisterTools installs under opts, in
// ToolNames() order (then HostedToolNames() order when opts.IncludeHostedTools
// is set): the same tools a tools/list response carries. (The SDK
// lists tools alphabetically; they are reordered here so callers get the
// canonical inventory order, read-only tools first.)
//
// It runs entirely in-process: the tools are registered on a throwaway server
// against definitionsBackend and listed over mcp.NewInMemoryTransports(), so no
// daemon, socket or subprocess is involved and no tool handler is ever invoked. Callers
// that render the tool surface for another host (the Hermes plugin renderer)
// use it to ship schemas that match the binary they were built from.
//
// It returns an error when tools/list is paginated, because one page would
// silently under-report the surface.
func ToolDefinitions(ctx context.Context, opts Options) ([]*mcp.Tool, error) {
	server := mcp.NewServer(&mcp.Implementation{Name: "bossanova", Version: "definitions"}, nil)
	RegisterTools(server, definitionsBackend{}, opts)

	client := mcp.NewClient(&mcp.Implementation{Name: "bossanova-definitions", Version: "definitions"}, nil)
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		return nil, fmt.Errorf("connect in-memory server: %w", err)
	}
	defer func() { _ = serverSession.Close() }()
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		return nil, fmt.Errorf("connect in-memory client: %w", err)
	}
	defer func() { _ = clientSession.Close() }()

	res, err := clientSession.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		return nil, fmt.Errorf("list tools: %w", err)
	}
	if res.NextCursor != "" {
		return nil, fmt.Errorf("tools/list was paginated (nextCursor %q): one page is not the whole surface", res.NextCursor)
	}
	return orderByToolNames(res.Tools), nil
}

// orderByToolNames sorts tools into ToolNames() order, followed by
// HostedToolNames() order for a listing made with IncludeHostedTools. A tool
// missing from both static inventories (which TestToolNamesMatchesRegisteredSet
// forbids) sorts after every known one, keeping the SDK's relative order.
func orderByToolNames(tools []*mcp.Tool) []*mcp.Tool {
	rank := make(map[string]int, len(readOnlyToolNames)+len(writeToolNames)+len(hostedToolNames))
	for i, name := range append(ToolNames(), hostedToolNames...) {
		rank[name] = i
	}
	rankOf := func(name string) int {
		if r, ok := rank[name]; ok {
			return r
		}
		return len(rank)
	}
	out := append([]*mcp.Tool{}, tools...)
	sort.SliceStable(out, func(i, j int) bool { return rankOf(out[i].Name) < rankOf(out[j].Name) })
	return out
}

// definitionsBackend satisfies Backend for ToolDefinitions without backing any
// operation. A bare nil Backend is not enough: several registrations take a
// method value (backend.StopSession and friends), and evaluating a method value
// on a nil interface panics. Embedding the nil interface gives the struct a
// full method set whose bound values are safe to take; calling one would
// panic, but ToolDefinitions only lists tools and never calls a handler.
// It embeds every hosted family's interface too (TriggerBackend,
// SessionWebhookBackend), so ToolDefinitions(Options{IncludeHostedTools}) lists
// the whole hosted tier; without that option the embeds change nothing.
type definitionsBackend struct {
	Backend
	TriggerBackend
	SessionWebhookBackend
}
