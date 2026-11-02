package credsync

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"sync"
	"time"
)

// ReadinessState is the daemon's credential-sharing verdict, in precedence order.
type ReadinessState string

// Readiness states. Unavailable and unknown never warn and never gate selection.
const (
	ReadinessUnavailable       ReadinessState = "unavailable"
	ReadinessUnknown           ReadinessState = "unknown"
	ReadinessRevoked           ReadinessState = "revoked"
	ReadinessUnenrolled        ReadinessState = "unenrolled"
	ReadinessEnrollmentExpired ReadinessState = "enrollment_expired"
	ReadinessEnrollmentPending ReadinessState = "enrollment_pending"
	ReadinessBootstrapUnpinned ReadinessState = "bootstrap_unpinned"
	ReadinessReady             ReadinessState = "ready"
)

// RemediationStep is one command and the machine that runs it. An empty
// MachineDaemonID on another machine means any enrolled machine.
type RemediationStep struct {
	MachineDaemonID string
	IsThisMachine   bool
	Command         string
}

// Readiness carries the verdict and the commands that move this daemon to ready.
type Readiness struct {
	State       ReadinessState
	DaemonID    string
	Fingerprint string
	ExpiresAt   time.Time
	// BootstrapDaemonID is the identity to pin: the bootstrap, or the first
	// unpinned approver on this daemon's chain.
	BootstrapDaemonID string
	ApproverDaemonIDs []string
	RemediationSteps  []RemediationStep
	CheckedAt         time.Time
}

// IsWarning reports a verdict an operator must act on.
func (r Readiness) IsWarning() bool {
	return r.State != "" && r.State != ReadinessReady && r.State != ReadinessUnknown && r.State != ReadinessUnavailable
}

// ReadinessInput is everything the evaluator reads. Fingerprint and
// DevicePublicKey are derived locally, never taken from bosso.
type ReadinessInput struct {
	IsConfigured    bool
	DaemonID        string
	Fingerprint     string
	DevicePublicKey []byte
	Devices         []Device
	Err             error
	Pins            PinLookup
	Now             time.Time
}

// EvaluateReadiness is pure over the device list, the pin lookup and the clock.
func EvaluateReadiness(in ReadinessInput) Readiness {
	r := Readiness{State: ReadinessUnavailable, DaemonID: in.DaemonID, Fingerprint: in.Fingerprint, CheckedAt: in.Now}
	switch {
	case !in.IsConfigured || in.Pins == nil:
		return r
	case in.Err != nil:
		r.State = ReadinessUnknown
		return r
	}
	r.State = ownRowState(in, &r)
	if r.State == ReadinessReady {
		b, err := FindBootstrap(in.Devices, in.Pins)
		r.BootstrapDaemonID = displayableDaemonID(b.CandidateDaemonID)
		if errors.Is(err, ErrBootstrapUnpinned) {
			r.State = ReadinessBootstrapUnpinned
		} else if approver, unpinned := UnpinnedApprover(in.Devices, in.DaemonID, in.Pins); unpinned {
			r.State, r.BootstrapDaemonID = ReadinessBootstrapUnpinned, displayableDaemonID(approver)
		}
	}
	for _, d := range in.Devices {
		if d.DaemonID != in.DaemonID && d.Status == "enrolled" {
			r.ApproverDaemonIDs = append(r.ApproverDaemonIDs, d.DaemonID)
		}
	}
	sort.Strings(r.ApproverDaemonIDs)
	r.RemediationSteps = remediationSteps(r)
	return r
}

func ownRowState(in ReadinessInput, r *Readiness) ReadinessState {
	for _, d := range in.Devices {
		if d.DaemonID != in.DaemonID {
			continue
		}
		if len(in.DevicePublicKey) == 0 || !bytes.Equal(d.DevicePublicKey, in.DevicePublicKey) {
			return ReadinessUnenrolled
		}
		switch d.Status {
		case "enrolled":
			return ReadinessReady
		case "revoked":
			return ReadinessRevoked
		case "expired":
			r.ExpiresAt = d.ExpiresAt
			return ReadinessEnrollmentExpired
		case "pending":
			r.ExpiresAt = d.ExpiresAt
			if !d.ExpiresAt.IsZero() && !in.Now.Before(d.ExpiresAt) {
				return ReadinessEnrollmentExpired
			}
			return ReadinessEnrollmentPending
		default:
			return ReadinessUnenrolled
		}
	}
	return ReadinessUnenrolled
}

const requestCommand = "boss account devices request"

// daemonIDPattern mirrors bosso's daemon ID rule. Bosso supplies the IDs, so a
// compromised bosso could otherwise smuggle flags into a copy-paste command.
var daemonIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// displayableDaemonID drops an ID that is not a plain daemon ID, so remediation
// falls back to the placeholder instead of printing it.
func displayableDaemonID(id string) string {
	if daemonIDPattern.MatchString(id) {
		return id
	}
	return ""
}

// remediationSteps is the one place remediation commands are built. The
// bootstrap fingerprint is never pre-filled: pinning verifies it out of band,
// so a value from bosso would let bosso choose what gets pinned.
func remediationSteps(r Readiness) []RemediationStep {
	self := RemediationStep{MachineDaemonID: r.DaemonID, IsThisMachine: true}
	approve := RemediationStep{Command: fmt.Sprintf("boss account devices approve %s --fingerprint %s", r.DaemonID, r.Fingerprint)}
	switch r.State {
	case ReadinessEnrollmentExpired, ReadinessUnenrolled:
		self.Command = requestCommand
		return []RemediationStep{self, approve}
	case ReadinessEnrollmentPending:
		return []RemediationStep{approve}
	case ReadinessRevoked:
		self.Command = requestCommand
		return []RemediationStep{self}
	case ReadinessBootstrapUnpinned:
		bootstrap := r.BootstrapDaemonID
		if bootstrap == "" {
			bootstrap = "<bootstrap-daemon-id>"
		}
		self.Command = fmt.Sprintf("boss account devices approve %s --fingerprint <fingerprint-shown-on-%s>", bootstrap, bootstrap)
		return []RemediationStep{{MachineDaemonID: r.BootstrapDaemonID, Command: "boss account devices status"}, self}
	default:
		return nil
	}
}

// ReadinessHolder caches the latest verdict for concurrent readers.
type ReadinessHolder struct {
	mu     sync.Mutex
	latest Readiness
	warned ReadinessState
}

// Load returns the cached verdict; its State is empty before the first evaluation.
func (h *ReadinessHolder) Load() Readiness {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.latest
}

// Store caches r and reports whether it is a transition into a warning state.
// Unknown keeps the last warned state, so a transient outage cannot re-warn.
func (h *ReadinessHolder) Store(r Readiness) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.latest = r
	if r.State == ReadinessUnknown || r.State == h.warned {
		return false
	}
	h.warned = r.State
	return r.IsWarning()
}
