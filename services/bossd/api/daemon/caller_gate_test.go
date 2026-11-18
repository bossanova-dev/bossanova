package server

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/contract/daemon/socketauth"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/safego"
	"github.com/recurser/bossd/internal/domain"
)

type gateContextKey struct{}
type gateAuthority struct {
	principal domain.CallerPrincipal
	resolved  atomic.Int32
}

func (a *gateAuthority) ConnContext(ctx context.Context, _ net.Conn) context.Context {
	return context.WithValue(ctx, gateContextKey{}, a.principal)
}
func (a *gateAuthority) Principal(ctx context.Context) domain.CallerPrincipal {
	a.resolved.Add(1)
	p, _ := ctx.Value(gateContextKey{}).(domain.CallerPrincipal)
	return p
}

func TestCallerGateUnixSocket_Sessions_AC221001_AC221002(t *testing.T) {
	// No identity env is consulted by either the raw client or the authority.
	t.Setenv("BOSS_AGENT_SESSION_ID", "")
	chat := func(role domain.AgentChatRole) *domain.CallerPrincipal {
		return &domain.CallerPrincipal{Kind: domain.CallerAgentChat, AgentSessionID: "chat-123", SessionID: "sess-123", Role: role}
	}
	for _, tc := range []struct {
		name string
		p    *domain.CallerPrincipal // nil: bossd has no caller authority
		// approves: the caller may approve and operator-merge.
		// operator: it may also accept a blocker and follow up notes.
		approves, operator bool
	}{
		{"operator", &domain.CallerPrincipal{Kind: domain.CallerOperator}, true, true},
		{"build chat", chat(domain.AgentChatRoleBuild), true, false},
		{"unknown-role chat", chat(domain.AgentChatRoleUnknown), true, false},
		{"daemon child", &domain.CallerPrincipal{Kind: domain.CallerDaemonChild}, false, false},
		{"unattested", &domain.CallerPrincipal{Kind: domain.CallerUnattested}, false, false},
		{"missing authority", nil, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newOperatorMergeHarness(t, headPin)
			a := &gateAuthority{}
			h.srv.callers = a
			if tc.p == nil {
				h.srv.callers = nil
			} else {
				a.principal = *tc.p
			}
			path := shortSocketPath(t)
			if err := h.srv.Listen(path); err != nil {
				t.Fatal(err)
			}
			done := safego.Go(h.srv.logger, func() { _ = h.srv.Serve() })
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
				defer cancel()
				_ = h.srv.Shutdown(ctx)
				<-done
			}()
			token, err := socketauth.ReadToken(path)
			if err != nil {
				t.Fatal(err)
			}
			client := clientFor(t, path, connect.WithInterceptors(socketauth.NewClientInterceptor(token)))
			if _, err := client.ListPlugins(context.Background(), connect.NewRequest(&pb.ListPluginsRequest{})); err != nil {
				t.Fatalf("ungated: %v", err)
			}
			if a.resolved.Load() != 0 {
				t.Fatal("ungated call resolved a principal")
			}
			_, err = clientFor(t, path).ApproveSessionPR(context.Background(), connect.NewRequest(&pb.ApproveSessionPRRequest{Id: "s1"}))
			if connect.CodeOf(err) != connect.CodeUnauthenticated || a.resolved.Load() != 0 {
				t.Fatalf("token must run first: %v", err)
			}

			_, err = client.AcceptSessionPRBlocker(context.Background(), connect.NewRequest(&pb.AcceptSessionPRBlockerRequest{Id: "s1", ExpectedHeadSha: headPin, BlockerId: "review:123456abcdef", Reason: "operator decision"}))
			assertCallerGateResult(t, tc.operator, err)
			_, err = client.FollowUpSessionPRNotes(context.Background(), connect.NewRequest(&pb.FollowUpSessionPRNotesRequest{Id: "s1", ShouldFollowUpAll: true}))
			assertCallerGateResult(t, tc.operator, err)
			if !tc.operator && h.verbs.builtCount != 0 {
				t.Fatal("a refused accept or follow-up reached the verify toolbox")
			}

			_, err = client.ApproveSessionPR(context.Background(), connect.NewRequest(&pb.ApproveSessionPRRequest{Id: "s1"}))
			assertCallerGateResult(t, tc.approves, err)
			_, err = client.OperatorMergeSession(context.Background(), connect.NewRequest(&pb.OperatorMergeSessionRequest{Id: "s1", ExpectedHeadSha: headPin, ShouldApprove: true}))
			assertCallerGateResult(t, tc.approves, err)
			if tc.approves && len(h.prov.mergeOpts) != 1 {
				t.Fatal("an approving caller did not merge")
			}
			if !tc.approves && (len(h.calls) != 0 || h.verbs.builtCount != 0 || len(h.prov.mergeOpts) != 0) {
				t.Fatalf("refused caller caused writes: %v", h.calls)
			}
		})
	}
}

func assertCallerGateResult(t *testing.T, allowed bool, err error) {
	t.Helper()
	if allowed {
		if err != nil {
			t.Fatalf("allowed call refused: %v", err)
		}
		return
	}
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("want PermissionDenied: %v", err)
	}
}
