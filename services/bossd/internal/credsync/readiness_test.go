package credsync

import (
	"crypto/ed25519"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

var (
	readinessNow   = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	readinessOwn   = []byte("own-device-key")
	readinessOther = []byte("other-device-key")
)

func readinessPins(ids ...string) PinLookup {
	return func(id string) (ed25519.PublicKey, error) {
		if slices.Contains(ids, id) {
			return make(ed25519.PublicKey, ed25519.PublicKeySize), nil
		}
		return nil, errors.New("not pinned")
	}
}

func readinessInput(pins PinLookup, devices ...Device) ReadinessInput {
	return ReadinessInput{IsConfigured: true, DaemonID: "self", Fingerprint: "SELF-FP", DevicePublicKey: readinessOwn, Devices: devices, Pins: pins, Now: readinessNow}
}

var readinessBoot = Device{DaemonID: "boot", Status: "enrolled", ApprovedByDaemonID: "boot", DevicePublicKey: []byte("boot-key")}

// readinessMid sits between the bootstrap and this daemon: boot -> mid -> self.
var readinessMid = Device{DaemonID: "mid", Status: "enrolled", ApprovedByDaemonID: "boot", DevicePublicKey: []byte("mid-key")}

func TestEvaluateReadinessStates(t *testing.T) {
	enrolled := Device{DaemonID: "self", Status: "enrolled", ApprovedByDaemonID: "boot", DevicePublicKey: readinessOwn}
	viaMid := Device{DaemonID: "self", Status: "enrolled", ApprovedByDaemonID: "mid", DevicePublicKey: readinessOwn}
	pending := func(expires time.Time) Device {
		return Device{DaemonID: "self", Status: "pending", DevicePublicKey: readinessOwn, ExpiresAt: expires}
	}
	for _, tc := range []struct {
		name      string
		in        ReadinessInput
		want      ReadinessState
		bootstrap string
	}{
		{"enrolled with pinned bootstrap", readinessInput(readinessPins("boot"), readinessBoot, enrolled), ReadinessReady, "boot"},
		{"enrolled without pinned bootstrap", readinessInput(readinessPins(), readinessBoot, enrolled), ReadinessBootstrapUnpinned, "boot"},
		{"intermediate approver unpinned", readinessInput(readinessPins("boot"), readinessBoot, readinessMid, viaMid), ReadinessBootstrapUnpinned, "mid"},
		{"whole chain pinned", readinessInput(readinessPins("boot", "mid"), readinessBoot, readinessMid, viaMid), ReadinessReady, "boot"},
		{"elapsed pending", readinessInput(readinessPins("boot"), readinessBoot, pending(readinessNow)), ReadinessEnrollmentExpired, ""},
		{"bosso-reported expired", readinessInput(readinessPins("boot"), readinessBoot, Device{DaemonID: "self", Status: "expired", DevicePublicKey: readinessOwn}), ReadinessEnrollmentExpired, ""},
		{"unexpired pending", readinessInput(readinessPins("boot"), readinessBoot, pending(readinessNow.Add(time.Minute))), ReadinessEnrollmentPending, ""},
		{"no row", readinessInput(readinessPins("boot"), readinessBoot), ReadinessUnenrolled, ""},
		{"different key", readinessInput(readinessPins("boot"), readinessBoot, Device{DaemonID: "self", Status: "enrolled", ApprovedByDaemonID: "boot", DevicePublicKey: readinessOther}), ReadinessUnenrolled, ""},
		{"revoked", readinessInput(readinessPins("boot"), readinessBoot, Device{DaemonID: "self", Status: "revoked", DevicePublicKey: readinessOwn}), ReadinessRevoked, ""},
		{"list error", func() ReadinessInput {
			in := readinessInput(readinessPins("boot"))
			in.Err = errors.New("bosso unreachable")
			return in
		}(), ReadinessUnknown, ""},
		{"missing identity or upstream", ReadinessInput{DaemonID: "self", Pins: readinessPins("boot"), Now: readinessNow}, ReadinessUnavailable, ""},
		{"credential sharing unconfigured", readinessInput(nil, readinessBoot, enrolled), ReadinessUnavailable, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := EvaluateReadiness(tc.in)
			if got.State != tc.want || got.BootstrapDaemonID != tc.bootstrap {
				t.Fatalf("state=%q bootstrap=%q, want %q %q", got.State, got.BootstrapDaemonID, tc.want, tc.bootstrap)
			}
			if !got.CheckedAt.Equal(readinessNow) {
				t.Errorf("checked_at=%v", got.CheckedAt)
			}
			if wantSteps := got.IsWarning(); wantSteps != (len(got.RemediationSteps) > 0) {
				t.Errorf("warning=%t steps=%v", got.IsWarning(), got.RemediationSteps)
			}
		})
	}
}

