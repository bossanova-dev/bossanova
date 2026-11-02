package credsync

import (
	"crypto/ed25519"
	"errors"
)

// ErrBootstrapUnpinned means no bootstrap device passes the local pin check.
var ErrBootstrapUnpinned = errors.New("credential bootstrap identity is not locally pinned; confirm its fingerprint with device approve")

// PinLookup resolves a locally confirmed signing identity.
type PinLookup func(daemonID string) (ed25519.PublicKey, error)

// Bootstrap names the pinned approval-chain root, or the device to pin when none is.
type Bootstrap struct {
	RootDaemonID      string
	CandidateDaemonID string
}

// FindBootstrap is the single bootstrap predicate shared by the engine and
// readiness: the first enrolled-or-revoked self-approved device with a local pin.
// Without one it returns ErrBootstrapUnpinned and names the enrolled
// self-approved device (else a revoked one) as the candidate to pin.
func FindBootstrap(devices []Device, pins PinLookup) (Bootstrap, error) {
	var b Bootstrap
	revokedCandidate := ""
	for _, d := range devices {
		if d.ApprovedByDaemonID != d.DaemonID || (d.Status != "enrolled" && d.Status != "revoked") {
			continue
		}
		if _, err := pins(d.DaemonID); err == nil {
			return Bootstrap{RootDaemonID: d.DaemonID, CandidateDaemonID: d.DaemonID}, nil
		}
		if d.Status == "enrolled" && b.CandidateDaemonID == "" {
			b.CandidateDaemonID = d.DaemonID
		}
		if d.Status == "revoked" && revokedCandidate == "" {
			revokedCandidate = d.DaemonID
		}
	}
	if b.CandidateDaemonID == "" {
		b.CandidateDaemonID = revokedCandidate
	}
	return b, ErrBootstrapUnpinned
}

// UnpinnedApprover walks selfID's approval chain to its self-approved root and
// returns the first enrolled approver without a local pin. The engine's chain
// verification needs a pin at every hop, not only at the root. The walk stops
// at an approver that is not enrolled: pinning it cannot make the chain verify,
// and bosso refuses to approve it, so naming it would give a remediation that
// cannot succeed.
func UnpinnedApprover(devices []Device, selfID string, pins PinLookup) (string, bool) {
	byID := make(map[string]Device, len(devices))
	for _, d := range devices {
		byID[d.DaemonID] = d
	}
	seen := map[string]bool{}
	for id := selfID; !seen[id]; {
		seen[id] = true
		d, ok := byID[id]
		if !ok || d.ApprovedByDaemonID == "" {
			return "", false
		}
		approver, ok := byID[d.ApprovedByDaemonID]
		if !ok || approver.Status != "enrolled" {
			return "", false
		}
		if _, err := pins(approver.DaemonID); err != nil {
			return approver.DaemonID, true
		}
		id = approver.DaemonID
	}
	return "", false
}
