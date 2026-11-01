package server

import (
	"context"
	"fmt"
	"net"

	"connectrpc.com/connect"
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
	bossanovav1connect.DaemonServiceApproveSessionPRProcedure:     domain.CapabilityVerifyApprove,
	bossanovav1connect.DaemonServiceOperatorMergeSessionProcedure: domain.CapabilityOperatorMerge,
}

// NewCallerInterceptor enforces operator capabilities on the local socket.
// An empty grants table is the default; role grants are reserved for coordinators.
func NewCallerInterceptor(authority CallerAuthority, grants domain.CallerGrants) *callerInterceptor {
	return &callerInterceptor{authority: authority, grants: grants}
}

type callerInterceptor struct {
	authority CallerAuthority
	grants    domain.CallerGrants
}

func (i *callerInterceptor) check(ctx context.Context, procedure string) error {
	capability, gated := callerCapabilities[procedure]
	if !gated {
		return nil
	}
	p := domain.CallerPrincipal{Kind: domain.CallerUnattested}
	if i.authority != nil {
		p = i.authority.Principal(ctx)
	}
	if p.HasCapability(capability, i.grants) {
		return nil
	}
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
	return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("approving or operator-merging a PR is the operator's call; %s. Run it from your own terminal or press [m] on the session in the TUI", who))
}

func (i *callerInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if err := i.check(ctx, req.Spec().Procedure); err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}
func (i *callerInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		if err := i.check(ctx, conn.Spec().Procedure); err != nil {
			return err
		}
		return next(ctx, conn)
	}
}
func (i *callerInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}
