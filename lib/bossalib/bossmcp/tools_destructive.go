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
	registerMergeSessionTool(server, backend, opts)
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
		Description: "Permanently delete one agent chat (by agent_session_id), leaving sibling chats and the session itself intact. Destructive — requires confirm:true.",
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

	addTool(server, opts, &mcp.Tool{
		Name:        "delete_github_callback",
		Description: "Permanently delete a registered GitHub PR callback by id, before it fires. Destructive — requires confirm:true.",
		Annotations: destructiveAnnotations(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args DeleteGithubCallbackArgs) (*mcp.CallToolResult, any, error) {
		if r := requireConfirm(args.Confirm, "delete_github_callback"); r != nil {
			return r, nil, nil
		}
		// target_chat_id lets the hosted gateway route the delete to the daemon
		// that owns the callback; the local socket adapter ignores it.
		if err := backend.DeleteGithubCallback(ctx, args.TargetChatID, args.ID); err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(map[string]string{"deleted_github_callback": args.ID})
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name: "delete_broadcast",
		Description: "Delete a broadcast and its delivery records by id, so nothing further is SCHEDULED for it. This is not a recall: deliveries already made stand, and a delivery a worker had already claimed is still sent (one attempt window), so a target may be woken after this returns. Idempotent: deleting an unknown id succeeds, so a retry is always safe. The message body is never returned by this tool. " +
			broadcastSignalRule +
			" Destructive — requires confirm:true.",
		Annotations: destructiveAnnotations(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args ConfirmIDArgs) (*mcp.CallToolResult, any, error) {
		if r := requireConfirm(args.Confirm, "delete_broadcast"); r != nil {
			return r, nil, nil
		}
		if err := backend.DeleteBroadcast(ctx, args.ID); err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(map[string]string{"deleted_broadcast": args.ID})
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name: "delete_broadcast_subscription",
		Description: "Retire a standing broadcast subscription by id so it never fires. It CANCELS rather than erases: the row survives as history in the canceled state and still appears in an unfiltered list_broadcast_subscriptions, so seeing it listed afterwards is not a failed delete. A subscription that had already fired keeps the broadcast it produced. Idempotent: an unknown id, and one already canceled, fired or expired, both succeed, so a retry is always safe. The registered message body is never returned by this tool. " +
			broadcastSignalRule +
			" Destructive — requires confirm:true.",
		Annotations: destructiveAnnotations(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args ConfirmIDArgs) (*mcp.CallToolResult, any, error) {
		if r := requireConfirm(args.Confirm, "delete_broadcast_subscription"); r != nil {
			return r, nil, nil
		}
		if err := backend.DeleteBroadcastSubscription(ctx, args.ID); err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(map[string]string{"deleted_broadcast_subscription": args.ID})
		return r, nil, err
	})

	addTool(server, opts, &mcp.Tool{
		Name:        "delete_note",
		Description: "Permanently delete a note by id: the row and its tags are erased, not archived, and the body cannot be recovered. Idempotent: deleting an unknown id succeeds, so a retry is always safe. " + noteRepoIDRouting + " Confirm the ID, not the repo. Destructive — requires confirm:true.",
		Annotations: destructiveAnnotations(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args DeleteNoteArgs) (*mcp.CallToolResult, any, error) {
		if r := requireConfirm(args.Confirm, "delete_note"); r != nil {
			return r, nil, nil
		}
		// repo_id lets the hosted gateway route the delete to the daemon that
		// owns the note; the local socket adapter ignores it.
		if err := backend.DeleteNote(ctx, args.RepoID, args.ID); err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(map[string]string{"deleted_note": args.ID})
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

// mergeSessionResult is the merge_session success payload: the session exactly
// as every other session tool returns it, plus an optional detail sibling.
//
// The session is embedded, not nested under a "session" key, so encoding/json
// promotes its fields to the top level and the payload keeps the same shape as
// close_session, archive_session and resurrect_session — a caller reading
// .id or .pr_number off a merge_session result still reads it off the top
// level. With an empty detail the payload is byte-identical to close_session's
// for the same session, which is what TestMergeSessionSessionShapeUnchanged
// pins by comparing the two tools' real output over the same transport.
//
// Embedding is only safe because *pb.Session does not implement json.Marshaler
// (protoc-gen-go generates no MarshalJSON); if it ever did, that method would be
// promoted here and would silently drop Detail. Session also has no top-level
// "detail" field of its own, so the sibling key cannot collide.
//
// detail is omitted when empty so its presence is meaningful, rather than a
// field every caller has to test against "".
type mergeSessionResult struct {
	*pb.Session
	Detail string `json:"detail,omitempty"`
}

// registerMergeSessionTool installs merge_session. It does not go through
// registerDestructiveSessionTool because merge is the one session op with a
// detail to carry — the daemon's note about what it actually did, most
// importantly a merge-strategy substitution (a rebase that was refused and
// squashed instead). Widening the shared helper for this one caller would force
// three unrelated tools to thread an always-empty string, so the confirm gate,
// argument struct, annotations and error path are mirrored here instead. A merge
// *refusal* stays an error, not a detail: errorResult passes err.Error()
// verbatim, so a gate refusal and the MERGE_STRATEGY_INCOMPATIBLE token still
// reach the caller intact.
func registerMergeSessionTool(server *mcp.Server, backend Backend, opts Options) {
	addTool(server, opts, &mcp.Tool{
		Name: "merge_session",
		// The optional `detail` key is deliberately NOT described here. The tool
		// surface is pinned byte-for-byte by TestToolSurfaceSizeRatchet, which sits
		// at zero headroom and refuses growth in either direction — every byte of
		// every description is resident in the cached prompt prefix and re-paid on
		// each turn of each session. Its remedy names this case exactly: move
		// situational detail out of the description. So `detail` is documented in
		// docs/mcp.md and services/docs/docs/guides/mcp.md instead.
		Description: "Merge a session's pull request. Destructive — requires confirm:true.",
		Annotations: destructiveAnnotations(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args ConfirmIDArgs) (*mcp.CallToolResult, any, error) {
		if r := requireConfirm(args.Confirm, "merge_session"); r != nil {
			return r, nil, nil
		}
		session, detail, err := backend.MergeSession(ctx, args.ID)
		if err != nil {
			return errorResult(err), nil, nil
		}
		r, err := jsonResult(mergeSessionResult{Session: session, Detail: detail})
		return r, nil, err
	})
}

// ConfirmIDArgs is the typed argument struct for confirm-gated id-keyed tools.
type ConfirmIDArgs struct {
	ID      string `json:"id" jsonschema:"the resource id"`
	Confirm bool   `json:"confirm,omitempty" jsonschema:"must be true to actually perform the destructive action"`
}

// DeleteGithubCallbackArgs is the typed argument struct for
// delete_github_callback. target_chat_id is the owning chat id; the hosted
// gateway uses it to route the delete to the daemon that owns the callback,
// while the local socket adapter ignores it (its own daemon owns every
// callback in its registry, so the id alone resolves it).
type DeleteGithubCallbackArgs struct {
	ID           string `json:"id" jsonschema:"the callback id to delete"`
	TargetChatID string `json:"target_chat_id,omitempty" jsonschema:"the callback's owning chat id (used for hosted routing; ignored for a local daemon)"`
	Confirm      bool   `json:"confirm,omitempty" jsonschema:"must be true to actually delete the callback"`
}

// DeleteNoteArgs is the typed argument struct for delete_note. repo_id is the
// note's owning repository; the hosted gateway uses it to route the delete to
// the daemon that owns the note, while the local socket adapter ignores it (its
// own daemon owns every note, so the id alone resolves it). It carries no
// `omitempty`, so the generated schema marks it REQUIRED and an omitted value
// fails validation before the handler runs — hence "required" in the field
// description, which must not read as optional just because the local adapter
// ignores the value. Keep the tag in step with noteRepoIDRoutingField.
type DeleteNoteArgs struct {
	RepoID  string `json:"repo_id" jsonschema:"the note's owning repo id (required, even on a local daemon that ignores it; the hosted gateway routes by it). Use the daemon-local repo id list_repos/resolve_context return, NOT a git origin URL — an origin URL resolves to NotFound. It routes but does NOT scope: the id alone selects the note, and a mismatched repo_id is not checked, so this is not a safety check"`
	ID      string `json:"id" jsonschema:"the note id to delete"`
	Confirm bool   `json:"confirm,omitempty" jsonschema:"must be true to actually delete the note"`
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
