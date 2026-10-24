package plugin_test

import (
	"testing"

	goplugin "github.com/hashicorp/go-plugin"
	sharedplugin "github.com/recurser/bossalib/contract/plugin/plugin"
	"github.com/recurser/bossalib/contract/plugin/plugin/agentrunnertest"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/plugin/pluginharness"
)

func TestStubRunnerConformance(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-process conformance requires a plugin binary")
	}
	binary := pluginharness.BuildPlugin(t, "bossd-plugin-stub-runner")
	process := pluginharness.SpawnPlugin(t, binary, goplugin.PluginSet{sharedplugin.PluginTypeAgentRunner: &sharedplugin.AgentRunnerPlugin{}})
	client, err := process.Client()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := client.Dispense(sharedplugin.PluginTypeAgentRunner)
	if err != nil {
		t.Fatal(err)
	}
	runner, ok := raw.(bossanovav1.AgentRunnerServiceClient)
	if !ok {
		t.Fatalf("dispensed client %T is not AgentRunnerServiceClient", raw)
	}
	agentrunnertest.Run(t, agentrunnertest.Runner{Name: "stub-runner subprocess", Client: runner, Unsupported: []string{
		bossanovav1.AgentRunnerService_PreflightHeadlessRun_FullMethodName,
		bossanovav1.AgentRunnerService_DescribeMCPSurface_FullMethodName,
		bossanovav1.AgentRunnerService_MaterializeAgentAccount_FullMethodName,
		bossanovav1.AgentRunnerService_ReadTranscript_FullMethodName,
		bossanovav1.AgentRunnerService_RotationCapability_FullMethodName,
	}})
}
