package bossmcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestGetSettingsTool(t *testing.T) {
	backend := &fakeBackend{
		getSettings: func(_ context.Context) (*pb.GlobalSettings, error) {
			return &pb.GlobalSettings{
				WorktreeBaseDir:     "/tmp/wt",
				PollIntervalSeconds: 45,
				DefaultAgent:        "claude",
				Agents: []*pb.AgentSettings{
					{Name: "claude", Enabled: true, Config: map[string]string{"dangerously_skip_permissions": "true"}},
				},
			}, nil
		},
	}
	cs := newConnectedClient(t, backend, Options{})

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_settings", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool returned error result: %s", textOf(t, res))
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["worktree_base_dir"] != "/tmp/wt" || got["default_agent"] != "claude" {
		t.Fatalf("unexpected settings payload: %s", textOf(t, res))
	}
}

func TestGetSettingsTool_ErrorSurfaces(t *testing.T) {
	backend := &fakeBackend{
		getSettings: func(_ context.Context) (*pb.GlobalSettings, error) {
			return nil, errors.New("boom-get")
		},
	}
	cs := newConnectedClient(t, backend, Options{})

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_settings", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if !res.IsError || !strings.Contains(textOf(t, res), "boom-get") {
		t.Fatalf("expected error result carrying boom-get, got: %+v", res)
	}
}

func TestUpdateSettingsTool_ForwardsArgs(t *testing.T) {
	var captured *pb.UpdateSettingsRequest
	backend := &fakeBackend{
		updateSettings: func(_ context.Context, req *pb.UpdateSettingsRequest) (*pb.GlobalSettings, error) {
			captured = req
			return &pb.GlobalSettings{PollIntervalSeconds: req.GetPollIntervalSeconds()}, nil
		},
	}
	cs := newConnectedClient(t, backend, Options{})

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "update_settings",
		Arguments: map[string]any{
			"poll_interval_seconds": float64(90),
			"default_agent":         "codex",
			"agents": []any{
				map[string]any{
					"name":    "codex",
					"enabled": true,
					"config":  map[string]any{"sandbox": "workspace-write", "model": ""},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool returned error result: %s", textOf(t, res))
	}
	if captured == nil {
		t.Fatal("backend did not receive a request")
	}
	if captured.PollIntervalSeconds == nil || captured.GetPollIntervalSeconds() != 90 {
		t.Errorf("poll_interval_seconds not forwarded: %+v", captured.PollIntervalSeconds)
	}
	if captured.DefaultAgent == nil || captured.GetDefaultAgent() != "codex" {
		t.Errorf("default_agent not forwarded: %+v", captured.DefaultAgent)
	}
	// Unset scalar stays nil (present-only semantics).
	if captured.WorktreeBaseDir != nil {
		t.Errorf("worktree_base_dir should be nil when omitted, got %q", captured.GetWorktreeBaseDir())
	}
	if len(captured.GetAgents()) != 1 {
		t.Fatalf("agents = %d, want 1", len(captured.GetAgents()))
	}
	a := captured.GetAgents()[0]
	if a.GetName() != "codex" || a.Enabled == nil || !a.GetEnabled() {
		t.Errorf("agent update not forwarded: %+v", a)
	}
	if a.GetConfig()["sandbox"] != "workspace-write" || a.GetConfig()["model"] != "" {
		t.Errorf("agent config not forwarded: %+v", a.GetConfig())
	}
}

func TestUpdateSettingsTool_ErrorSurfaces(t *testing.T) {
	backend := &fakeBackend{
		updateSettings: func(_ context.Context, _ *pb.UpdateSettingsRequest) (*pb.GlobalSettings, error) {
			return nil, errors.New("boom-update")
		},
	}
	cs := newConnectedClient(t, backend, Options{})

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "update_settings",
		Arguments: map[string]any{"poll_interval_seconds": float64(1)},
	})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if !res.IsError || !strings.Contains(textOf(t, res), "boom-update") {
		t.Fatalf("expected error result carrying boom-update, got: %+v", res)
	}
}

// TestSettingsTools_ReadOnlyMode proves get_settings registers read-only but
// update_settings does not (it is a write tool).
func TestSettingsTools_ReadOnlyMode(t *testing.T) {
	ro := listedToolNames(t, Options{ReadOnly: true})
	if !ro["get_settings"] {
		t.Errorf("get_settings should be registered in read-only mode")
	}
	if ro["update_settings"] {
		t.Errorf("update_settings must NOT be registered in read-only mode")
	}
}
