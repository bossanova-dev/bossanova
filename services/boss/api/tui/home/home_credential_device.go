package home

import (
	"context"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/api/tui/ui"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

const credentialDevicePollInterval = time.Minute

// CredentialDeviceReadinessMsg carries the daemon's machine-wide verdict.
// A failed RPC clears the warning, including Unimplemented from older daemons.
type CredentialDeviceReadinessMsg struct {
	Readiness  *pb.CredentialDeviceReadiness
	Generation uint64
}

func (h *Model) tickCredentialDeviceReadiness() tea.Cmd {
	now := h.currentTime()
	if h.Client == nil || h.credentialDevicePolling || (!h.credentialDevicePolledAt.IsZero() && now.Sub(h.credentialDevicePolledAt) < credentialDevicePollInterval) {
		return nil
	}
	h.credentialDevicePolling = true
	h.credentialDevicePolledAt = now
	client, ctx, generation := h.Client, h.Ctx, h.Generation
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, upgradeCheckTimeout)
		defer cancel()
		result, err := client.GetAgentCredentialDeviceStatus(ctx)
		msg := CredentialDeviceReadinessMsg{Generation: generation}
		if err == nil {
			msg.Readiness = result.GetReadiness()
		}
		return msg
	}
}

func (h Model) handleCredentialDeviceReadiness(msg CredentialDeviceReadinessMsg) Model {
	if msg.Generation != h.Generation {
		return h
	}
	h.credentialDevicePolling = false
	h.CredentialDeviceReadiness = msg.Readiness
	h.Table.SetHeight(h.TableHeight())
	return h
}

// CredentialDeviceLine keeps remediation tokens intact at the status width floor.
func (h Model) CredentialDeviceLine() string {
	state := h.CredentialDeviceReadiness.GetState()
	switch state {
	case "enrollment_expired", "enrollment_pending", "unenrolled", "revoked", "bootstrap_unpinned":
	default:
		return ""
	}
	lines := []string{
		"Credential sharing is off on this machine:",
		strings.ReplaceAll(state, "_", " ") + ". Synced Codex accounts are paused.",
	}
	for _, step := range h.CredentialDeviceReadiness.GetRemediationSteps() {
		machine := step.GetMachineDaemonId()
		if step.GetIsThisMachine() {
			machine = "this machine"
		} else if machine == "" {
			machine = "an enrolled machine"
		}
		lines = append(lines, "On "+machine+": "+step.GetCommand())
	}
	return h.statusLine(ui.ColorWarning, strings.Join(lines, "\n"))
}

// RetainCredentialDeviceReadiness carries the warning and poll cadence across Home rebuilds.
// In-flight results belong to the old generation; the replacement resumes on its next due poll.
func (h *Model) RetainCredentialDeviceReadiness(previous Model) {
	h.CredentialDeviceReadiness = previous.CredentialDeviceReadiness
	h.credentialDevicePolledAt = previous.credentialDevicePolledAt
}