func TestReadinessApproversExcludeSelfAndUnenrolled(t *testing.T) {
	got := EvaluateReadiness(readinessInput(readinessPins("boot"), readinessBoot,
		Device{DaemonID: "zed", Status: "enrolled", ApprovedByDaemonID: "boot"},
		Device{DaemonID: "gone", Status: "revoked", ApprovedByDaemonID: "boot"},
		Device{DaemonID: "self", Status: "pending", DevicePublicKey: readinessOwn, ExpiresAt: readinessNow.Add(time.Hour)}))
	if !slices.Equal(got.ApproverDaemonIDs, []string{"boot", "zed"}) {
		t.Fatalf("approvers=%v", got.ApproverDaemonIDs)
	}
	if !got.ExpiresAt.Equal(readinessNow.Add(time.Hour)) || len(got.RemediationSteps) != 1 || got.RemediationSteps[0].IsThisMachine {
		t.Fatalf("pending readiness=%+v", got)
	}
}

// The engine and readiness share FindBootstrap and the per-hop pin walk:
// readiness reports bootstrap_unpinned exactly when the engine's root search
// refuses or a hop on this daemon's approval chain has no pin.
func TestReadinessUnpinnedMatchesEngineBootstrapSearch(t *testing.T) {
	enrolled := Device{DaemonID: "self", Status: "enrolled", ApprovedByDaemonID: "boot", DevicePublicKey: readinessOwn}
	revokedBoot := Device{DaemonID: "old", Status: "revoked", ApprovedByDaemonID: "old"}
	pendingSelfApproved := Device{DaemonID: "pend", Status: "pending", ApprovedByDaemonID: "pend"}
	notSelfApproved := Device{DaemonID: "peer", Status: "enrolled", ApprovedByDaemonID: "boot"}
	for _, tc := range []struct {
		name    string
		pins    PinLookup
		devices []Device
	}{
		{"pinned bootstrap", readinessPins("boot"), []Device{readinessBoot, enrolled}},
		{"unpinned bootstrap", readinessPins(), []Device{readinessBoot, enrolled}},
		{"pinned revoked bootstrap", readinessPins("old"), []Device{revokedBoot, enrolled}},
		{"pinned pending self-approval", readinessPins("pend"), []Device{pendingSelfApproved, enrolled}},
		{"pinned peer not self-approved", readinessPins("peer"), []Device{readinessBoot, notSelfApproved, enrolled}},
		{"second bootstrap pinned", readinessPins("boot2"), []Device{readinessBoot, {DaemonID: "boot2", Status: "enrolled", ApprovedByDaemonID: "boot2"}, enrolled}},
		{"no self-approved device", readinessPins("peer"), []Device{notSelfApproved, enrolled}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, engineErr := FindBootstrap(tc.devices, tc.pins)
			_, chainUnpinned := UnpinnedApprover(tc.devices, "self", tc.pins)
			unpinned := EvaluateReadiness(readinessInput(tc.pins, tc.devices...)).State == ReadinessBootstrapUnpinned
			if unpinned != (errors.Is(engineErr, ErrBootstrapUnpinned) || chainUnpinned) {
				t.Fatalf("readiness unpinned=%t, engine error=%v, chain unpinned=%t", unpinned, engineErr, chainUnpinned)
			}
		})
	}
}

