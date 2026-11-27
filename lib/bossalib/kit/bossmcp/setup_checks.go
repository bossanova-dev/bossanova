package bossmcp

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var errSetupChecksUnsupported = fmt.Errorf("setup checks are not supported by this backend")

// ListSetupChecksArgs is the typed argument struct for list_setup_checks.
type ListSetupChecksArgs struct {
	RepoID        string `json:"repo_id,omitempty" jsonschema:"optional registered repo id"`
	WorkspacePath string `json:"workspace_path,omitempty" jsonschema:"optional repository checkout path"`
}

func listSetupChecks(ctx context.Context, backend Backend, args ListSetupChecksArgs) (*mcp.CallToolResult, any, error) {
	sb, ok := backend.(SetupCheckBackend)
	if !ok {
		return errorResult(errSetupChecksUnsupported), nil, nil
	}
	out, err := sb.ListSetupChecks(ctx, args.RepoID, args.WorkspacePath)
	if err != nil {
		return errorResult(err), nil, nil
	}
	b, err := MarshalSetupChecks(out)
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(b)}},
	}, nil, nil
}
