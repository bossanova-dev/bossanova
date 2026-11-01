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
type CallerPrincipal struct {
	Kind           CallerKind
	AgentSessionID string
	SessionID      string
	Role           AgentChatRole
	PID            int
}

// CallerCapability identifies an operator action at the daemon boundary.
type CallerCapability string

const (
	CapabilityVerifyApprove CallerCapability = "verify.approve"
	CapabilityOperatorMerge CallerCapability = "verify.operator-merge"
)

// CallerGrantKey scopes a future coordinator grant to a principal kind and role.
type CallerGrantKey struct {
	Kind CallerKind
	Role AgentChatRole
}

// CallerGrants extends the operator's default capabilities. An empty table grants nothing.
type CallerGrants map[CallerGrantKey][]CallerCapability

// HasCapability applies the default operator grant and an optional role grant.
func (p CallerPrincipal) HasCapability(capability CallerCapability, grants CallerGrants) bool {
	if p.Kind == CallerOperator {
		return capability == CapabilityVerifyApprove || capability == CapabilityOperatorMerge
	}
	if p.Kind != CallerAgentChat {
		return false
	}
	return slices.Contains(grants[CallerGrantKey{Kind: p.Kind, Role: p.Role}], capability)
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
