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
	if got := reg.Current(); got != apiversion.V20260914 {
		t.Fatalf("DefaultRegistry().Current() = %q, want %q", got, apiversion.V20260914)
	}

	msg := &pb.ProxyListAccountsResponse{Accounts: []*pb.Account{{Id: "acct-1", AuthCheck: supersededCheck()}}}
	apiversion.ProductionChanges().Apply(bossanovav1connect.OrchestratorServiceProxyListAccountsProcedure, msg, apiversion.V20260912)
	check := msg.GetAccounts()[0].GetAuthCheck()
	if check.GetFailureClass() != "" {
		t.Errorf("failure_class at V20260912 = %q, want \"\"; a client built before V20260913 never saw a class on a healthy check", check.GetFailureClass())
	}
	if check.GetOutcome() != "healthy" {
		t.Errorf("outcome at V20260912 = %q, want \"healthy\"; the down-convert must not change eligibility", check.GetOutcome())
	}

	current := &pb.ProxyListAccountsResponse{Accounts: []*pb.Account{{Id: "acct-1", AuthCheck: supersededCheck()}}}
	apiversion.ProductionChanges().Apply(bossanovav1connect.OrchestratorServiceProxyListAccountsProcedure, current, apiversion.V20260913)
	if got := current.GetAccounts()[0].GetAuthCheck().GetFailureClass(); got != "credential_superseded" {
		t.Errorf("failure_class at Current = %q, want %q; the current version must serve the new class", got, "credential_superseded")
	}
}

// TestSupersededCredentialClassChange_CoversEveryAccountProcedure asserts the
// transform reaches every procedure that can carry an Account, not just the
// list read the web polls.
func TestSupersededCredentialClassChange_CoversEveryAccountProcedure(t *testing.T) {
	changes := apiversion.ProductionChanges()

	manage := &pb.ProxyManageListAccountsResponse{Accounts: []*pb.Account{{AuthCheck: supersededCheck()}}}
	changes.Apply(bossanovav1connect.OrchestratorServiceProxyManageListAccountsProcedure, manage, apiversion.Baseline)
	if got := manage.GetAccounts()[0].GetAuthCheck().GetFailureClass(); got != "" {
		t.Errorf("ProxyManageListAccounts failure_class = %q, want \"\"", got)
	}

	add := &pb.ProxyAddAccountResponse{Account: &pb.Account{AuthCheck: supersededCheck()}}
	changes.Apply(bossanovav1connect.OrchestratorServiceProxyAddAccountProcedure, add, apiversion.Baseline)
	if got := add.GetAccount().GetAuthCheck().GetFailureClass(); got != "" {
		t.Errorf("ProxyAddAccount failure_class = %q, want \"\"", got)
	}

	refresh := &pb.ProxyRefreshAccountResponse{Account: &pb.Account{AuthCheck: supersededCheck()}}
	changes.Apply(bossanovav1connect.OrchestratorServiceProxyRefreshAccountProcedure, refresh, apiversion.Baseline)
	if got := refresh.GetAccount().GetAuthCheck().GetFailureClass(); got != "" {
		t.Errorf("ProxyRefreshAccount failure_class = %q, want \"\"", got)
	}
}

// TestSupersededCredentialClassChange_LeavesEverythingElseAlone pins the
// no-op contract: an unrelated method, an unrelated payload type, a nil
// account, and — the substantive one — a class on a NON-healthy outcome, which
// this version did not introduce and older clients could always observe.
func TestSupersededCredentialClassChange_LeavesEverythingElseAlone(t *testing.T) {
	change := apiversion.SupersededCredentialClassChange{}

	unrelatedMethod := &pb.ProxyListAccountsResponse{Accounts: []*pb.Account{{AuthCheck: supersededCheck()}}}
	change.TransformResponse(bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure, unrelatedMethod)
	if got := unrelatedMethod.GetAccounts()[0].GetAuthCheck().GetFailureClass(); got != "credential_superseded" {
		t.Errorf("transform fired on an unrelated method: failure_class = %q", got)
	}

	// A mismatched payload type on a targeted method must not panic.
	change.TransformResponse(bossanovav1connect.OrchestratorServiceProxyListAccountsProcedure, &pb.ProxyListSessionsResponse{})
	change.TransformResponse(bossanovav1connect.OrchestratorServiceProxyAddAccountProcedure, nil)
	change.TransformResponse(bossanovav1connect.OrchestratorServiceProxyAddAccountProcedure, (*pb.ProxyAddAccountResponse)(nil))

	nilAccount := &pb.ProxyListAccountsResponse{Accounts: []*pb.Account{nil, {}}}
	change.TransformResponse(bossanovav1connect.OrchestratorServiceProxyListAccountsProcedure, nilAccount)

	unhealthy := &pb.ProxyListAccountsResponse{Accounts: []*pb.Account{{
		AuthCheck: &pb.AuthCheck{Outcome: "auth_invalid", FailureClass: "credential_superseded"},
	}}}
	change.TransformResponse(bossanovav1connect.OrchestratorServiceProxyListAccountsProcedure, unhealthy)
	if got := unhealthy.GetAccounts()[0].GetAuthCheck().GetFailureClass(); got != "credential_superseded" {
		t.Errorf("transform blanked a class on a non-healthy outcome: failure_class = %q, want it preserved", got)
	}

	otherClass := &pb.ProxyListAccountsResponse{Accounts: []*pb.Account{{
		AuthCheck: &pb.AuthCheck{Outcome: "healthy", FailureClass: "rate_limited"},
	}}}
	change.TransformResponse(bossanovav1connect.OrchestratorServiceProxyListAccountsProcedure, otherClass)
	if got := otherClass.GetAccounts()[0].GetAuthCheck().GetFailureClass(); got != "rate_limited" {
		t.Errorf("transform blanked an unrelated class: failure_class = %q", got)
	}
}
