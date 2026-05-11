package server

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/agent"
	"github.com/rs/zerolog"
)

// listAgentsFakeClient is a minimal AgentRunnerClient for ListAgents tests.
// Only GetInfo is exercised; the rest are no-op stubs. info controls the
// response and infoErr lets a test simulate a plugin whose GetInfo fails so
// the daemon's defensive fallback (Name-only AgentInfo) can be asserted.
type listAgentsFakeClient struct {
	info    *bossanovav1.PluginInfo
	infoErr error
}

func (c *listAgentsFakeClient) GetInfo(context.Context) (*bossanovav1.PluginInfo, error) {
	if c.infoErr != nil {
		return nil, c.infoErr
	}
	return c.info, nil
}
func (c *listAgentsFakeClient) StartRun(context.Context, *bossanovav1.StartAgentRunRequest) (*bossanovav1.StartAgentRunResponse, error) {
	return &bossanovav1.StartAgentRunResponse{}, nil
}
func (c *listAgentsFakeClient) StopRun(context.Context, *bossanovav1.StopAgentRunRequest) (*bossanovav1.StopAgentRunResponse, error) {
	return &bossanovav1.StopAgentRunResponse{}, nil
}
func (c *listAgentsFakeClient) IsRunning(context.Context, *bossanovav1.IsAgentRunningRequest) (*bossanovav1.IsAgentRunningResponse, error) {
	return &bossanovav1.IsAgentRunningResponse{}, nil
}
func (c *listAgentsFakeClient) ExitStatus(context.Context, *bossanovav1.AgentExitStatusRequest) (*bossanovav1.AgentExitStatusResponse, error) {
	return &bossanovav1.AgentExitStatusResponse{}, nil
}
func (c *listAgentsFakeClient) ConfigureFinalizeHook(context.Context, *bossanovav1.ConfigureFinalizeHookRequest) (*bossanovav1.ConfigureFinalizeHookResponse, error) {
	return &bossanovav1.ConfigureFinalizeHookResponse{}, nil
}
func (c *listAgentsFakeClient) BuildInteractiveCommand(context.Context, *bossanovav1.BuildInteractiveCommandRequest) (*bossanovav1.BuildInteractiveCommandResponse, error) {
	return &bossanovav1.BuildInteractiveCommandResponse{}, nil
}
func (c *listAgentsFakeClient) ResolveInteractiveSessionID(context.Context, *bossanovav1.ResolveInteractiveSessionIDRequest) (*bossanovav1.ResolveInteractiveSessionIDResponse, error) {
	return &bossanovav1.ResolveInteractiveSessionIDResponse{}, nil
}
func (c *listAgentsFakeClient) ListIgnoredDirtyFiles(context.Context, *bossanovav1.ListIgnoredDirtyFilesRequest) (*bossanovav1.ListIgnoredDirtyFilesResponse, error) {
	return &bossanovav1.ListIgnoredDirtyFilesResponse{}, nil
}
func (c *listAgentsFakeClient) GetChatTitle(context.Context, *bossanovav1.GetChatTitleRequest) (*bossanovav1.GetChatTitleResponse, error) {
	return &bossanovav1.GetChatTitleResponse{}, nil
}
func (c *listAgentsFakeClient) HasQuestionPrompt(context.Context, *bossanovav1.HasQuestionPromptRequest) (*bossanovav1.HasQuestionPromptResponse, error) {
	return &bossanovav1.HasQuestionPromptResponse{}, nil
}
func (c *listAgentsFakeClient) LastTurnIsUser(context.Context, *bossanovav1.LastTurnIsUserRequest) (*bossanovav1.LastTurnIsUserResponse, error) {
	return &bossanovav1.LastTurnIsUserResponse{}, nil
}
func (c *listAgentsFakeClient) TranscriptExists(context.Context, *bossanovav1.TranscriptExistsRequest) (*bossanovav1.TranscriptExistsResponse, error) {
	return &bossanovav1.TranscriptExistsResponse{}, nil
}

