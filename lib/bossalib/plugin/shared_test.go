package plugin

import "testing"

func TestProtocolVersionProtectsRepoTrustWireContract(t *testing.T) {
	if ProtocolVersion != 3 {
		t.Errorf("ProtocolVersion = %d, want 3", ProtocolVersion)
	}
	if ProtocolVersion <= ProtocolVersionV2 || ProtocolVersionV2 <= ProtocolVersionV1 {
		t.Errorf("protocol versions out of order: v1=%d v2=%d current=%d", ProtocolVersionV1, ProtocolVersionV2, ProtocolVersion)
	}
}

func TestPluginTypeAgentRunner(t *testing.T) {
	if PluginTypeAgentRunner != "agent_runner" {
		t.Errorf("PluginTypeAgentRunner = %q, want %q", PluginTypeAgentRunner, "agent_runner")
	}
}

func TestBrokerIDs(t *testing.T) {
	if BrokerIDTaskSourceHostService != 1 {
		t.Errorf("BrokerIDTaskSourceHostService = %d, want 1", BrokerIDTaskSourceHostService)
	}
	if BrokerIDWorkflowHostService != 1 {
		t.Errorf("BrokerIDWorkflowHostService = %d, want 1", BrokerIDWorkflowHostService)
	}
	if BrokerIDAgentRunnerHostService != 2 {
		t.Errorf("BrokerIDAgentRunnerHostService = %d, want 2", BrokerIDAgentRunnerHostService)
	}
}
