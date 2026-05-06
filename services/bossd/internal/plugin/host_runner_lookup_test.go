package plugin

import (
	"context"
	"testing"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/config"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/plugin/eventbus"
)

// stubAgentRunner is a minimal AgentRunner used purely as an identity marker
// in the AgentRunnerByName / AgentRunners tests. The methods are not
// exercised — the tests only assert that the host returns the right pointer
// for each plugin name.
type stubAgentRunner struct{ name string }

func (s *stubAgentRunner) GetInfo(context.Context) (*bossanovav1.PluginInfo, error) {
	return &bossanovav1.PluginInfo{Name: s.name}, nil
}
func (s *stubAgentRunner) StartRun(context.Context, *bossanovav1.StartAgentRunRequest) (*bossanovav1.StartAgentRunResponse, error) {
	return &bossanovav1.StartAgentRunResponse{}, nil
}
func (s *stubAgentRunner) StopRun(context.Context, *bossanovav1.StopAgentRunRequest) (*bossanovav1.StopAgentRunResponse, error) {
	return &bossanovav1.StopAgentRunResponse{}, nil
}
func (s *stubAgentRunner) IsRunning(context.Context, *bossanovav1.IsAgentRunningRequest) (*bossanovav1.IsAgentRunningResponse, error) {
	return &bossanovav1.IsAgentRunningResponse{}, nil
}
func (s *stubAgentRunner) ExitStatus(context.Context, *bossanovav1.AgentExitStatusRequest) (*bossanovav1.AgentExitStatusResponse, error) {
	return &bossanovav1.AgentExitStatusResponse{}, nil
}
func (s *stubAgentRunner) ConfigureFinalizeHook(context.Context, *bossanovav1.ConfigureFinalizeHookRequest) (*bossanovav1.ConfigureFinalizeHookResponse, error) {
	return &bossanovav1.ConfigureFinalizeHookResponse{}, nil
}
func (s *stubAgentRunner) BuildInteractiveCommand(context.Context, *bossanovav1.BuildInteractiveCommandRequest) (*bossanovav1.BuildInteractiveCommandResponse, error) {
	return &bossanovav1.BuildInteractiveCommandResponse{}, nil
}
func (s *stubAgentRunner) ListIgnoredDirtyFiles(context.Context, *bossanovav1.ListIgnoredDirtyFilesRequest) (*bossanovav1.ListIgnoredDirtyFilesResponse, error) {
	return &bossanovav1.ListIgnoredDirtyFilesResponse{}, nil
}
func (s *stubAgentRunner) GetChatTitle(context.Context, *bossanovav1.GetChatTitleRequest) (*bossanovav1.GetChatTitleResponse, error) {
	return &bossanovav1.GetChatTitleResponse{}, nil
}

// newHostForTest builds a Host pre-populated with the given managedPlugins,
// bypassing the gRPC subprocess launch path. The callers only need the
// cfg.Name and agentRunner fields populated for runner-lookup tests.
func newHostForTest(plugins []managedPlugin) *Host {
	bus := eventbus.New(zerolog.Nop())
	h := New(bus, nil, zerolog.Nop())
	h.plugins = plugins
	return h
}

func TestAgentRunnerByName(t *testing.T) {
	claudeRunner := &stubAgentRunner{name: "claude"}
	opencodeRunner := &stubAgentRunner{name: "opencode"}
	h := newHostForTest([]managedPlugin{
		{cfg: config.PluginConfig{Name: "claude"}, agentRunner: claudeRunner},
		{cfg: config.PluginConfig{Name: "opencode"}, agentRunner: opencodeRunner},
	})

	if got := h.AgentRunnerByName("claude"); got != claudeRunner {
		t.Errorf("AgentRunnerByName(claude) = %v, want claudeRunner", got)
	}
	if got := h.AgentRunnerByName("opencode"); got != opencodeRunner {
		t.Errorf("AgentRunnerByName(opencode) = %v, want opencodeRunner", got)
	}
	if got := h.AgentRunnerByName("missing"); got != nil {
		t.Errorf("AgentRunnerByName(missing) = %v, want nil", got)
	}
}

func TestAgentRunnerByNameSkipsPluginsWithoutAgentRunner(t *testing.T) {
	claudeRunner := &stubAgentRunner{name: "claude"}
	h := newHostForTest([]managedPlugin{
		// A plugin that did not dispense an AgentRunner — must be ignored
		// even when its name matches.
		{cfg: config.PluginConfig{Name: "linear"}, agentRunner: nil},
		{cfg: config.PluginConfig{Name: "claude"}, agentRunner: claudeRunner},
	})

	if got := h.AgentRunnerByName("linear"); got != nil {
		t.Errorf("AgentRunnerByName(linear) = %v, want nil (no agentRunner dispensed)", got)
	}
	if got := h.AgentRunnerByName("claude"); got != claudeRunner {
		t.Errorf("AgentRunnerByName(claude) = %v, want claudeRunner", got)
	}
}

func TestAgentRunnersReturnsAllRunners(t *testing.T) {
	claudeRunner := &stubAgentRunner{name: "claude"}
	opencodeRunner := &stubAgentRunner{name: "opencode"}
	h := newHostForTest([]managedPlugin{
		{cfg: config.PluginConfig{Name: "claude"}, agentRunner: claudeRunner},
		{cfg: config.PluginConfig{Name: "linear"}, agentRunner: nil},
		{cfg: config.PluginConfig{Name: "opencode"}, agentRunner: opencodeRunner},
	})

	runners := h.AgentRunners()
	if len(runners) != 2 {
		t.Fatalf("AgentRunners returned %d entries, want 2 (got %v)", len(runners), runners)
	}
	if runners["claude"] != claudeRunner {
		t.Errorf("AgentRunners[claude] = %v, want claudeRunner", runners["claude"])
	}
	if runners["opencode"] != opencodeRunner {
		t.Errorf("AgentRunners[opencode] = %v, want opencodeRunner", runners["opencode"])
	}
	if _, ok := runners["linear"]; ok {
		t.Errorf("AgentRunners should omit plugins with nil agentRunner; found 'linear'")
	}
}

func TestAgentRunnersReturnsCopy(t *testing.T) {
	claudeRunner := &stubAgentRunner{name: "claude"}
	h := newHostForTest([]managedPlugin{
		{cfg: config.PluginConfig{Name: "claude"}, agentRunner: claudeRunner},
	})

	first := h.AgentRunners()
	// Mutate the returned map: delete the entry and add a junk one.
	delete(first, "claude")
	first["injected"] = &stubAgentRunner{name: "injected"}

	second := h.AgentRunners()
	if len(second) != 1 {
		t.Fatalf("mutation of returned map leaked into host; second call returned %d entries", len(second))
	}
	if second["claude"] != claudeRunner {
		t.Errorf("expected host to still hold claudeRunner; got %v", second["claude"])
	}
	if _, ok := second["injected"]; ok {
		t.Error("injected entry leaked into host's runner map")
	}
}

func TestAgentRunnersEmptyMap(t *testing.T) {
	h := newHostForTest(nil)
	runners := h.AgentRunners()
	if runners == nil {
		t.Fatal("AgentRunners must return a non-nil empty map, got nil")
	}
	if len(runners) != 0 {
		t.Errorf("expected empty map, got %d entries", len(runners))
	}
}
