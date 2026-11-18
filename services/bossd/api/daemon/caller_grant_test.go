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

// serveGated serves h's daemon on a real socket with the given attribution,
// and returns a client that sends origin (when non-nil) on every call.
func serveGated(t *testing.T, h *operatorMergeHarness, p domain.CallerPrincipal, origin *callerorigin.Assertion) bossanovav1connect.DaemonServiceClient {
	t.Helper()
	h.srv.callers = &gateAuthority{principal: p}
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

func TestAgentChatApprovesAndMergesAndNamesTheChat(t *testing.T) {
	for _, role := range []domain.AgentChatRole{domain.AgentChatRoleBuild, domain.AgentChatRoleUnknown, domain.AgentChatRoleCoordinator} {
		t.Run("role "+string(role), func(t *testing.T) {
			t.Setenv("BOSS_APPROVER_KIND", "operator") // an inherited value must not leak into the record
			h := newOperatorMergeHarness(t, headPin)
			p := domain.CallerPrincipal{Kind: domain.CallerAgentChat, AgentSessionID: "chat-a", SessionID: "sess-a", Role: role}
			client := serveGated(t, h, p, nil)

			if err := approveS1(client, false); err != nil {
				t.Fatalf("agent chat approve: %v", err)
			}
			want := map[string]string{
				"BOSS_APPROVER_KIND": "agent-chat", "BOSS_APPROVER_ROLE": string(role),
				"BOSS_APPROVER_CHAT": "chat-a", "BOSS_APPROVER_SESSION": "sess-a",
			}
			if got := approverEnv(h.verbs.env); !maps.Equal(got, want) {
				t.Fatalf("approval record approver = %v, want %v", got, want)
			}
			if err := mergeS1(client); err != nil {
				t.Fatalf("agent chat merge: %v", err)
			}
			if len(h.prov.mergeOpts) != 1 {
				t.Fatal("agent chat did not merge")
			}
			if got := approverEnv(h.verbs.env); !maps.Equal(got, want) {
				t.Fatalf("merge approval approver = %v, want %v", got, want)
			}
		})
	}
}

func TestAgentChatCannotAcceptOrFollowUp(t *testing.T) {
	h := newOperatorMergeHarness(t, headPin)
	client := serveGated(t, h, domain.CallerPrincipal{Kind: domain.CallerAgentChat, AgentSessionID: "chat-a", Role: domain.AgentChatRoleCoordinator}, nil)
	_, err := client.AcceptSessionPRBlocker(context.Background(), connect.NewRequest(&pb.AcceptSessionPRBlockerRequest{Id: "s1", ExpectedHeadSha: headPin, BlockerId: "review:123456abcdef", Reason: "x"}))
	if connect.CodeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), "chat-a") {
		t.Fatalf("agent chat accept: %v", err)
	}
	_, err = client.FollowUpSessionPRNotes(context.Background(), connect.NewRequest(&pb.FollowUpSessionPRNotesRequest{Id: "s1", ShouldFollowUpAll: true}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("agent chat follow-up: %v", err)
	}
	if h.verbs.builtCount != 0 {
		t.Fatal("a refused agent chat reached the verify toolbox")
	}
}

// sshdOperator is how a forwarded connection attributes: sshd's ancestry
// joins no chat pane and does not contain the daemon.
var sshdOperator = domain.CallerPrincipal{Kind: domain.CallerOperator, PID: 4242}

func TestForwardedAgentChatApprovesAsAnAgentChat(t *testing.T) {
	for _, role := range []string{"build", "coordinator", ""} {
		t.Run("role "+role, func(t *testing.T) {
			h := newOperatorMergeHarness(t, headPin)
			origin := &callerorigin.Assertion{Kind: callerorigin.KindAgentChat, AgentSessionID: "chat-remote", SessionID: "sess-remote", Role: role, Host: "laptop"}
			client := serveGated(t, h, sshdOperator, origin)
			if err := approveS1(client, false); err != nil {
				t.Fatalf("forwarded agent chat approve: %v", err)
			}
			want := map[string]string{
				"BOSS_APPROVER_KIND": "agent-chat", "BOSS_APPROVER_ROLE": role, "BOSS_APPROVER_CHAT": "chat-remote",
				"BOSS_APPROVER_SESSION": "sess-remote", "BOSS_APPROVER_FORWARDED_FROM": "laptop",
			}
			if got := approverEnv(h.verbs.env); !maps.Equal(got, want) {
				t.Fatalf("approver = %v, want %v", got, want)
			}
			if err := mergeS1(client); err != nil || len(h.prov.mergeOpts) != 1 {
				t.Fatalf("forwarded agent chat merge: %v", err)
			}
			// It holds no more than a local agent chat.
			_, err := client.FollowUpSessionPRNotes(context.Background(), connect.NewRequest(&pb.FollowUpSessionPRNotesRequest{Id: "s1", ShouldFollowUpAll: true}))
			if connect.CodeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), "laptop") {
				t.Fatalf("forwarded agent chat follow-up: %v", err)
			}
		})
	}
}