func TestFindBootstrapNamesCandidateWhenUnpinned(t *testing.T) {
	b, err := FindBootstrap([]Device{{DaemonID: "old", Status: "revoked", ApprovedByDaemonID: "old"}, readinessBoot}, readinessPins())
	if !errors.Is(err, ErrBootstrapUnpinned) || b.RootDaemonID != "" || b.CandidateDaemonID != "boot" {
		t.Fatalf("bootstrap=%+v err=%v", b, err)
	}
}

func TestRemediationFingerprints(t *testing.T) {
	enrolled := Device{DaemonID: "self", Status: "enrolled", ApprovedByDaemonID: "boot", DevicePublicKey: readinessOwn}
	unpinned := EvaluateReadiness(readinessInput(readinessPins(), readinessBoot, enrolled))
	if len(unpinned.RemediationSteps) != 2 || unpinned.RemediationSteps[0].MachineDaemonID != "boot" || !unpinned.RemediationSteps[1].IsThisMachine {
		t.Fatalf("unpinned steps=%+v", unpinned.RemediationSteps)
	}
	for _, step := range unpinned.RemediationSteps {
		if strings.Contains(step.Command, unpinned.Fingerprint) {
			t.Errorf("bootstrap remediation carries a fingerprint: %q", step.Command)
		}
	}
	if _, value, ok := strings.Cut(unpinned.RemediationSteps[1].Command, "--fingerprint "); !ok || !strings.HasPrefix(value, "<") {
		t.Errorf("bootstrap fingerprint is pre-filled: %q", unpinned.RemediationSteps[1].Command)
	}
	if !strings.Contains(unpinned.RemediationSteps[1].Command, "approve boot ") {
		t.Errorf("approve step does not name the bootstrap: %q", unpinned.RemediationSteps[1].Command)
	}
	expired := EvaluateReadiness(readinessInput(readinessPins("boot"), readinessBoot, Device{DaemonID: "self", Status: "expired", DevicePublicKey: readinessOwn}))
	if len(expired.RemediationSteps) != 2 || !expired.RemediationSteps[0].IsThisMachine || expired.RemediationSteps[0].Command != "boss account devices request" {
		t.Fatalf("expired steps=%+v", expired.RemediationSteps)
	}
	if want := "boss account devices approve self --fingerprint SELF-FP"; expired.RemediationSteps[1].Command != want || expired.RemediationSteps[1].IsThisMachine {
		t.Errorf("approve step=%+v, want %q", expired.RemediationSteps[1], want)
	}
}

func TestReadinessHolderWarnsOncePerTransition(t *testing.T) {
	var h ReadinessHolder
	var warned []ReadinessState
	for _, state := range []ReadinessState{ReadinessReady, ReadinessEnrollmentPending, ReadinessEnrollmentPending, ReadinessUnknown, ReadinessEnrollmentPending, ReadinessEnrollmentExpired, ReadinessUnavailable, ReadinessReady, ReadinessRevoked} {
		if h.Store(Readiness{State: state}) {
			warned = append(warned, state)
		}
		if h.Load().State != state {
			t.Fatalf("holder=%q, want %q", h.Load().State, state)
		}
	}
	if want := []ReadinessState{ReadinessEnrollmentPending, ReadinessEnrollmentExpired, ReadinessRevoked}; !slices.Equal(warned, want) {
		t.Fatalf("warned=%v, want %v", warned, want)
	}
}

