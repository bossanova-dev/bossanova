package domain

import "slices"

// CallerKind is the process provenance attested by the local daemon.
type CallerKind string

const (
	CallerOperator    CallerKind = "operator"
	CallerAgentChat   CallerKind = "agent chat"
	CallerDaemonChild CallerKind = "daemon child"
	CallerUnattested  CallerKind = "unattested"
)

// CallerPrincipal is derived from kernel process facts, never client headers.
// A forwarding client's origin assertion can only lower it; see CapToOrigin.
type CallerPrincipal struct {
	Kind           CallerKind
	AgentSessionID string
	SessionID      string
	Role           AgentChatRole
	PID            int
	// Forwarded marks a call whose operator attribution was capped to the
	// origin its client asserted (`boss --host` over an SSH socket forward).
	// ForwardedFrom is the asserted origin host, kept for the audit trail only.
	Forwarded     bool
	ForwardedFrom string
}

// CallerCapability identifies an operator action at the daemon boundary.
type CallerCapability string

const (
	CapabilityVerifyAccept   CallerCapability = "verify.accept"
	CapabilityVerifyApprove  CallerCapability = "verify.approve"
	CapabilityVerifyFollowUp CallerCapability = "verify.follow-up"
	CapabilityOperatorMerge  CallerCapability = "verify.operator-merge"
	// CapabilityCreateCoordinatorChat creates a chat with the coordinator role.
	// It stays operator-only, so a granted role cannot be minted by the chats
	// it would outrank.
	CapabilityCreateCoordinatorChat CallerCapability = "chat.create-coordinator"
)

var operatorCapabilities = []CallerCapability{
	CapabilityVerifyAccept, CapabilityVerifyApprove, CapabilityVerifyFollowUp,
	CapabilityOperatorMerge, CapabilityCreateCoordinatorChat,
}

// CallerGrantKey scopes a grant to a principal kind and role.
type CallerGrantKey struct {
	Kind CallerKind
	Role AgentChatRole
}

// CallerGrants extends the operator's default capabilities. An empty table grants nothing.
type CallerGrants map[CallerGrantKey][]CallerCapability

// CoordinatorApprovalGrants is the shipped coordinator approval grant: an agent
// chat with the coordinator role may approve a verify park and run an operator
// merge. It grants nothing to any other role or kind.
func CoordinatorApprovalGrants() CallerGrants {
	return CallerGrants{
		{Kind: CallerAgentChat, Role: AgentChatRoleCoordinator}: {CapabilityVerifyApprove, CapabilityOperatorMerge},
	}
}

// HasCapability applies the default operator grant and an optional role grant.
func (p CallerPrincipal) HasCapability(capability CallerCapability, grants CallerGrants) bool {
	if p.Kind == CallerOperator {
		return slices.Contains(operatorCapabilities, capability)
	}
	if p.Kind != CallerAgentChat {
		return false
	}
	return slices.Contains(grants[CallerGrantKey{Kind: p.Kind, Role: p.Role}], capability)
}

// CallerOrigin is what a forwarding client asserts about the process it runs
// for. An assertion bossd cannot read is the unattested kind.
type CallerOrigin struct {
	Kind           CallerKind
	AgentSessionID string
	SessionID      string
	Role           AgentChatRole
	Host           string
}

// CapToOrigin lowers an operator attribution to the asserted origin. A call
// that reached the socket through a forward is attributed to the forwarder
// (sshd), which ancestry reads as the operator; the client's assertion is the
// only fact about the real caller. It never raises authority: every asserted
// kind holds at most the operator's capabilities, and a principal already
// below the operator is returned unchanged.
func (p CallerPrincipal) CapToOrigin(origin *CallerOrigin) CallerPrincipal {
	if origin == nil || p.Kind != CallerOperator {
		return p
	}
	capped := CallerPrincipal{Kind: CallerUnattested, PID: p.PID, Forwarded: true, ForwardedFrom: origin.Host}
	switch origin.Kind {
	case CallerOperator, CallerDaemonChild:
		capped.Kind = origin.Kind
	case CallerAgentChat:
		capped.Kind = CallerAgentChat
		capped.AgentSessionID = origin.AgentSessionID
		capped.SessionID = origin.SessionID
		capped.Role = origin.Role
	case CallerUnattested:
	}
	return capped
}

// ClassifyCaller gives chat panes priority over the daemon in an attested ancestry.
func ClassifyCaller(chain []int, panesByPID map[int]CallerPrincipal, daemonPID int) CallerPrincipal {
	if len(chain) == 0 {
		return CallerPrincipal{Kind: CallerUnattested}
	}
	for _, pid := range chain {
		if chat, ok := panesByPID[pid]; ok {
			chat.Kind = CallerAgentChat
			chat.PID = chain[0]
			return chat
		}
	}
	kind := CallerOperator
	if slices.Contains(chain, daemonPID) {
		kind = CallerDaemonChild
	}
	return CallerPrincipal{Kind: kind, PID: chain[0]}
}
