package plugin

import (
	"testing"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestAgentRunner_StartRun_RoutesToCorrectMethod(t *testing.T) {
	t.Skip("integration-style — verified end-to-end in Task 25")
	// Placeholder for now; the full smoke test happens in the integration
	// test that spins up a real plugin process. The agentRunnerGRPCClient
	// method-routing is exercised in plugin_runner_test.go via the
	// AgentRunnerClient interface.
	_ = bossanovav1.StartAgentRunRequest{}
}
