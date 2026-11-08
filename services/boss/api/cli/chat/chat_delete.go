package chat

import (
	"context"
	"fmt"

	"github.com/recurser/boss/api/cli/cliutil"
	"github.com/recurser/boss/internal/domain/rpc"
	"github.com/spf13/cobra"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func (g *group) chatDeleteCmd() *cobra.Command {
	del := &cobra.Command{
		Use:   "delete <chat-id>",
		Short: "Delete a chat: stop its agent pane and remove it from its session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			asJSON, _ := cmd.Flags().GetBool(cliutil.JSONFlagName)
			c, err := g.newClient(cmd)
			if err != nil {
				return cliutil.EmitJSONFailure(cmd, asJSON, err)
			}
			return chatDelete(context.Background(), c, cmd, args[0])
		},
	}
	del.Flags().Bool(cliutil.JSONFlagName, false, "Emit the outcome as a machine-readable JSON envelope")
	return del
}

// chatDeleteClient is the client surface `boss chat delete` needs, narrowed so a
// test can read back the delete that reached the daemon.
type chatDeleteClient interface {
	GetSession(context.Context, string, rpc.SessionReadOptions) (*pb.Session, error)
	DeleteChat(ctx context.Context, agentSessionID string, reason pb.DeleteChatRequest_DeletionReason) error
}

// chatDeleteJSON is the `boss chat delete --json` success envelope.
type chatDeleteJSON struct {
	AgentSessionID string `json:"agent_session_id"`
	Deleted        bool   `json:"deleted"`
}

// chatDelete deletes one chat by its agent_session_id. Unlike every other chat
// verb it refuses a session id: resolving one to the session's primary chat
// would stop the session's own agent on a mistyped id.
func chatDelete(ctx context.Context, c chatDeleteClient, cmd *cobra.Command, target string) error {
	asJSON, _ := cmd.Flags().GetBool(cliutil.JSONFlagName)
	resolved, err := resolveChatTarget(ctx, c, target)
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, err)
	}
	if resolved.SessionID != "" {
		return cliutil.EmitJSONFailure(cmd, asJSON, fmt.Errorf(
			"delete takes a chat id, and %s is a session; list its chats with `boss chats %s`", target, target))
	}
	// A CLI delete is a direct request from the operator or their script. It is
	// never the cleanup reason, which the daemon gates by the chat's agent.
	if err := c.DeleteChat(ctx, resolved.AgentSessionID, pb.DeleteChatRequest_DELETION_REASON_USER_REQUESTED); err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, fmt.Errorf("delete chat: %w", err))
	}
	if asJSON {
		return cliutil.EmitJSON(cmd, chatDeleteJSON{AgentSessionID: resolved.AgentSessionID, Deleted: true})
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Chat %s deleted.\n", resolved.AgentSessionID)
	return nil
}
