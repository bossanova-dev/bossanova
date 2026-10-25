package bossmcp

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func listedToolNames(t *testing.T, opts Options) map[string]bool {
	t.Helper()
	cs := newConnectedClient(t, &fakeBackend{}, opts)
	res, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	names := make(map[string]bool, len(res.Tools))
	for _, tool := range res.Tools {
		names[tool.Name] = true
	}
	return names
}

func TestReadOnlyOmitsWriteTools(t *testing.T) {
	names := listedToolNames(t, Options{ReadOnly: true})

	for _, want := range readOnlyToolNames {
		if !names[want] {
			t.Errorf("read-only mode is missing read tool %q", want)
		}
	}
	for _, bad := range writeToolNames {
		if names[bad] {
			t.Errorf("read-only mode must not register write tool %q", bad)
		}
	}
	if len(names) != len(readOnlyToolNames) {
		t.Errorf("read-only tools/list = %d tools, want %d: %v", len(names), len(readOnlyToolNames), names)
	}
}

func TestFullToolSetRegistered(t *testing.T) {
	names := listedToolNames(t, Options{})
	want := append(append([]string{}, readOnlyToolNames...), writeToolNames...)
	for _, w := range want {
		if !names[w] {
			t.Errorf("full mode is missing tool %q", w)
		}
	}
	if len(names) != len(want) {
		t.Errorf("full tools/list = %d tools, want %d: %v", len(names), len(want), names)
	}
}
