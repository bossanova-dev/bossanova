package apiversion_test

import (
	"testing"

	"github.com/recurser/bossalib/apiversion"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

// supersededCheck is the current (V20260913+) shape BOS-1175 introduced: a
// healthy probe that nevertheless reports the stored refresh chain is dead.
func supersededCheck() *pb.AuthCheck {
	return &pb.AuthCheck{Outcome: "healthy", FailureClass: "credential_superseded"}
}

// TestSupersededCredentialClassChange_DownconvertsOneVersionBack pins the
// boundary itself: at the PREVIOUS Current the new pairing is invisible, and at
// the new Current it is served intact. Asserting against the registry rather
// than a literal keeps the test meaningful after the next bump.
func TestSupersededCredentialClassChange_DownconvertsOneVersionBack(t *testing.T) {
	reg := apiversion.DefaultRegistry()
	if got := reg.Current(); got != apiversion.V20261031 {
		t.Fatalf("DefaultRegistry().Current() = %q, want %q", got, apiversion.V20261031)
	}

	msg := &pb.ProxyListAgentAccountsResponse{Accounts: []*pb.AgentAccount{{Id: "acct-1", AuthCheck: supersededCheck()}}}
	apiversion.ProductionChanges().Apply(bossanovav1connect.OrchestratorServiceProxyListAgentAccountsProcedure, msg, apiversion.V20260912)
	check := msg.GetAccounts()[0].GetAuthCheck()
	if check.GetFailureClass() != "" {
		t.Errorf("failure_class at V20260912 = %q, want \"\"; a client built before V20260913 never saw a class on a healthy check", check.GetFailureClass())
	}
	if check.GetOutcome() != "healthy" {
		t.Errorf("outcome at V20260912 = %q, want \"healthy\"; the down-convert must not change eligibility", check.GetOutcome())
	}

	current := &pb.ProxyListAgentAccountsResponse{Accounts: []*pb.AgentAccount{{Id: "acct-1", AuthCheck: supersededCheck()}}}
	apiversion.ProductionChanges().Apply(bossanovav1connect.OrchestratorServiceProxyListAgentAccountsProcedure, current, apiversion.V20260913)
	if got := current.GetAccounts()[0].GetAuthCheck().GetFailureClass(); got != "credential_superseded" {
		t.Errorf("failure_class at Current = %q, want %q; the current version must serve the new class", got, "credential_superseded")
	}
}

// TestSupersededCredentialClassChange_CoversEveryAccountProcedure asserts the
// transform reaches every procedure that can carry an Account, not just the
// list read the web polls.
func TestSupersededCredentialClassChange_CoversEveryAccountProcedure(t *testing.T) {
	changes := apiversion.ProductionChanges()

	manage := &pb.ProxyManageListAgentAccountsResponse{Accounts: []*pb.AgentAccount{{AuthCheck: supersededCheck()}}}
	changes.Apply(bossanovav1connect.OrchestratorServiceProxyManageListAgentAccountsProcedure, manage, apiversion.Baseline)
	if got := manage.GetAccounts()[0].GetAuthCheck().GetFailureClass(); got != "" {
		t.Errorf("ProxyManageListAgentAccounts failure_class = %q, want \"\"", got)
	}

	add := &pb.ProxyAddAgentAccountResponse{Account: &pb.AgentAccount{AuthCheck: supersededCheck()}}
	changes.Apply(bossanovav1connect.OrchestratorServiceProxyAddAgentAccountProcedure, add, apiversion.Baseline)
	if got := add.GetAccount().GetAuthCheck().GetFailureClass(); got != "" {
		t.Errorf("ProxyAddAgentAccount failure_class = %q, want \"\"", got)
	}

	refresh := &pb.ProxyRefreshAgentAccountResponse{Account: &pb.AgentAccount{AuthCheck: supersededCheck()}}
	changes.Apply(bossanovav1connect.OrchestratorServiceProxyRefreshAgentAccountProcedure, refresh, apiversion.Baseline)
	if got := refresh.GetAccount().GetAuthCheck().GetFailureClass(); got != "" {
		t.Errorf("ProxyRefreshAgentAccount failure_class = %q, want \"\"", got)
	}
}

// TestSupersededCredentialClassChange_LeavesEverythingElseAlone pins the
// no-op contract: an unrelated method, an unrelated payload type, a nil
// account, and — the substantive one — a class on a NON-healthy outcome, which
// this version did not introduce and older clients could always observe.
func TestSupersededCredentialClassChange_LeavesEverythingElseAlone(t *testing.T) {
	change := apiversion.SupersededCredentialClassChange{}

	unrelatedMethod := &pb.ProxyListAgentAccountsResponse{Accounts: []*pb.AgentAccount{{AuthCheck: supersededCheck()}}}
	change.TransformResponse(bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure, unrelatedMethod)
	if got := unrelatedMethod.GetAccounts()[0].GetAuthCheck().GetFailureClass(); got != "credential_superseded" {
		t.Errorf("transform fired on an unrelated method: failure_class = %q", got)
	}

	// A mismatched payload type on a targeted method must not panic.
	change.TransformResponse(bossanovav1connect.OrchestratorServiceProxyListAgentAccountsProcedure, &pb.ProxyListSessionsResponse{})
	change.TransformResponse(bossanovav1connect.OrchestratorServiceProxyAddAgentAccountProcedure, nil)
	change.TransformResponse(bossanovav1connect.OrchestratorServiceProxyAddAgentAccountProcedure, (*pb.ProxyAddAgentAccountResponse)(nil))

	nilAccount := &pb.ProxyListAgentAccountsResponse{Accounts: []*pb.AgentAccount{nil, {}}}
	change.TransformResponse(bossanovav1connect.OrchestratorServiceProxyListAgentAccountsProcedure, nilAccount)

	unhealthy := &pb.ProxyListAgentAccountsResponse{Accounts: []*pb.AgentAccount{{
		AuthCheck: &pb.AuthCheck{Outcome: "auth_invalid", FailureClass: "credential_superseded"},
	}}}
	change.TransformResponse(bossanovav1connect.OrchestratorServiceProxyListAgentAccountsProcedure, unhealthy)
	if got := unhealthy.GetAccounts()[0].GetAuthCheck().GetFailureClass(); got != "credential_superseded" {
		t.Errorf("transform blanked a class on a non-healthy outcome: failure_class = %q, want it preserved", got)
	}

	otherClass := &pb.ProxyListAgentAccountsResponse{Accounts: []*pb.AgentAccount{{
		AuthCheck: &pb.AuthCheck{Outcome: "healthy", FailureClass: "rate_limited"},
	}}}
	change.TransformResponse(bossanovav1connect.OrchestratorServiceProxyListAgentAccountsProcedure, otherClass)
	if got := otherClass.GetAccounts()[0].GetAuthCheck().GetFailureClass(); got != "rate_limited" {
		t.Errorf("transform blanked an unrelated class: failure_class = %q", got)
	}
}
