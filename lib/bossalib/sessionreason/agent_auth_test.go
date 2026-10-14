package sessionreason

import "testing"

func TestAgentAuthFailed(t *testing.T) {
	reason := AgentAuthFailed()
	if reason != "agent-auth-failed" {
		t.Fatalf("AgentAuthFailed() = %q, want the shared agent-auth-failed slug", reason)
	}
	if !IsAgentAuthFailed(&reason) {
		t.Fatal("IsAgentAuthFailed did not match its own constructor")
	}
	other := "draft PR creation failed: boom"
	for name, r := range map[string]*string{"nil": nil, "other": &other} {
		if IsAgentAuthFailed(r) {
			t.Fatalf("IsAgentAuthFailed matched %s reason", name)
		}
	}
}
