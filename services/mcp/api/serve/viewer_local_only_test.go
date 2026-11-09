package serve

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestViewerApprovalsUnavailableOverMCP(t *testing.T) {
	client := newContractClient(t, &contractBackend{})
	tools, err := client.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"get_viewer_device_status", "list_viewer_devices", "approve_viewer_device", "revoke_viewer_device"} {
		t.Run(name, func(t *testing.T) {
			for _, tool := range tools.Tools {
				if tool.Name == name {
					t.Fatalf("local-only operation advertised: %s", name)
				}
			}
			result, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: map[string]any{"viewer_device_id": "viewer", "fingerprint": "ANYTHING"}})
			if err == nil && (result == nil || !result.IsError) {
				t.Fatalf("local-only viewer operation accepted: %v", result)
			}
		})
	}
}

func TestRepoCloudControlUnavailableOverMCP(t *testing.T) {
	client := newContractClient(t, &contractBackend{})
	tools, err := client.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Name == "list_repo_cloud_controls" {
			t.Fatal("local grants advertised over MCP")
		}
	}
	result, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_repo_cloud_controls", Arguments: map[string]any{}})
	if err == nil && (result == nil || !result.IsError) {
		t.Fatal("local grant RPC accepted over MCP")
	}
}
