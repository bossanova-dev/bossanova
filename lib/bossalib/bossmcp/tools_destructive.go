package bossmcp

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// requireConfirm returns an error result if confirm is false. Centralizes the
// guard so every destructive tool refuses uncommitted calls identically.
func requireConfirm(confirm bool, op string) *mcp.CallToolResult {
	if confirm {
		return nil
	}
	return errorResult(fmt.Errorf(
		"%s is destructive and requires {\"confirm\": true}; re-call with confirm set once you are sure", op))
}

// destructiveAnnotations builds the SDK annotation for a destructive tool. The
// SDK's DestructiveHint is a *bool and is only meaningful when ReadOnlyHint is
// false (the default), so we set it explicitly.
func destructiveAnnotations() *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{DestructiveHint: boolPtr(true)}
}

// registerDestructiveTools installs the destructive write tools, each gated on
// an explicit confirm:true argument.
func registerDestructiveTools(server *mcp.Server, backend Backend, opts Options) {
	addTool(server, opts, &mcp.Tool{
		Name:        "remove_repo",
		Description: "Permanently remove a registered repository. Destructive — requires confirm:true.",
		Annotations: destructiveAnnotations(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args ConfirmIDArgs) (*mcp.CallToolResult, any, error) {
		if r := requireConfirm(args.Confirm, "remove_repo"); r != nil {
			return r, nil, nil
		}
		if err := backend.RemoveRepo(ctx, args.ID); err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(map[string]string{"removed_repo": args.ID})
		return r, nil, err
	})

	registerDestructiveSessionTool(server, opts, "close_session", "Close a session (abandons its work). Destructive — requires confirm:true.", backend.CloseSession)
	registerDestructiveSessionTool(server, opts, "merge_session", "Merge a session's pull request. Destructive — requires confirm:true.", backend.MergeSession)
	registerDestructiveSessionTool(server, opts, "archive_session", "Archive a session into the trash. Destructive — requires confirm:true.", backend.ArchiveSession)
	registerDestructiveSessionTool(server, opts, "resurrect_session", "Resurrect an archived session from the trash. Destructive — requires confirm:true.", backend.ResurrectSession)

	addTool(server, opts, &mcp.Tool{
		Name:        "remove_session",
		Description: "Permanently remove a session and its worktree. Destructive — requires confirm:true.",
		Annotations: destructiveAnnotations(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args ConfirmIDArgs) (*mcp.CallToolResult, any, error) {
		if r := requireConfirm(args.Confirm, "remove_session"); r != nil {
			return r, nil, nil
		}
		if err := backend.RemoveSession(ctx, args.ID); err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(map[string]string{"removed_session": args.ID})
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "delete_chat",
		Description: "Permanently delete an agent chat. Destructive — requires confirm:true.",
		Annotations: destructiveAnnotations(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args DeleteChatArgs) (*mcp.CallToolResult, any, error) {
		if r := requireConfirm(args.Confirm, "delete_chat"); r != nil {
			return r, nil, nil
		}
		if err := backend.DeleteChat(ctx, args.AgentSessionID); err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(map[string]string{"deleted_chat": args.AgentSessionID})
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "empty_trash",
		Description: "Permanently delete archived sessions, optionally only those older than a timestamp. Destructive — requires confirm:true.",
		Annotations: destructiveAnnotations(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args EmptyTrashArgs) (*mcp.CallToolResult, any, error) {
		if r := requireConfirm(args.Confirm, "empty_trash"); r != nil {
			return r, nil, nil
		}
		req := &pb.EmptyTrashRequest{}
		if args.OlderThan != "" {
			ts, err := time.Parse(time.RFC3339, args.OlderThan)
			if err != nil {
				return errorResult(fmt.Errorf("older_than must be RFC3339 (e.g. 2026-01-02T15:04:05Z): %w", err)), nil, nil
			}
			req.OlderThan = timestamppb.New(ts)
		}
		count, err := backend.EmptyTrash(ctx, req)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(map[string]int32{"deleted_count": count})
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "delete_cron_job",
		Description: "Permanently delete a cron job. Destructive — requires confirm:true.",
		Annotations: destructiveAnnotations(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args ConfirmIDArgs) (*mcp.CallToolResult, any, error) {
		if r := requireConfirm(args.Confirm, "delete_cron_job"); r != nil {
			return r, nil, nil
		}
		if err := backend.DeleteCronJob(ctx, args.ID); err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(map[string]string{"deleted_cron_job": args.ID})
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "remove_account",
		Description: "Permanently remove an account and its stored credential. Destructive — requires confirm:true.",
		Annotations: destructiveAnnotations(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args ConfirmIDArgs) (*mcp.CallToolResult, any, error) {
		if r := requireConfirm(args.Confirm, "remove_account"); r != nil {
			return r, nil, nil
		}
		if err := backend.RemoveAccount(ctx, args.ID); err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(map[string]string{"removed_account": args.ID})
		return r, nil, err
	})
}

// registerDestructiveSessionTool installs a confirm-gated id-keyed session tool
// returning the resulting session.
func registerDestructiveSessionTool(server *mcp.Server, opts Options, name, desc string, fn func(context.Context, string) (*pb.Session, error)) {
	addTool(server, opts, &mcp.Tool{
		Name:        name,
		Description: desc,
		Annotations: destructiveAnnotations(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args ConfirmIDArgs) (*mcp.CallToolResult, any, error) {
		if r := requireConfirm(args.Confirm, name); r != nil {
			return r, nil, nil
		}
		out, err := fn(ctx, args.ID)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(out)
		return r, nil, err
	})
}

// ConfirmIDArgs is the typed argument struct for confirm-gated id-keyed tools.
type ConfirmIDArgs struct {
	ID      string `json:"id" jsonschema:"the resource id"`
	Confirm bool   `json:"confirm,omitempty" jsonschema:"must be true to actually perform the destructive action"`
}

// DeleteChatArgs is the typed argument struct for delete_chat.
type DeleteChatArgs struct {
	AgentSessionID string `json:"agent_session_id" jsonschema:"the agent session UUID to delete"`
	Confirm        bool   `json:"confirm,omitempty" jsonschema:"must be true to actually delete the chat"`
}

// EmptyTrashArgs is the typed argument struct for empty_trash.
type EmptyTrashArgs struct {
	OlderThan string `json:"older_than,omitempty" jsonschema:"only delete sessions archived before this RFC3339 timestamp"`
	Confirm   bool   `json:"confirm,omitempty" jsonschema:"must be true to actually empty the trash"`
}
