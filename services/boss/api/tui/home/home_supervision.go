// Home's daemon-supervision health line (BOS-1520): the TUI half of the
// per-invocation "the serving bossd is not under the service manager" warning.
// The stderr warning is printed before the alt screen takes over, so a user who
// lives in the TUI never sees it; this puts the same verdict on Home.

package home

import (
	"fmt"
	"os"
	"time"

	"github.com/recurser/boss/api/tui/ui"

	tea "charm.land/bubbletea/v2"
)

// DaemonSupervision is what one supervision probe observed about the local
// daemon. The zero value is "not unsupervised", which is also how an unknown
// verdict is reported: an advisory for a probe that could not tell trains
// operators to ignore it, and `boss daemon doctor` is where unknowns are
// explained.
type DaemonSupervision struct {
	// Unsupervised is true only for a live daemon the service manager does not
	// own.
	Unsupervised bool
	// PID is the recorded PID of that daemon, or 0 when none is known.
	PID int
}

// DaemonSupervisionProbe observes the local daemon's supervision. It may run a
// service-manager subprocess, so Home only ever calls it from a tea.Cmd, never
// on the render path. The cmd package builds it; views cannot import cmd.
type DaemonSupervisionProbe func() DaemonSupervision

// daemonSupervisionProbeInterval bounds how often the poll tick re-probes.
// It does not bound Init: every (re)entry to Home probes once more, so the
// subprocess rate is one per minute plus one per return to Home.
const daemonSupervisionProbeInterval = 60 * time.Second

// daemonSupervisionLineLimit bounds one unsupervised verdict's Home advisory.
const daemonSupervisionLineLimit = time.Minute

func daemonSupervisionLineVisible(s DaemonSupervision, now, since time.Time) bool {
	if !s.Unsupervised {
		return false
	}
	return since.IsZero() || now.Before(since.Add(daemonSupervisionLineLimit))
}

// statusDaemonUnsupervisedFormat / statusDaemonUnsupervisedNoPID are the
// health line's copy, with and without a recorded PID.
const (
	statusDaemonUnsupervisedFormat = "The daemon (PID %d) is not supervised, so it will not restart after a crash or reboot."
	statusDaemonUnsupervisedNoPID  = "The daemon is not supervised, so it will not restart after a crash or reboot."
)

// proofDaemonUnsupervised, when set via BOSS_PROOF_DAEMON_UNSUPERVISED (proof
// scenarios only), makes a registered probe report unsupervised, so the line
// can be captured on an inert verify stack whose bossd is deliberately not
// service-manager owned and so would otherwise read as unknown. It never
// conjures a probe: a TUI driving another machine's daemon stays silent.
// Mirrors proofUpgradeRateLimit in home_upgrade.go.
var proofDaemonUnsupervised = os.Getenv("BOSS_PROOF_DAEMON_UNSUPERVISED") != ""

// daemonSupervisionMsg carries one probe's result back to the Home that
// issued it. homeGeneration and probeID let a result from a replaced Home, or
// one superseded by a later probe, be discarded rather than overwrite a newer
// answer.
type DaemonSupervisionMsg struct {
	HomeGeneration uint64
	ProbeID        uint64
	Result         DaemonSupervision
}

// SetDaemonSupervisionProbe registers the probe on Home. A registered probe is
// treated as in flight and as just issued, because Init — which every path that
// builds a Home calls next — issues it; that keeps the first poll tick from
// starting a second probe alongside it.
func (h *Model) SetDaemonSupervisionProbe(probe DaemonSupervisionProbe) {
	h.supervisionProbe = probe
	h.SupervisionProbing = probe != nil
	h.supervisionProbedAt = h.currentTime()
}

// daemonSupervisionProbeCmd runs the probe for the current probe ID, or
// returns nil when no probe is registered (the --host / --remote / BOSS_SOCKET
// case, where this machine's supervision says nothing about the daemon the
// user is driving).
func (h Model) DaemonSupervisionProbeCmd() tea.Cmd {
	probe := h.supervisionProbe
	if probe == nil {
		return nil
	}
	generation, probeID := h.Generation, h.SupervisionProbeID
	return func() tea.Msg {
		result := probe()
		if proofDaemonUnsupervised {
			result.Unsupervised = true
		}
		return DaemonSupervisionMsg{HomeGeneration: generation, ProbeID: probeID, Result: result}
	}
}

// startDaemonSupervisionProbe issues a fresh probe, superseding any in flight.
func (h *Model) startDaemonSupervisionProbe() tea.Cmd {
	if h.supervisionProbe == nil {
		return nil
	}
	h.SupervisionProbeID++
	h.SupervisionProbing = true
	h.supervisionProbedAt = h.currentTime()
	return h.DaemonSupervisionProbeCmd()
}

// tickDaemonSupervisionProbe is the poll tick's re-probe: at most once per
// daemonSupervisionProbeInterval and never while a probe is in flight. It
// rides the existing tick rather than starting a second self-perpetuating tick
// chain.
func (h *Model) tickDaemonSupervisionProbe() tea.Cmd {
	if h.supervisionProbe == nil || h.SupervisionProbing {
		return nil
	}
	if h.currentTime().Sub(h.supervisionProbedAt) < daemonSupervisionProbeInterval {
		return nil
	}
	return h.startDaemonSupervisionProbe()
}

func (h Model) handleDaemonSupervision(msg DaemonSupervisionMsg) (tea.Model, tea.Cmd) {
	if msg.HomeGeneration != h.Generation || msg.ProbeID != h.SupervisionProbeID {
		return h, nil
	}
	h.SupervisionProbing = false
	h.Supervision = msg.Result
	if msg.Result.Unsupervised && (h.SupervisionSince.IsZero() || msg.Result.PID != h.SupervisionSincePID) {
		h.SupervisionSince = h.currentTime()
		h.SupervisionSincePID = msg.Result.PID
	}
	// The line adds or removes footer rows below the table; refresh the cached
	// table height now rather than at the next poll.
	h.Table.SetHeight(h.TableHeight())
	return h, nil
}

// daemonSupervisionLine renders the unsupervised-daemon advisory, or "" when
// the last probe found the daemon supervised, could not tell, or never ran.
func (h Model) DaemonSupervisionLine() string {
	since := h.SupervisionSince
	if since.IsZero() {
		since = h.StartedAt
	}
	if !daemonSupervisionLineVisible(h.Supervision, h.currentTime(), since) {
		return ""
	}
	text := statusDaemonUnsupervisedNoPID
	if h.Supervision.PID > 0 {
		text = fmt.Sprintf(statusDaemonUnsupervisedFormat, h.Supervision.PID)
	}
	return h.statusLine(ui.ColorWarning, text)
}
