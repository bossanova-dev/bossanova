package server

import (
	"context"
	"maps"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/contract/daemon/callerorigin"
	"github.com/recurser/bossalib/contract/daemon/socketauth"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossalib/kit/safego"
	"github.com/recurser/bossd/internal/domain"
)

// serveGated serves h's daemon on a real socket with the given attribution and
// grants, and returns a client that sends origin (when non-nil) on every call.
func serveGated(t *testing.T, h *operatorMergeHarness, p domain.CallerPrincipal, grants domain.CallerGrants, origin *callerorigin.Assertion) bossanovav1connect.DaemonServiceClient {
	t.Helper()
	h.srv.callers = &gateAuthority{principal: p}
	h.srv.callerGrants = grants
	path := shortSocketPath(t)
	if err := h.srv.Listen(path); err != nil {
		t.Fatal(err)
	}
	done := safego.Go(h.srv.logger, func() { _ = h.srv.Serve() })
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = h.srv.Shutdown(ctx)
		<-done
	})
	token, err := socketauth.ReadToken(path)
	if err != nil {
		t.Fatal(err)
	}
	interceptors := []connect.Interceptor{socketauth.NewClientInterceptor(token)}
	if origin != nil {
		interceptors = append(interceptors, callerorigin.NewClientInterceptor(*origin))
	}
	return clientFor(t, path, connect.WithInterceptors(interceptors...))
}

var coordinatorChat = domain.CallerPrincipal{Kind: domain.CallerAgentChat, AgentSessionID: "chat-coord", SessionID: "sess-coord", Role: domain.AgentChatRoleCoordinator}

func approveS1(client bossanovav1connect.DaemonServiceClient, dryRun bool) error {
	_, err := client.ApproveSessionPR(context.Background(), connect.NewRequest(&pb.ApproveSessionPRRequest{Id: "s1", IsDryRun: dryRun}))
	return err
}

func mergeS1(client bossanovav1connect.DaemonServiceClient) error {
	_, err := client.OperatorMergeSession(context.Background(), connect.NewRequest(&pb.OperatorMergeSessionRequest{Id: "s1", ExpectedHeadSha: headPin, ShouldApprove: true}))
	return err
}

// approverEnv is the BOSS_APPROVER_* part of the environment the verify
// toolbox ran with: what its approval record names.
func approverEnv(env []string) map[string]string {
	out := map[string]string{}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, approverEnvPrefix) {
			if _, dup := out[k]; dup {
				out[k] = "DUPLICATE"
				continue
			}
			out[k] = v
		}
	}
	return out
}

func TestCoordinatorGrant_ApprovesAndMergesAndNamesTheChat(t *testing.T) {
	t.Setenv("BOSS_APPROVER_KIND", "operator") // an inherited value must not leak into the record
	h := newOperatorMergeHarness(t, headPin)
	client := serveGated(t, h, coordinatorChat, domain.CoordinatorApprovalGrants(), nil)

	if err := approveS1(client, false); err != nil {
		t.Fatalf("coordinator approve: %v", err)
	}
	want := map[string]string{
		"BOSS_APPROVER_KIND": "agent-chat", "BOSS_APPROVER_ROLE": "coordinator",
		"BOSS_APPROVER_CHAT": "chat-coord", "BOSS_APPROVER_SESSION": "sess-coord",
	}
	if got := approverEnv(h.verbs.env); !maps.Equal(got, want) {
		t.Fatalf("approval record approver = %v, want %v", got, want)
	}
	if err := mergeS1(client); err != nil {
		t.Fatalf("coordinator merge: %v", err)
	}
	if len(h.prov.mergeOpts) != 1 {
		t.Fatal("coordinator did not merge")
	}
	if got := approverEnv(h.verbs.env); !maps.Equal(got, want) {
		t.Fatalf("merge approval approver = %v, want %v", got, want)
	}
	// The grant covers approval and merge only.
	_, err := client.AcceptSessionPRBlocker(context.Background(), connect.NewRequest(&pb.AcceptSessionPRBlockerRequest{Id: "s1", ExpectedHeadSha: headPin, BlockerId: "review:123456abcdef", Reason: "x"}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("coordinator accept: %v", err)
	}
	_, err = client.FollowUpSessionPRNotes(context.Background(), connect.NewRequest(&pb.FollowUpSessionPRNotesRequest{Id: "s1", ShouldFollowUpAll: true}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("coordinator follow-up: %v", err)
	}
}

func TestCoordinatorGrant_OptOutRefuses(t *testing.T) {
	h := newOperatorMergeHarness(t, headPin)
	client := serveGated(t, h, coordinatorChat, nil, nil)
	for name, call := range map[string]func() error{
		"approve": func() error { return approveS1(client, false) },
		"merge":   func() error { return mergeS1(client) },
	} {
		if err := call(); connect.CodeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), "role coordinator") {
			t.Fatalf("%s with the grant withheld: %v", name, err)
		}
	}
	if h.verbs.builtCount != 0 || len(h.prov.mergeOpts) != 0 {
		t.Fatal("a refused coordinator caused writes")
	}
}