func TestForwardedDaemonChildIsCapped(t *testing.T) {
	h := newOperatorMergeHarness(t, headPin)
	client := serveGated(t, h, sshdOperator, &callerorigin.Assertion{Kind: callerorigin.KindDaemonChild, Host: "laptop"})
	if err := approveS1(client, true); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("forwarded daemon child dry-run approve: %v", err)
	}
	if err := mergeS1(client); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("forwarded daemon child merge: %v", err)
	}
	if h.verbs.builtCount != 0 || len(h.prov.mergeOpts) != 0 {
		t.Fatal("a capped forwarded call caused writes")
	}
}

func TestForwardedMalformedOriginFailsClosed(t *testing.T) {
	h := newOperatorMergeHarness(t, headPin)
	client := serveGated(t, h, sshdOperator, &callerorigin.Assertion{Kind: "root"})
	if err := approveS1(client, true); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("malformed origin: %v", err)
	}
}

func TestForwardedOperatorStillWorks(t *testing.T) {
	h := newOperatorMergeHarness(t, headPin)
	client := serveGated(t, h, sshdOperator, &callerorigin.Assertion{Kind: callerorigin.KindOperator, Host: "laptop"})
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

func TestOriginCannotRaiseALocalCaller(t *testing.T) {
	h := newOperatorMergeHarness(t, headPin)
	build := domain.CallerPrincipal{Kind: domain.CallerAgentChat, AgentSessionID: "chat-local", Role: domain.AgentChatRoleBuild}
	for _, tc := range []struct {
		local  domain.CallerPrincipal
		origin callerorigin.Assertion
	}{
		{domain.CallerPrincipal{Kind: domain.CallerDaemonChild}, callerorigin.Assertion{Kind: callerorigin.KindOperator}},
		{domain.CallerPrincipal{Kind: domain.CallerDaemonChild}, callerorigin.Assertion{Kind: callerorigin.KindAgentChat, AgentSessionID: "chat-local"}},
		{domain.CallerPrincipal{Kind: domain.CallerUnattested}, callerorigin.Assertion{Kind: callerorigin.KindOperator}},
		{build, callerorigin.Assertion{Kind: callerorigin.KindOperator}},
	} {
		h.srv.callers = &gateAuthority{principal: tc.local}
		i := NewCallerInterceptor(h.srv.callers)
		header := http.Header{}
		tc.origin.Set(header)
		ctx := h.srv.callers.ConnContext(context.Background(), nil)
		_, err := i.check(ctx, bossanovav1connect.DaemonServiceAcceptSessionPRBlockerProcedure, header)
		if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("%+v raised local %s to accept: %v", tc.origin, tc.local.Kind, err)
		}
	}
}

func TestAnyCallerCreatesACoordinatorChat(t *testing.T) {
	for _, p := range []domain.CallerPrincipal{
		{Kind: domain.CallerOperator},
		{Kind: domain.CallerAgentChat, AgentSessionID: "chat-build", SessionID: "s1", Role: domain.AgentChatRoleBuild},
		{Kind: domain.CallerAgentChat, AgentSessionID: "chat-coord", SessionID: "s1", Role: domain.AgentChatRoleCoordinator},
	} {
		t.Run(string(p.Kind)+" "+string(p.Role), func(t *testing.T) {
			h := newOperatorMergeHarness(t, headPin)
			h.srv.agentChats = &recordChatStoreFake{}
			h.srv.initDeferrers()
			client := serveGated(t, h, p, nil)
			resp, err := client.RecordChat(context.Background(), connect.NewRequest(&pb.RecordChatRequest{
				SessionId: "s1", AgentSessionId: "0199a3c1-7b2e-7d40-9c55-1a2b3c4d5e71", Role: pb.ChatRole_CHAT_ROLE_COORDINATOR,
			}))
			if err != nil {
				t.Fatalf("create coordinator chat: %v", err)
			}
			if got := resp.Msg.GetChat().GetRole(); got != pb.ChatRole_CHAT_ROLE_COORDINATOR {
				t.Fatalf("recorded role = %v, want coordinator", got)
			}
		})
	}
}

func TestDescribeCallerReportsTheAttribution(t *testing.T) {
	h := newOperatorMergeHarness(t, headPin)
	client := serveGated(t, h, coordinatorChat, nil)
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
