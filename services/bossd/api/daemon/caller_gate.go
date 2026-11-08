package server

import (
	"context"
	"fmt"
	"net"
	"net/http"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/contract/daemon/callerorigin"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossd/internal/domain"
)

// CallerAuthority attests local callers without trusting client-supplied identity.
type CallerAuthority interface {
	ConnContext(context.Context, net.Conn) context.Context
	Principal(context.Context) domain.CallerPrincipal
}

// In-process upstream commands bypass this socket gate: bosso authorizes those
// commands before dispatching them through CommandHandlerAdapter.
var callerCapabilities = map[string]domain.CallerCapability{
	bossanovav1connect.DaemonServiceAcceptSessionPRBlockerProcedure: domain.CapabilityVerifyAccept,
	bossanovav1connect.DaemonServiceFollowUpSessionPRNotesProcedure: domain.CapabilityVerifyFollowUp,
	bossanovav1connect.DaemonServiceApproveSessionPRProcedure:       domain.CapabilityVerifyApprove,
	bossanovav1connect.DaemonServiceOperatorMergeSessionProcedure:   domain.CapabilityOperatorMerge,
}

// requiredCapability names the capability a request needs, if any. Creating a
// coordinator chat is gated by the role it asks for, so the chats a coordinator
// grant outranks cannot mint one; other RecordChat calls are ungated.
func requiredCapability(procedure string, msg any) (domain.CallerCapability, bool) {
	if capability, gated := callerCapabilities[procedure]; gated {
		return capability, true
	}
	if r, ok := msg.(*pb.RecordChatRequest); ok && procedure == bossanovav1connect.DaemonServiceRecordChatProcedure &&
		r.GetRole() == pb.ChatRole_CHAT_ROLE_COORDINATOR {
		return domain.CapabilityCreateCoordinatorChat, true
	}
	return "", false
}

// NewCallerInterceptor enforces operator capabilities on the local socket.
// grants extends them to chat roles; nil grants nothing.
func NewCallerInterceptor(authority CallerAuthority, grants domain.CallerGrants) *callerInterceptor {
	return &callerInterceptor{authority: authority, grants: grants}
}

type callerInterceptor struct {
	authority CallerAuthority
	grants    domain.CallerGrants
}

type callerPrincipalKey struct{}

// callerFrom returns the principal the caller gate attributed to a gated
// request, for the handler's audit trail.
func callerFrom(ctx context.Context) (domain.CallerPrincipal, bool) {
	p, ok := ctx.Value(callerPrincipalKey{}).(domain.CallerPrincipal)
	return p, ok
}

// principal attributes the connection from kernel facts, then caps it to the
// origin a forwarding client asserted. The assertion can only lower it.
func (i *callerInterceptor) principal(ctx context.Context, header http.Header) domain.CallerPrincipal {
	return attributeCaller(ctx, i.authority, header)
}

func attributeCaller(ctx context.Context, authority CallerAuthority, header http.Header) domain.CallerPrincipal {
	p := domain.CallerPrincipal{Kind: domain.CallerUnattested}
	if authority != nil {
		p = authority.Principal(ctx)
	}
	if assertion, present := callerorigin.Read(header); present {
		p = p.CapToOrigin(originOf(assertion))
	}
	return p
}

var callerKindProto = map[domain.CallerKind]pb.CallerKind{
	domain.CallerOperator:    pb.CallerKind_CALLER_KIND_OPERATOR,
	domain.CallerAgentChat:   pb.CallerKind_CALLER_KIND_AGENT_CHAT,
	domain.CallerDaemonChild: pb.CallerKind_CALLER_KIND_DAEMON_CHILD,
}

// DescribeCaller reports the principal this connection is attributed as. It is
// ungated: a caller learns only its own attribution. A boss CLI forwarding a
// call with `--host` asserts the answer to the remote daemon.
func (s *Server) DescribeCaller(ctx context.Context, req *connect.Request[pb.DescribeCallerRequest]) (*connect.Response[pb.DescribeCallerResponse], error) {
	p := attributeCaller(ctx, s.callers, req.Header())
	resp := &pb.DescribeCallerResponse{Kind: callerKindProto[p.Kind]}
	if p.Kind == domain.CallerAgentChat {
		resp.AgentSessionId, resp.SessionId, resp.Role = p.AgentSessionID, p.SessionID, AgentChatRoleToProto(p.Role)
	}
	return connect.NewResponse(resp), nil
}

func originOf(a callerorigin.Assertion) *domain.CallerOrigin {
	origin := &domain.CallerOrigin{Kind: domain.CallerUnattested, Host: a.Host}
	switch a.Kind {
	case callerorigin.KindOperator:
		origin.Kind = domain.CallerOperator
	case callerorigin.KindAgentChat:
		origin.Kind = domain.CallerAgentChat
		origin.AgentSessionID = a.AgentSessionID
		origin.SessionID = a.SessionID
		origin.Role = domain.ParseAgentChatRole(a.Role)
	case callerorigin.KindDaemonChild:
		origin.Kind = domain.CallerDaemonChild
	case callerorigin.KindUnattested:
	}
	return origin
}

func (i *callerInterceptor) check(ctx context.Context, procedure string, header http.Header, msg any) (context.Context, error) {
	capability, gated := requiredCapability(procedure, msg)
	if !gated {
		return ctx, nil
	}
	p := i.principal(ctx, header)
	if p.HasCapability(capability, i.grants) {
		return context.WithValue(ctx, callerPrincipalKey{}, p), nil
	}
	return ctx, refusal(p, capability)
}

func refusal(p domain.CallerPrincipal, capability domain.CallerCapability) error {
	who := "bossd could not attribute this call"
	switch p.Kind {
	case domain.CallerAgentChat:
		role := string(p.Role)
		if role == "" {
			role = "unknown"
		}
		who = fmt.Sprintf("bossd attributed this call to agent chat %s (role %s)", p.AgentSessionID, role)
	case domain.CallerDaemonChild:
		who = "bossd attributed this call to a daemon child"
	case domain.CallerOperator, domain.CallerUnattested:
		// Operators pass the capability check above; retain the fail-closed guidance.
	}
	if p.Forwarded {
		who += ", forwarded through `boss --host`"
		if p.ForwardedFrom != "" {
			who += " from " + p.ForwardedFrom
		}
	}
	action := "acting on a PR's verify outcome"
	remedy := "Run it from your own terminal"
	switch capability {
	case domain.CapabilityCreateCoordinatorChat:
		action = "creating a coordinator chat"
	case domain.CapabilityVerifyAccept, domain.CapabilityVerifyApprove, domain.CapabilityOperatorMerge:
		remedy += " or press [m] on the session in the TUI"
	case domain.CapabilityVerifyFollowUp:
	}
	return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("%s is the operator's call; %s. %s", action, who, remedy))
}

func (i *callerInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		ctx, err := i.check(ctx, req.Spec().Procedure, req.Header(), req.Any())
		if err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

func (i *callerInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		ctx, err := i.check(ctx, conn.Spec().Procedure, conn.RequestHeader(), nil)
		if err != nil {
			return err
		}
		return next(ctx, conn)
	}
}

func (i *callerInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}
