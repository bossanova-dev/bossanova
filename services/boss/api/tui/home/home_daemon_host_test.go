package home

import (
	"errors"
	"strings"
	"testing"

	"github.com/recurser/boss/api/tui/ui"
)

// withHostDestination points the daemon-down screen at a --host destination for
// one test and restores the local wording afterwards, so an ordinary local run
// in a sibling test never inherits ssh remediation.
func withHostDestination(t *testing.T, destination string) {
	t.Helper()
	previous := ui.RemoteHostDestination()
	t.Cleanup(func() { ui.SetHostDestination(previous) })
	ui.SetHostDestination(destination)
}

// TestDaemonDownRemediationNamesTheHost is the mid-session half of the --host
// reconnect criterion: a tunnel that drops after startup never reaches the
// startup wait screen, so this daemon-down view is what the user actually sees.
// It must say the connection is being retried and name the machine that
// stopped answering — naming `boss daemon restart` would aim a --host user at
// the local daemon boss is not talking to.
func TestDaemonDownRemediationNamesTheHost(t *testing.T) {
	withHostDestination(t, "deploy@bastion.example.com")

	got := daemonDownRemediation()
	for _, want := range []string{
		"Reconnecting to deploy@bastion.example.com",
		"resumes automatically",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("remediation %q must contain %q", got, want)
		}
	}

	for _, forbidden := range []string{"boss daemon", "ssh deploy@", "boss tail", "\nbossd "} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("remediation %q must not offer the local %q", got, forbidden)
		}
	}
}

// TestDaemonDownRemediationDefaultsToLocal pins the local daemon commands
// when --host was never used, and that the destination does not leak across
// runs.
func TestDaemonDownRemediationDefaultsToLocal(t *testing.T) {
	withHostDestination(t, "")

	got := daemonDownRemediation()
	for _, want := range []string{"boss daemon restart", "boss daemon status", "boss daemon install"} {
		if !strings.Contains(got, want) {
			t.Fatalf("local remediation %q must contain %q", got, want)
		}
	}
	for _, forbidden := range []string{"ssh ", "as soon as"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("local remediation %q must not contain %q", got, forbidden)
		}
	}
}

// TestRenderDaemonErrorUsesTheHostRemediation drives the rendered view, not the
// helper alone: renderDaemonError falls back to daemonDownRemediation when the
// model carries none, so a --host drop caught before the poll threshold must
// still render the host wording.
func TestRenderDaemonErrorUsesTheHostRemediation(t *testing.T) {
	withHostDestination(t, "workstation")

	h := Model{Err: errTestDaemonDown, Width: 100}
	rendered := h.renderDaemonError()
	if !strings.Contains(rendered, "Reconnecting to workstation") {
		t.Fatalf("rendered daemon error %q must name the host", rendered)
	}
}

type testDaemonDownError struct{}

func (testDaemonDownError) Error() string { return "dial unix: connection refused" }

var errTestDaemonDown = testDaemonDownError{}

// withHostTunnelReason registers a fixed supervisor diagnosis for one test and
// restores the previous reader afterwards, so a sibling test never inherits it.
func withHostTunnelReason(t *testing.T, reason string) {
	t.Helper()
	previous := ui.SetHostTunnelReason(nil)
	t.Cleanup(func() { ui.SetHostTunnelReason(previous) })
	if reason == "" {
		ui.SetHostTunnelReason(nil)
		return
	}
	ui.SetHostTunnelReason(func() string { return reason })
}

// errForwardedSocketDial is the failure a --host RPC actually returns once the
// ssh tunnel is down: every request dials the forwarded unix socket, so the
// error names a local temp path that does not exist. This exact string reached a
// user verbatim, which is what BOS-724 was filed for.
var errForwardedSocketDial = errors.New(
	"unavailable: dial unix /var/folders/x/T/boss-host-4081179615/bossd.sock: connect: no such file or directory")

