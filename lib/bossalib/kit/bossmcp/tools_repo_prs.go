package bossmcp

import (
	"context"
	"math"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// ListRepoPRsArgs is the typed argument struct for list_repo_prs. Every field
// except repo_id is optional.
type ListRepoPRsArgs struct {
	RepoID         string `json:"repo_id" jsonschema:"the repo id"`
	Author         string `json:"author,omitempty" jsonschema:"login, any case; app/ optional for bots"`
	BaseBranch     string `json:"base_branch,omitempty" jsonschema:"target branch"`
	Order          string `json:"order,omitempty" jsonschema:"PR_LIST_ORDER_{CREATED,UPDATED}_{DESC,ASC}"`
	Limit          int    `json:"limit,omitempty" jsonschema:"cap after filtering"`
	WithoutSession bool   `json:"without_session,omitempty" jsonschema:"only PRs no session owns"`
}

// listRepoPRsDescription names what the row fields mean where their names do
// not; the argument docs say what each filter does. Both are paid on every
// turn (see TestToolSurfaceSizeRatchet), so the MCP guide carries the detail.
const listRepoPRsDescription = "List a repo's open PRs, owned or not. base_branch is the target; times are RFC3339; " +
	"session_id and session_display_status name the non-archived session owning the PR by number or head branch, " +
	"empty if none."

func registerRepoPRsTool(server *mcp.Server, backend Backend, opts Options) {
	addTool(server, opts, &mcp.Tool{
		Name:        "list_repo_prs",
		Description: listRepoPRsDescription,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args ListRepoPRsArgs) (*mcp.CallToolResult, any, error) {
		req, err := listRepoPRsRequest(args)
		if err != nil {
			return errorResult(err), nil, nil
		}
		out, err := backend.ListRepoPRs(ctx, req)
		if err != nil {
			return errorResult(err), nil, nil
		}
		return hostedResult(hostedMessagesJSON(out))
	})
}

// listRepoPRsRequest maps the tool arguments onto the daemon request. MCP
// callers see owned PRs by default, each naming its session.
func listRepoPRsRequest(args ListRepoPRsArgs) (*pb.ListRepoPRsRequest, error) {
	query := &pb.PRListQuery{
		Author:      args.Author,
		BaseBranch:  args.BaseBranch,
		Limit:       int32(min(max(args.Limit, 0), math.MaxInt32)),
		OwnerFilter: pb.PRListOwnerFilter_PR_LIST_OWNER_FILTER_ALL,
	}
	if args.WithoutSession {
		query.OwnerFilter = pb.PRListOwnerFilter_PR_LIST_OWNER_FILTER_UNOWNED
	}
	if args.Order != "" {
		order, err := parseTriggerEnum("order", args.Order, "PR_LIST_ORDER_", pb.PRListOrder_value)
		if err != nil {
			return nil, err
		}
		query.Order = pb.PRListOrder(order)
	}
	return &pb.ListRepoPRsRequest{RepoId: args.RepoID, Query: query}, nil
}
