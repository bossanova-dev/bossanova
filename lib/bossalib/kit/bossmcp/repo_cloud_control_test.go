package bossmcp

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestRepoCloudControlExcludedFromAllMCPSurfaces(t *testing.T) {
	for _, hosted := range []bool{false, true} {
		client := newConnectedClient(t, &fakeHostedBackend{}, Options{IncludeHostedTools: hosted})
		tools, err := client.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, tool := range tools.Tools {
			if tool.Name == "list_repo_cloud_controls" {
				t.Fatal("local grants in MCP registry")
			}
		}
		result, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_repo_cloud_controls", Arguments: map[string]any{}})
		if err == nil && (result == nil || !result.IsError) {
			t.Fatal("local grant read accepted")
		}
	}
}
