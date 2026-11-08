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

func TestAcceptRequiresItsOwnOperatorCapability(t *testing.T) {
	for _, kind := range []CallerKind{CallerOperator, CallerAgentChat, CallerDaemonChild, CallerUnattested} {
		p := CallerPrincipal{Kind: kind, Role: AgentChatRoleVerify}
		if p.HasCapability(CapabilityVerifyAccept, nil) != (kind == CallerOperator) {
			t.Fatalf("accept capability for %s", kind)
		}
	}
	p := CallerPrincipal{Kind: CallerAgentChat, Role: AgentChatRoleVerify}
	grants := CallerGrants{{Kind: p.Kind, Role: p.Role}: {CapabilityVerifyApprove}}
	if p.HasCapability(CapabilityVerifyAccept, grants) {
		t.Fatal("approval grant also allowed acceptance")
	}
}

func TestCoordinatorApprovalGrantIsRoleScoped(t *testing.T) {
	grants := CoordinatorApprovalGrants()
	for _, role := range []AgentChatRole{AgentChatRoleCoordinator, AgentChatRoleOperator, AgentChatRoleBuild, AgentChatRoleRepair, AgentChatRoleVerify, AgentChatRoleUnknown} {
		p := CallerPrincipal{Kind: CallerAgentChat, Role: role}
		want := role == AgentChatRoleCoordinator
		for _, c := range []CallerCapability{CapabilityVerifyApprove, CapabilityOperatorMerge} {
			if p.HasCapability(c, grants) != want {
				t.Fatalf("%s %s: want %v", role, c, want)
			}
		}
		for _, c := range []CallerCapability{CapabilityVerifyAccept, CapabilityVerifyFollowUp, CapabilityCreateCoordinatorChat} {
			if p.HasCapability(c, grants) {
				t.Fatalf("%s gained %s", role, c)
			}
		}
	}
	for _, kind := range []CallerKind{CallerDaemonChild, CallerUnattested} {
		if (CallerPrincipal{Kind: kind, Role: AgentChatRoleCoordinator}).HasCapability(CapabilityVerifyApprove, grants) {
			t.Fatalf("%s gained the coordinator grant", kind)
		}
	}
	if !(CallerPrincipal{Kind: CallerOperator}).HasCapability(CapabilityCreateCoordinatorChat, nil) {
		t.Fatal("operator cannot create a coordinator chat")
	}
}

func TestCapToOriginOnlyLowers(t *testing.T) {
	operator := CallerPrincipal{Kind: CallerOperator, PID: 7}
	grants := CoordinatorApprovalGrants()
	cases := []struct {
		origin  CallerOrigin
		kind    CallerKind
		approve bool
	}{
		{CallerOrigin{Kind: CallerOperator, Host: "a"}, CallerOperator, true},
		{CallerOrigin{Kind: CallerAgentChat, AgentSessionID: "chat-1", SessionID: "s-1", Role: AgentChatRoleBuild}, CallerAgentChat, false},
		{CallerOrigin{Kind: CallerAgentChat, AgentSessionID: "chat-2", Role: AgentChatRoleCoordinator}, CallerAgentChat, true},
		{CallerOrigin{Kind: CallerDaemonChild}, CallerDaemonChild, false},
		{CallerOrigin{Kind: CallerUnattested}, CallerUnattested, false},
		{CallerOrigin{Kind: "root"}, CallerUnattested, false},
	}
	for _, tc := range cases {
		got := operator.CapToOrigin(&tc.origin)
		if got.Kind != tc.kind || !got.Forwarded || got.HasCapability(CapabilityVerifyApprove, grants) != tc.approve {
			t.Fatalf("%+v: %+v", tc.origin, got)
		}
		if tc.kind == CallerAgentChat && (got.AgentSessionID != tc.origin.AgentSessionID || got.Role != tc.origin.Role) {
			t.Fatalf("lost the asserted chat: %+v", got)
		}
	}
	// A principal already below the operator cannot be raised by any assertion.
	build := CallerPrincipal{Kind: CallerAgentChat, AgentSessionID: "local", Role: AgentChatRoleBuild}
	for _, origin := range []CallerOrigin{{Kind: CallerOperator}, {Kind: CallerAgentChat, Role: AgentChatRoleCoordinator}} {
		if got := build.CapToOrigin(&origin); got != build {
			t.Fatalf("assertion changed a local chat: %+v", got)
		}
	}
	if got := operator.CapToOrigin(nil); got != operator {
		t.Fatalf("no assertion changed the operator: %+v", got)
	}
}
