package server

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/contract/daemon/socketauth"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
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
	for _, kind := range []domain.CallerKind{domain.CallerAgentChat, domain.CallerDaemonChild, domain.CallerUnattested, domain.CallerOperator, "missing authority"} {
		t.Run(string(kind), func(t *testing.T) {
			h := newOperatorMergeHarness(t, headPin)
			a := &gateAuthority{principal: domain.CallerPrincipal{Kind: kind, AgentSessionID: "chat-123", Role: domain.AgentChatRoleOperator}}
			h.srv.callers = a
			if kind == "missing authority" {
				h.srv.callers = nil
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
			assertCallerGateResult(t, kind, err)
			_, err = client.ApproveSessionPR(context.Background(), connect.NewRequest(&pb.ApproveSessionPRRequest{Id: "s1"}))
			assertCallerGateResult(t, kind, err)
			_, err = client.FollowUpSessionPRNotes(context.Background(), connect.NewRequest(&pb.FollowUpSessionPRNotesRequest{Id: "s1", ShouldFollowUpAll: true}))
			if kind == domain.CallerOperator {
				if err != nil {
					t.Fatalf("operator follow-up: %v", err)
				}
			} else if connect.CodeOf(err) != connect.CodePermissionDenied {
				t.Fatalf("%s follow-up: %v", kind, err)
			}
			for _, approve := range []bool{true, false} {
				_, err = client.OperatorMergeSession(context.Background(), connect.NewRequest(&pb.OperatorMergeSessionRequest{Id: "s1", ExpectedHeadSha: headPin, ShouldApprove: approve}))
				assertCallerGateResult(t, kind, err)
				if kind == domain.CallerOperator {
					break
				} // first call merged the fixture PR
			}
			if kind != domain.CallerOperator && (len(h.calls) != 0 || h.verbs.builtCount != 0 || len(h.prov.mergeOpts) != 0) {
				t.Fatalf("rejected caller caused writes: %v", h.calls)
			}
			if kind == domain.CallerOperator && len(h.prov.mergeOpts) != 1 {
				t.Fatal("operator did not merge")
			}
		})
	}
}

func assertCallerGateResult(t *testing.T, kind domain.CallerKind, err error) {
	t.Helper()
	if kind == domain.CallerOperator {
		if err != nil {
			t.Fatalf("operator: %v", err)
		}
		return
	}
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("%s: %v", kind, err)
	}
	if kind == domain.CallerAgentChat {
		for _, s := range []string{"chat-123", "role operator", "your own terminal", "[m]"} {
			if !strings.Contains(err.Error(), s) {
				t.Fatalf("missing %q: %v", s, err)
			}
		}
	}
}

func TestCallerGateRoleGrant(t *testing.T) {
	p := domain.CallerPrincipal{Kind: domain.CallerAgentChat, Role: domain.AgentChatRoleVerify}
	a := &gateAuthority{principal: p}
	i := &callerInterceptor{authority: a, grants: domain.CallerGrants{{Kind: p.Kind, Role: p.Role}: {domain.CapabilityVerifyApprove}}}
	ctx := a.ConnContext(context.Background(), nil)
	if err := i.check(ctx, bossanovav1connect.DaemonServiceApproveSessionPRProcedure); err != nil {
		t.Fatal(err)
	}
	if err := i.check(ctx, bossanovav1connect.DaemonServiceOperatorMergeSessionProcedure); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("grant exceeded scope: %v", err)
	}
}
