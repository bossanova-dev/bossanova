package credsync

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/recurser/bossd/internal/devicekey"
)

// previouslyAdmitted compares exact locally verified evidence. Server timestamps
// cannot make a fresh signature from a revoked signer historical.
func (e *Engine) previouslyAdmitted(d Device) bool {
	previous, ok := e.admitted[d.DaemonID]
	if !ok {
		var err error
		previous, err = e.trust.ApprovedDevice(d.DaemonID)
		ok = err == nil
	}

	return ok && previous.ApprovedByDaemonID == d.ApprovedByDaemonID && bytes.Equal(previous.DevicePublicKey, d.DevicePublicKey) && bytes.Equal(previous.IdentityPublicKey, d.IdentityPublicKey) && bytes.Equal(previous.ApprovalSignature, d.ApprovalSignature)
}

// verifiedDevices proves each recipient's approval chain back to the local root pin.
func (e *Engine) verifiedDevices(devices []Device) ([]Device, error) {
	if e.trust == nil || e.config.BootstrapDaemonID == "" {
		return nil, errors.New("credential sync: bootstrap identity is not pinned")
	}
	byID := map[string]Device{}
	for _, d := range devices {
		if _, ok := byID[d.DaemonID]; ok {
			return nil, errors.New("credential sync: duplicate device")
		}
		byID[d.DaemonID] = d
	}
	v := deviceVerifier{engine: e, byID: byID, verified: map[string]bool{}, visiting: map[string]bool{}}
	var result []Device
	for _, d := range devices {
		if v.verify(d.DaemonID) && d.Status == "enrolled" {
			result = append(result, d)
		}
	}
	self := false
	for _, d := range result {
		if d.DaemonID == e.config.DaemonID && e.config.DeviceKey != nil && bytes.Equal(d.DevicePublicKey, e.config.DeviceKey.PublicKey().Bytes()) {
			self = true
		}
	}
	if !self {
		return nil, errors.New("credential sync: own enrolled device or approval chain is unverified")
	}
	for id := range v.verified {
		d := byID[id]
		if err := e.trust.SaveApprovedDevice(d); err != nil {
			return nil, fmt.Errorf("credential sync: persist approved device: %w", err)
		}
		e.admitted[id] = d
	}
	return result, nil
}

type deviceVerifier struct {
	engine             *Engine
	byID               map[string]Device
	verified, visiting map[string]bool
}

func (v *deviceVerifier) verify(id string) bool {
	if v.verified[id] {
		return true
	}
	if v.visiting[id] {
		return false
	}
	v.visiting[id] = true
	defer delete(v.visiting, id)
	d, ok := v.byID[id]
	if !ok {
		return false
	}
	historical := v.engine.previouslyAdmitted(d)
	if d.Status != "enrolled" && (d.Status != "revoked" || !historical) {
		return false
	}
	approver, ok := v.byID[d.ApprovedByDaemonID]
	if !ok {
		return false
	}
	if approver.Status != "enrolled" && !historical {
		return false
	}
	if id == v.engine.config.BootstrapDaemonID {
		if d.ApprovedByDaemonID != id {
			return false
		}
	} else if !v.verify(d.ApprovedByDaemonID) {
		return false
	}
	pin, err := v.engine.trust.PinnedIdentity(approver.DaemonID)
	if err != nil || !bytes.Equal(pin, approver.IdentityPublicKey) {
		return false
	}
	if !devicekey.VerifyApproval(pin, devicekey.Approval{ApproverDaemonID: approver.DaemonID, DaemonID: d.DaemonID, DevicePub: d.DevicePublicKey}, d.ApprovalSignature) {
		return false
	}
	v.verified[id] = true
	return true
}
