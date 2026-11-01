package domain

import "testing"

func TestClassifyCaller(t *testing.T) {
	for _, role := range []AgentChatRole{AgentChatRoleBuild, AgentChatRoleOperator} {
		chat := CallerPrincipal{Kind: CallerAgentChat, AgentSessionID: "chat", SessionID: "session", Role: role}
		panes := map[int]CallerPrincipal{20: chat, 30: chat}
		for _, pid := range []int{20, 30} {
			got := ClassifyCaller([]int{10, pid, 40}, panes, 40)
			if got.Kind != CallerAgentChat || got.Role != role || got.AgentSessionID != "chat" {
				t.Fatalf("chat: %+v", got)
			}
		}
	}
	if got := ClassifyCaller([]int{10, 40}, nil, 40); got.Kind != CallerDaemonChild {
		t.Fatalf("daemon: %+v", got)
	}
	if got := ClassifyCaller([]int{10, 2, 1}, nil, 40); got.Kind != CallerOperator {
		t.Fatalf("operator: %+v", got)
	}
}

func TestCallerCapabilities(t *testing.T) {
	for _, kind := range []CallerKind{CallerOperator, CallerAgentChat, CallerDaemonChild, CallerUnattested} {
		p := CallerPrincipal{Kind: kind, Role: AgentChatRoleOperator}
		if got := p.HasCapability(CapabilityVerifyApprove, nil); got != (kind == CallerOperator) {
			t.Fatalf("%s: %v", kind, got)
		}
	}
	grants := CallerGrants{{Kind: CallerAgentChat, Role: AgentChatRoleVerify}: {CapabilityVerifyApprove}}
	p := CallerPrincipal{Kind: CallerAgentChat, Role: AgentChatRoleVerify}
	if !p.HasCapability(CapabilityVerifyApprove, grants) || p.HasCapability(CapabilityOperatorMerge, grants) {
		t.Fatal("grant does not scope capability")
	}
}
