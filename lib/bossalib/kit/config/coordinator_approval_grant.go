package config

import (
	"fmt"
	"strings"
)

// CoordinatorApprovalGrant says whether bossd grants a coordinator-role chat
// verify approval and operator merge at its socket caller gate.
type CoordinatorApprovalGrant string

const (
	// CoordinatorApprovalGrantOn is the shipped default: a coordinator chat may
	// approve and merge, and every such action names the chat in its audit trail.
	CoordinatorApprovalGrantOn CoordinatorApprovalGrant = "on"
	// CoordinatorApprovalGrantOff is the operator opt-out: no chat role holds
	// any operator capability.
	CoordinatorApprovalGrantOff CoordinatorApprovalGrant = "off"
)

// parseCoordinatorApprovalGrant maps a settings string to a mode and, when the
// configured value was not honoured as written, the reason.
//
// Absent or blank keeps the default grant. An unrecognised value withholds the
// grant: it is the opposite direction from subagent_dispatch_grant because the
// fallback here widens authority, and the only reason to write the key is to
// change the default. Case and surrounding whitespace are normalised.
func parseCoordinatorApprovalGrant(raw string) (CoordinatorApprovalGrant, string) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return CoordinatorApprovalGrantOn, ""
	}
	switch mode := CoordinatorApprovalGrant(strings.ToLower(trimmed)); mode {
	case CoordinatorApprovalGrantOn, CoordinatorApprovalGrantOff:
		return mode, ""
	}
	return CoordinatorApprovalGrantOff, fmt.Sprintf(
		"coordinator_approval_grant %q is not a recognised mode (%q or %q); withholding the grant",
		trimmed, CoordinatorApprovalGrantOn, CoordinatorApprovalGrantOff)
}

// ResolveCoordinatorApprovalGrant maps loaded settings to the mode bossd acts
// on, with the reason when the configured value was not honoured. Nil settings
// (none could be loaded) withhold the grant: an opt-out the daemon cannot read
// must not be dropped.
func ResolveCoordinatorApprovalGrant(s *Settings) (CoordinatorApprovalGrant, string) {
	if s == nil {
		return CoordinatorApprovalGrantOff, "settings could not be loaded; withholding coordinator_approval_grant"
	}
	return parseCoordinatorApprovalGrant(s.CoordinatorApprovalGrant)
}