// TestRenderDaemonErrorNeverShowsTheForwardedSocketDial is acceptance criterion
// 2 on the home screen. The absence assertion is the guard: today's screen
// prints "Cannot connect to daemon (unavailable: dial unix …)" ABOVE correct
// remediation, so a test that only checked the reconnecting wording passed
// against the bug.
func TestRenderDaemonErrorNeverShowsTheForwardedSocketDial(t *testing.T) {
	withHostDestination(t, "deploy@bastion.example.com")
	withHostTunnelReason(t, "")

	rendered := normalizedRender(Model{Err: errForwardedSocketDial, Width: 100}.renderDaemonError())
	assertNoForwardedSocketDial(t, "home daemon error", rendered)
	if !strings.Contains(rendered, "deploy@bastion.example.com") {
		t.Fatalf("home daemon error %q must name the destination it is redialling", rendered)
	}
	if !strings.Contains(rendered, "Reconnecting to deploy@bastion.example.com") {
		t.Fatalf("home daemon error %q must show the reconnecting affordance", rendered)
	}
}

// TestConvertedListViewsShowTheReconnectingAffordance is the behavioural half of
// the structural guard above: routing a view through rpcErrorMessage has to
// actually change what a user sees. Repos and Trash are ordinary daemon-backed
// list screens the ticket never named, and are exactly where a --host user would
// land next after the session view already told them the tunnel was down.
func TestRPCStatusMessageSubstitutesOnlyForADroppedTunnel(t *testing.T) {
	withHostDestination(t, "deploy@bastion.example.com")
	withHostTunnelReason(t, "remotehost: ssh tunnel to deploy@bastion.example.com exited: signal: terminated")

	got := ui.RPCStatusMessage("Delete failed", errForwardedSocketDial)
	assertNoForwardedSocketDial(t, "cron delete status", got)
	if !strings.HasPrefix(got, "Delete failed: ") {
		t.Errorf("status %q must still say which action failed", got)
	}
	if !strings.Contains(got, "deploy@bastion.example.com") {
		t.Errorf("status %q must name the host that stopped answering", got)
	}
	if !strings.Contains(got, "signal: terminated") {
		t.Errorf("status %q must report why the tunnel went down", got)
	}
	if strings.ContainsAny(got, "\n") {
		t.Errorf("status %q must stay one line; a status bar cannot carry the ssh remediation block", got)
	}

	realFailure := errors.New("not_found: cron job cron-1 does not exist")
	if got := ui.RPCStatusMessage("Delete failed", realFailure); got != "Delete failed: not_found: cron job cron-1 does not exist" {
		t.Errorf("status = %q, want the daemon's own answer", got)
	}

	withHostDestination(t, "")
	if got := ui.RPCStatusMessage("Delete failed", errForwardedSocketDial); !strings.Contains(got, "dial unix") {
		t.Errorf("local status %q must keep showing the real error", got)
	}
}

// withHostTunnelHealth registers a fixed supervisor liveness answer for one test
// and restores the previous reader afterwards.
func withHostSupervisorRunning(t *testing.T, healthy bool, registered bool) {
	t.Helper()
	previous := ui.SetHostSupervisorRunning(nil)
	t.Cleanup(func() { ui.SetHostSupervisorRunning(previous) })
	if !registered {
		ui.SetHostSupervisorRunning(nil)
		return
	}
	ui.SetHostSupervisorRunning(func() bool { return healthy })
}

// TestHostRemediationStopsPromisingARedialOnceTheSupervisorIsGone is what
// consuming Start's done channel is for. A supervisor that has unwound is not
// redialling, so a screen that still says "Boss keeps redialling and resumes
// automatically" is the same silence the ticket was filed for, dressed as
// reassurance: the user waits for a reconnect nobody is attempting.
func TestHostRemediationStopsPromisingARedialOnceTheSupervisorIsGone(t *testing.T) {
	withHostDestination(t, "deploy@bastion.example.com")
	withHostTunnelReason(t, "remotehost: ssh tunnel to deploy@bastion.example.com exited: signal: terminated")
	withHostSupervisorRunning(t, false, true)

	stopped := ui.HostDownRemediation("deploy@bastion.example.com")
	if strings.Contains(stopped, "keeps redialling") {
		t.Errorf("remediation %q promises a redial after the supervisor exited", stopped)
	}
	if !strings.Contains(stopped, "boss stopped reconnecting") {
		t.Errorf("remediation %q must say nothing is reconnecting any more", stopped)
	}
	if !strings.Contains(stopped, "Restart boss to reconnect.") {
		t.Errorf("remediation %q must tell the user the one thing that recovers it", stopped)
	}
	if !strings.Contains(stopped, "signal: terminated") {
		t.Errorf("remediation %q must still report why the tunnel went down", stopped)
	}

	withHostSupervisorRunning(t, true, true)
	if live := ui.HostDownRemediation("deploy@bastion.example.com"); !strings.Contains(live, "keeps redialling") {
		t.Errorf("remediation %q must promise the redial that is actually happening", live)
	}
	withHostSupervisorRunning(t, false, false)
	if unset := ui.HostDownRemediation("deploy@bastion.example.com"); !strings.Contains(unset, "keeps redialling") {
		t.Errorf("remediation %q must not claim the supervisor died with no reader registered", unset)
	}
}

