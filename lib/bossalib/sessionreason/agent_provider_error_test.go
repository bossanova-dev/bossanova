package sessionreason

import "testing"

func TestAgentProviderError(t *testing.T) {
	reason := AgentProviderError()
	if reason != "agent-provider-error" {
		t.Fatalf("AgentProviderError() = %q, want the shared agent-provider-error slug", reason)
	}
	if !IsAgentProviderError(&reason) {
		t.Fatal("IsAgentProviderError did not match its own constructor")
	}
	other := "draft PR creation failed: boom"
	for name, r := range map[string]*string{"nil": nil, "other": &other} {
		if IsAgentProviderError(r) {
			t.Fatalf("IsAgentProviderError matched %s reason", name)
		}
	}
}