func TestCoordinatorGrant_OtherRolesRefused(t *testing.T) {
	for _, role := range []domain.AgentChatRole{domain.AgentChatRoleOperator, domain.AgentChatRoleBuild, domain.AgentChatRoleRepair, domain.AgentChatRoleVerify, domain.AgentChatRoleUnknown} {
		t.Run(string(role), func(t *testing.T) {
			h := newOperatorMergeHarness(t, headPin)
			p := domain.CallerPrincipal{Kind: domain.CallerAgentChat, AgentSessionID: "chat-x", Role: role}
			client := serveGated(t, h, p, domain.CoordinatorApprovalGrants(), nil)
			if err := approveS1(client, true); connect.CodeOf(err) != connect.CodePermissionDenied {
				t.Fatalf("approve: %v", err)
			}
			if err := mergeS1(client); connect.CodeOf(err) != connect.CodePermissionDenied {
				t.Fatalf("merge: %v", err)
			}
			if h.verbs.builtCount != 0 || len(h.prov.mergeOpts) != 0 {
				t.Fatal("a refused role caused writes")
			}
		})
	}
}

// sshdOperator is how a forwarded connection attributes: sshd's ancestry
// joins no chat pane and does not contain the daemon.
var sshdOperator = domain.CallerPrincipal{Kind: domain.CallerOperator, PID: 4242}

func TestForwardedAgentChatIsCapped(t *testing.T) {
	for _, role := range []string{"build", "operator", ""} {
		t.Run("role "+role, func(t *testing.T) {
			h := newOperatorMergeHarness(t, headPin)
			origin := &callerorigin.Assertion{Kind: callerorigin.KindAgentChat, AgentSessionID: "chat-remote", SessionID: "sess-remote", Role: role, Host: "laptop"}
			client := serveGated(t, h, sshdOperator, domain.CoordinatorApprovalGrants(), origin)
			// The reported gap: even a read-only dry run was not refused.
			err := approveS1(client, true)
			if connect.CodeOf(err) != connect.CodePermissionDenied {
				t.Fatalf("forwarded dry-run approve: %v", err)
			}
			for _, s := range []string{"chat-remote", "forwarded", "laptop"} {
				if !strings.Contains(err.Error(), s) {
					t.Fatalf("refusal missing %q: %v", s, err)
				}
			}
			if err := mergeS1(client); connect.CodeOf(err) != connect.CodePermissionDenied {
				t.Fatalf("forwarded merge: %v", err)
			}
			if h.verbs.builtCount != 0 || len(h.prov.mergeOpts) != 0 {
				t.Fatal("a capped forwarded call caused writes")
			}
		})
	}
}

func TestForwardedMalformedOriginFailsClosed(t *testing.T) {
	h := newOperatorMergeHarness(t, headPin)
	client := serveGated(t, h, sshdOperator, domain.CoordinatorApprovalGrants(), &callerorigin.Assertion{Kind: "root"})
	if err := approveS1(client, true); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("malformed origin: %v", err)
	}
}

func TestForwardedOperatorStillWorks(t *testing.T) {
	h := newOperatorMergeHarness(t, headPin)
	client := serveGated(t, h, sshdOperator, nil, &callerorigin.Assertion{Kind: callerorigin.KindOperator, Host: "laptop"})
	if err := approveS1(client, false); err != nil {
		t.Fatalf("forwarded operator approve: %v", err)
	}
	if got, want := approverEnv(h.verbs.env), map[string]string{"BOSS_APPROVER_KIND": "operator", "BOSS_APPROVER_FORWARDED_FROM": "laptop"}; !maps.Equal(got, want) {
		t.Fatalf("approver = %v, want %v", got, want)
	}
	if err := mergeS1(client); err != nil || len(h.prov.mergeOpts) != 1 {
		t.Fatalf("forwarded operator merge: %v", err)
	}
}