func newListAgentsServer(clients map[string]agent.AgentRunnerClient) *Server {
	return &Server{
		agentClients: clients,
		logger:       zerolog.Nop(),
	}
}

// TestListAgents_ReturnsRegisteredAgents verifies that a server with two
// registered AgentRunnerClients surfaces both, with full GetInfo metadata
// (name + version + user_settings) and in lexicographic order so callers
// see deterministic output.
func TestListAgents_ReturnsRegisteredAgents(t *testing.T) {
	t.Parallel()
	clients := map[string]agent.AgentRunnerClient{
		"claude": &listAgentsFakeClient{
			info: &bossanovav1.PluginInfo{Name: "claude", Version: "1.2.3", UserSettings: []*bossanovav1.UserSetting{{Key: "claude.model"}}},
		},
		"codex": &listAgentsFakeClient{
			info: &bossanovav1.PluginInfo{Name: "codex", Version: "0.4.1", UserSettings: []*bossanovav1.UserSetting{{Key: "codex.sandbox"}}},
		},
	}
	srv := newListAgentsServer(clients)
	resp, err := srv.ListAgents(context.Background(), connect.NewRequest(&bossanovav1.ListAgentsRequest{}))
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if got := len(resp.Msg.GetAgents()); got != 2 {
		t.Fatalf("expected 2 agents, got %d (%+v)", got, resp.Msg.GetAgents())
	}
	got := resp.Msg.GetAgents()
	if got[0].GetName() != "claude" {
		t.Errorf("agents[0].name = %q, want claude (lex order)", got[0].GetName())
	}
	if got[0].GetVersion() != "1.2.3" {
		t.Errorf("agents[0].version = %q, want 1.2.3", got[0].GetVersion())
	}
	if len(got[0].GetUserSettings()) != 1 || got[0].GetUserSettings()[0].GetKey() != "claude.model" {
		t.Errorf("claude UserSettings not propagated: %+v", got[0].GetUserSettings())
	}
	if got[1].GetName() != "codex" {
		t.Errorf("agents[1].name = %q, want codex", got[1].GetName())
	}
	if len(got[1].GetUserSettings()) != 1 || got[1].GetUserSettings()[0].GetKey() != "codex.sandbox" {
		t.Errorf("codex UserSettings not propagated: %+v", got[1].GetUserSettings())
	}
}

// TestListAgents_GetInfoErrorIncludesBareName proves a plugin whose GetInfo
// errors is still surfaced (with Name only) rather than silently dropped —
// so the operator can see something broke and pick the plugin anyway.
func TestListAgents_GetInfoErrorIncludesBareName(t *testing.T) {
	t.Parallel()
	clients := map[string]agent.AgentRunnerClient{
		"broken": &listAgentsFakeClient{infoErr: errors.New("boom")},
	}
	srv := newListAgentsServer(clients)
	resp, err := srv.ListAgents(context.Background(), connect.NewRequest(&bossanovav1.ListAgentsRequest{}))
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	got := resp.Msg.GetAgents()
	if len(got) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(got))
	}
	if got[0].GetName() != "broken" {
		t.Errorf("agents[0].name = %q, want broken", got[0].GetName())
	}
	if got[0].GetVersion() != "" {
		t.Errorf("expected empty Version on GetInfo error, got %q", got[0].GetVersion())
	}
}

// TestListAgents_EmptyRegistryReturnsEmpty proves a server with no agent
// clients (e.g. NoopRunner mode) returns a non-nil empty list, matching the
// proto contract callers can rely on.
func TestListAgents_EmptyRegistryReturnsEmpty(t *testing.T) {
	t.Parallel()
	srv := newListAgentsServer(nil)
	resp, err := srv.ListAgents(context.Background(), connect.NewRequest(&bossanovav1.ListAgentsRequest{}))
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if got := len(resp.Msg.GetAgents()); got != 0 {
		t.Errorf("expected 0 agents in empty registry, got %d", got)
	}
}
