package plugin

import "testing"

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