// TestRenderDaemonErrorShowsTheRetainedTunnelReason is acceptance criterion 3:
// the classified failure recordFailure already produced must be readable from a
// running TUI, with no debugger and no source access.
func TestRenderDaemonErrorShowsTheRetainedTunnelReason(t *testing.T) {
	withHostDestination(t, "deploy@bastion.example.com")
	withHostTunnelReason(t, "remotehost: ssh tunnel to deploy@bastion.example.com exited: signal: terminated")

	rendered := normalizedRender(Model{Err: errForwardedSocketDial, Width: 100}.renderDaemonError())
	if !strings.Contains(rendered, "signal: terminated") {
		t.Fatalf("home daemon error %q must report why the tunnel went down", rendered)
	}
}

// TestRenderDaemonErrorKeepsTheLocalErrorVisible is acceptance criterion 6 for
// this view: with no --host destination the raw error is still the most useful
// thing on screen and must not be swallowed.
func TestRenderDaemonErrorKeepsTheLocalErrorVisible(t *testing.T) {
	withHostDestination(t, "")
	withHostTunnelReason(t, "")

	rendered := normalizedRender(Model{Err: errForwardedSocketDial, Width: 100}.renderDaemonError())
	if !strings.Contains(rendered, "Cannot connect to daemon") {
		t.Fatalf("local daemon error %q must keep its unchanged wording", rendered)
	}
	if !strings.Contains(rendered, "boss daemon restart") {
		t.Fatalf("local daemon error %q must keep the local remediation", rendered)
	}
	if strings.Contains(rendered, "Reconnecting to") {
		t.Fatalf("local daemon error %q must not claim an ssh tunnel is being redialled", rendered)
	}
}

// TestHostDialAffordanceOnlyClaimsTransportFailures keeps the substitution
// narrow: a daemon that answers and reports a real problem must still say so,
// or --host would swallow every error the remote daemon returns.
func TestHostDialAffordanceOnlyClaimsTransportFailures(t *testing.T) {
	withHostDestination(t, "deploy@bastion.example.com")
	withHostTunnelReason(t, "")

	if _, ok := ui.HostDialAffordance(errors.New("not_found: session sess-1 does not exist")); ok {
		t.Fatal("an ordinary RPC error was replaced by the reconnecting affordance")
	}
	if _, ok := ui.HostDialAffordance(nil); ok {
		t.Fatal("a nil error produced a reconnecting affordance")
	}
	if _, ok := ui.HostDialAffordance(errForwardedSocketDial); !ok {
		t.Fatal("a forwarded-socket dial failure must yield the reconnecting affordance")
	}

	withHostDestination(t, "")
	if _, ok := ui.HostDialAffordance(errForwardedSocketDial); ok {
		t.Fatal("a local run must keep rendering its own dial error")
	}
}

// normalizedRender flattens a rendered view to plain, single-spaced text.
// lipgloss wraps to the model width and colours every line, so an assertion on
// a phrase that happens to straddle a wrap point would fail for a screen that
// says exactly the right thing — the same normalization a proof scenario's
// "normalized" match applies.
func normalizedRender(rendered string) string {
	return strings.Join(strings.Fields(stripANSI(rendered)), " ")
}

// assertNoForwardedSocketDial is the shared absence guard for the three views:
// none of them may show the raw dial error, the forwarded socket path, or the
// temp directory it lives in.
func assertNoForwardedSocketDial(t *testing.T, what, rendered string) {
	t.Helper()
	for _, forbidden := range []string{"dial unix", "bossd.sock", "boss-host-"} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("%s %q must never show %q", what, rendered, forbidden)
		}
	}
}
