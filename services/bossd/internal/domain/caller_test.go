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
	approveAndMerge := []CallerCapability{CapabilityVerifyApprove, CapabilityOperatorMerge}
	operatorOnly := []CallerCapability{CapabilityVerifyAccept, CapabilityVerifyFollowUp}
	roles := []AgentChatRole{AgentChatRoleBuild, AgentChatRoleRepair, AgentChatRoleVerify, AgentChatRoleOperator, AgentChatRoleCoordinator, AgentChatRoleUnknown}
	for _, role := range roles {
		chat := CallerPrincipal{Kind: CallerAgentChat, Role: role}
		for _, c := range approveAndMerge {
			if !chat.HasCapability(c) {
				t.Fatalf("agent chat with role %q lacks %s", role, c)
			}
		}
		for _, c := range operatorOnly {
			if chat.HasCapability(c) {
				t.Fatalf("agent chat with role %q holds %s", role, c)
			}
		}
	}
	for _, c := range append(approveAndMerge, operatorOnly...) {
		if !(CallerPrincipal{Kind: CallerOperator}).HasCapability(c) {
			t.Fatalf("operator lacks %s", c)
		}
		for _, kind := range []CallerKind{CallerDaemonChild, CallerUnattested, "missing"} {
			if (CallerPrincipal{Kind: kind, Role: AgentChatRoleCoordinator}).HasCapability(c) {
				t.Fatalf("%s holds %s", kind, c)
			}
		}
	}
}

func TestCapToOriginOnlyLowers(t *testing.T) {
	operator := CallerPrincipal{Kind: CallerOperator, PID: 7}
	cases := []struct {
		origin  CallerOrigin
		kind    CallerKind
		approve bool
	}{
		{CallerOrigin{Kind: CallerOperator, Host: "a"}, CallerOperator, true},
		{CallerOrigin{Kind: CallerAgentChat, AgentSessionID: "chat-1", SessionID: "s-1", Role: AgentChatRoleBuild}, CallerAgentChat, true},
		{CallerOrigin{Kind: CallerAgentChat, AgentSessionID: "chat-2", Role: AgentChatRoleCoordinator}, CallerAgentChat, true},
		{CallerOrigin{Kind: CallerDaemonChild}, CallerDaemonChild, false},
		{CallerOrigin{Kind: CallerUnattested}, CallerUnattested, false},
		{CallerOrigin{Kind: "root"}, CallerUnattested, false},
	}
	for _, tc := range cases {
		got := operator.CapToOrigin(&tc.origin)
		if got.Kind != tc.kind || !got.Forwarded || got.HasCapability(CapabilityVerifyApprove) != tc.approve {
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