// The engine's chain check needs a pin at every hop, so an unpinned
// intermediate approver is not ready and remediation pins that approver.
func TestReadinessPinsUnpinnedIntermediateApprover(t *testing.T) {
	viaMid := Device{DaemonID: "self", Status: "enrolled", ApprovedByDaemonID: "mid", DevicePublicKey: readinessOwn}
	got := EvaluateReadiness(readinessInput(readinessPins("boot"), readinessBoot, readinessMid, viaMid))
	if got.State != ReadinessBootstrapUnpinned || len(got.RemediationSteps) != 2 {
		t.Fatalf("readiness=%+v", got)
	}
	if got.RemediationSteps[0].MachineDaemonID != "mid" || !strings.Contains(got.RemediationSteps[1].Command, "approve mid --fingerprint <") {
		t.Fatalf("steps=%+v", got.RemediationSteps)
	}
	if id, unpinned := UnpinnedApprover([]Device{readinessBoot, readinessMid, viaMid}, "self", readinessPins("boot", "mid")); unpinned {
		t.Fatalf("fully pinned chain reported %q", id)
	}
}

// A revoked intermediate approver cannot be fixed by pinning it (bosso refuses
// to approve a non-pending device), so the walk never names it.
func TestUnpinnedApproverSkipsRevokedApprover(t *testing.T) {
	revokedMid := Device{DaemonID: "mid", Status: "revoked", ApprovedByDaemonID: "boot", DevicePublicKey: []byte("mid-key")}
	viaMid := Device{DaemonID: "self", Status: "enrolled", ApprovedByDaemonID: "mid", DevicePublicKey: readinessOwn}
	if id, unpinned := UnpinnedApprover([]Device{readinessBoot, revokedMid, viaMid}, "self", readinessPins("boot")); unpinned {
		t.Fatalf("revoked approver %q named for pinning", id)
	}
	got := EvaluateReadiness(readinessInput(readinessPins("boot"), readinessBoot, revokedMid, viaMid))
	for _, step := range got.RemediationSteps {
		if strings.Contains(step.Command, "approve mid") {
			t.Fatalf("remediation approves revoked approver: %+v", got.RemediationSteps)
		}
	}
}

// Bosso supplies daemon IDs, so one that is not a plain daemon ID must never
// reach a copy-paste command: a hostile bootstrap or intermediate approver
// falls back to the placeholder.
func TestRemediationRefusesHostileDaemonID(t *testing.T) {
	const hostile = "boot --fingerprint ATTACKER-FP #"
	hostileBoot := Device{DaemonID: hostile, Status: "enrolled", ApprovedByDaemonID: hostile, DevicePublicKey: []byte("boot-key")}
	hostileMid := Device{DaemonID: "mid --fingerprint ATTACKER-FP #", Status: "enrolled", ApprovedByDaemonID: "boot", DevicePublicKey: []byte("mid-key")}
	for _, tc := range []struct {
		name string
		in   ReadinessInput
	}{
		{"bootstrap", readinessInput(readinessPins(), hostileBoot, Device{DaemonID: "self", Status: "enrolled", ApprovedByDaemonID: hostile, DevicePublicKey: readinessOwn})},
		{"intermediate approver", readinessInput(readinessPins("boot"), readinessBoot, hostileMid, Device{DaemonID: "self", Status: "enrolled", ApprovedByDaemonID: hostileMid.DaemonID, DevicePublicKey: readinessOwn})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := EvaluateReadiness(tc.in)
			if got.State != ReadinessBootstrapUnpinned || got.BootstrapDaemonID != "" || len(got.RemediationSteps) != 2 {
				t.Fatalf("readiness=%+v", got)
			}
			if got.RemediationSteps[0].MachineDaemonID != "" {
				t.Fatalf("status step names hostile machine: %+v", got.RemediationSteps)
			}
			if want := "boss account devices approve <bootstrap-daemon-id> --fingerprint <fingerprint-shown-on-<bootstrap-daemon-id>>"; got.RemediationSteps[1].Command != want {
				t.Fatalf("command=%q, want %q", got.RemediationSteps[1].Command, want)
			}
		})
	}
}