func TestForwardedCoordinatorKeepsTheGrant(t *testing.T) {
	h := newOperatorMergeHarness(t, headPin)
	origin := &callerorigin.Assertion{Kind: callerorigin.KindAgentChat, AgentSessionID: "chat-coord", SessionID: "sess-coord", Role: "coordinator", Host: "laptop"}
	client := serveGated(t, h, sshdOperator, domain.CoordinatorApprovalGrants(), origin)
	if err := approveS1(client, false); err != nil {
		t.Fatalf("forwarded coordinator approve: %v", err)
	}
	want := map[string]string{
		"BOSS_APPROVER_KIND": "agent-chat", "BOSS_APPROVER_ROLE": "coordinator", "BOSS_APPROVER_CHAT": "chat-coord",
		"BOSS_APPROVER_SESSION": "sess-coord", "BOSS_APPROVER_FORWARDED_FROM": "laptop",
	}
	if got := approverEnv(h.verbs.env); !maps.Equal(got, want) {
		t.Fatalf("approver = %v, want %v", got, want)
	}
	if err := mergeS1(client); err != nil || len(h.prov.mergeOpts) != 1 {
		t.Fatalf("forwarded coordinator merge: %v", err)
	}
}

func TestOriginCannotRaiseALocalChat(t *testing.T) {
	h := newOperatorMergeHarness(t, headPin)
	build := domain.CallerPrincipal{Kind: domain.CallerAgentChat, AgentSessionID: "chat-local", Role: domain.AgentChatRoleBuild}
	for _, origin := range []callerorigin.Assertion{
		{Kind: callerorigin.KindOperator},
		{Kind: callerorigin.KindAgentChat, AgentSessionID: "chat-local", Role: "coordinator"},
	} {
		h.srv.callers = &gateAuthority{principal: build}
		i := NewCallerInterceptor(h.srv.callers, domain.CoordinatorApprovalGrants())
		header := http.Header{}
		origin.Set(header)
		ctx := h.srv.callers.ConnContext(context.Background(), nil)
		if _, err := i.check(ctx, bossanovav1connect.DaemonServiceApproveSessionPRProcedure, header, nil); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("%+v raised a local build chat: %v", origin, err)
		}
	}
}

func TestCreatingACoordinatorChatIsOperatorOnly(t *testing.T) {
	procedure := bossanovav1connect.DaemonServiceRecordChatProcedure
	coordinator := &pb.RecordChatRequest{SessionId: "s1", AgentSessionId: "a", Role: pb.ChatRole_CHAT_ROLE_COORDINATOR}
	build := &pb.RecordChatRequest{SessionId: "s1", AgentSessionId: "a", Role: pb.ChatRole_CHAT_ROLE_BUILD}
	cases := []struct {
		p       domain.CallerPrincipal
		allowed bool
	}{
		{domain.CallerPrincipal{Kind: domain.CallerOperator}, true},
		{coordinatorChat, false},
		{domain.CallerPrincipal{Kind: domain.CallerAgentChat, Role: domain.AgentChatRoleBuild}, false},
		{domain.CallerPrincipal{Kind: domain.CallerDaemonChild}, false},
		{domain.CallerPrincipal{Kind: domain.CallerUnattested}, false},
	}
	for _, tc := range cases {
		a := &gateAuthority{principal: tc.p}
		i := NewCallerInterceptor(a, domain.CoordinatorApprovalGrants())
		ctx := a.ConnContext(context.Background(), nil)
		_, err := i.check(ctx, procedure, http.Header{}, coordinator)
		if tc.allowed != (err == nil) {
			t.Fatalf("%s create coordinator: %v", tc.p.Kind, err)
		}
		if err != nil && !strings.Contains(err.Error(), "creating a coordinator chat") {
			t.Fatalf("refusal wording: %v", err)
		}
		if _, err := i.check(ctx, procedure, http.Header{}, build); err != nil {
			t.Fatalf("%s create build chat was gated: %v", tc.p.Kind, err)
		}
	}
	// A forwarded agent chat cannot create one either.
	a := &gateAuthority{principal: sshdOperator}
	i := NewCallerInterceptor(a, domain.CoordinatorApprovalGrants())
	header := http.Header{}
	callerorigin.Assertion{Kind: callerorigin.KindAgentChat, AgentSessionID: "remote", Role: "coordinator"}.Set(header)
	if _, err := i.check(a.ConnContext(context.Background(), nil), procedure, header, coordinator); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("forwarded chat created a coordinator: %v", err)
	}
}

func TestDescribeCallerReportsTheAttribution(t *testing.T) {
	h := newOperatorMergeHarness(t, headPin)
	client := serveGated(t, h, coordinatorChat, nil, nil)
	resp, err := client.DescribeCaller(context.Background(), connect.NewRequest(&pb.DescribeCallerRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	got := resp.Msg
	if got.GetKind() != pb.CallerKind_CALLER_KIND_AGENT_CHAT || got.GetAgentSessionId() != "chat-coord" ||
		got.GetSessionId() != "sess-coord" || got.GetRole() != pb.ChatRole_CHAT_ROLE_COORDINATOR {
		t.Fatalf("DescribeCaller = %+v", got)
	}
}
