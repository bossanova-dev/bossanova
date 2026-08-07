package views

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryViewBuildsItsErrorMessageThroughTheHostHelpers is the enumeration
// guard acceptance criterion 2 needs. The criterion is worded for *any* --host
// view, and this package has two distinct error surfaces, both of which show
// daemon RPC failures:
//
//   - full-screen error views ("Error: <err>"), fifteen of them, which must use
//     rpcErrorMessage;
//   - transient status lines an action failure writes ("Delete failed: <err>"),
//     fifteen of them, carrying an error delivered on a tea.Msg, which must use
//     rpcStatusMessage / rpcStatusDetail.
//
// Converting the three the ticket named by hand left the other twelve leaking
// `dial unix …`; converting all fifteen still left every status line leaking it
// on a different surface; and forbidding one status-line spelling still let two
// sites written as `"prefix: " + msg.err.Error()` through, in a file where the
// lines above and below them had both been converted. Each round's guard
// certified coverage it did not have. Nothing but a reviewer re-reading thirty
// call sites would have caught any of them, or would catch the thirty-first — so
// the rule is structural rather than a matter of diligence.
//
// Deliberately NOT covered: the views that format their own m.err for a failure
// that never crosses the forwarded socket (bugreport.go's cloud submit,
// login.go's auth). Substituting a tunnel affordance there would claim an outage
// that is not happening.
func TestEveryViewBuildsItsErrorMessageThroughTheHostHelpers(t *testing.T) {
	// hostcontext.go owns all three helpers, so it is the one legitimate site.
	const owner = "hostcontext.go"
	forbidden := map[string]string{
		`fmt.Sprintf("Error: %v"`: "renders a full-screen error itself; use rpcErrorMessage(err)",
		`%v", msg.err`:            "formats a message-delivered error itself; use rpcStatusMessage(prefix, err) or rpcStatusDetail(err)",
		`.err.Error()`:            "stringifies a message-delivered error itself; use rpcStatusMessage(prefix, err) or rpcStatusDetail(err)",
	}
	// Files whose displayed errors provably never cross the forwarded socket, so
	// substituting a tunnel affordance in them would claim an outage that is not
	// happening. Each was checked, not assumed:
	//   home.go          — logout, which goes to cloud auth, not bossd
	//   home_daemon.go   — restarting the *local* daemon (a --host run is not
	//                      talking to it, and its own dial error is local)
	//   home_upgrade.go  — upgrading the local binary
	//   subscription.go  — cloud billing
	// A new daemon-RPC error surface in one of these still has to be routed; the
	// allowlist is per file, so it is visible here rather than silent.
	allowed := map[string]bool{
		"home.go": true, "home_daemon.go": true, "home_upgrade.go": true, "subscription.go": true,
	}
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob view sources: %v", err)
	}
	if len(sources) == 0 {
		t.Fatal("no view sources found; the guard would pass vacuously")
	}
	checked := 0
	for _, source := range sources {
		if strings.HasSuffix(source, "_test.go") || source == owner {
			continue
		}
		body, err := os.ReadFile(source)
		if err != nil {
			t.Fatalf("read %s: %v", source, err)
		}
		checked++
		for pattern, complaint := range forbidden {
			if pattern == `.err.Error()` && allowed[source] {
				continue
			}
			if strings.Contains(string(body), pattern) {
				t.Errorf("%s %s, so a dropped --host tunnel renders the forwarded socket's dial error instead of the reconnecting affordance", source, complaint)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no non-test view sources were checked; the guard would pass vacuously")
	}
}

// withHostControlPath sets the tunnel's multiplexing socket for one test and
// restores it afterwards. It is package-level state, so a test whose subject
// depends on its value must set it rather than inherit whatever ran before —
// remoteTerminfoProber reads it at probe time to ride the tunnel's ssh master.
func withHostControlPath(t *testing.T, controlPath string) {
	t.Helper()
	previous := hostControlPath
	t.Cleanup(func() { SetHostControlPath(previous) })
	SetHostControlPath(controlPath)
}

// withHostDestination points the daemon-down screen at a --host destination for
// one test and restores the local wording afterwards, so an ordinary local run
// in a sibling test never inherits ssh remediation.
func withHostDestination(t *testing.T, destination string) {
	t.Helper()
	previous := hostDestination
	t.Cleanup(func() { SetHostDestination(previous) })
	SetHostDestination(destination)
}

// TestDaemonDownRemediationNamesTheHost is the mid-session half of the --host
// reconnect criterion: a tunnel that drops after startup never reaches the
// startup wait screen, so this daemon-down view is what the user actually sees.
// It must say the connection is being retried and point at the machine that
// stopped answering — telling a --host user to run `boss daemon restart` would
// aim them at the local daemon boss is not talking to.
func TestDaemonDownRemediationNamesTheHost(t *testing.T) {
	withHostDestination(t, "deploy@bastion.example.com")

	got := daemonDownRemediation()
	for _, want := range []string{
		"Reconnecting to deploy@bastion.example.com",
		"resumes automatically",
		"ssh deploy@bastion.example.com",
		"boss daemon status",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("remediation %q must contain %q", got, want)
		}
	}
	// The local advice restarts, installs, and uninstalls the wrong machine's
	// daemon, so none of those commands may be offered unqualified here.
	for _, forbidden := range []string{"boss daemon restart", "boss daemon install", "\nbossd "} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("remediation %q must not offer the local %q", got, forbidden)
		}
	}
}

// TestDaemonDownRemediationDefaultsToLocal pins that the ordinary local daemon
// wording is unchanged when --host was never used, and that the destination
// does not leak across runs.
func TestDaemonDownRemediationDefaultsToLocal(t *testing.T) {
	withHostDestination(t, "")

	got := daemonDownRemediation()
	for _, want := range []string{"boss daemon restart", "boss daemon status", "boss daemon install"} {
		if !strings.Contains(got, want) {
			t.Fatalf("local remediation %q must contain %q", got, want)
		}
	}
	if strings.Contains(got, "ssh ") {
		t.Fatalf("local remediation %q must not mention ssh", got)
	}
}

// TestRenderDaemonErrorUsesTheHostRemediation drives the rendered view, not the
// helper alone: renderDaemonError falls back to daemonDownRemediation when the
// model carries none, so a --host drop caught before the poll threshold must
// still render the host wording.
func TestRenderDaemonErrorUsesTheHostRemediation(t *testing.T) {
	withHostDestination(t, "workstation")

	h := HomeModel{err: errTestDaemonDown, width: 100}
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
	previous := hostTunnelReason
	t.Cleanup(func() { SetHostTunnelReason(previous) })
	if reason == "" {
		SetHostTunnelReason(nil)
		return
	}
	SetHostTunnelReason(func() string { return reason })
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

	rendered := normalizedRender(HomeModel{err: errForwardedSocketDial, width: 100}.renderDaemonError())
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
func TestConvertedListViewsShowTheReconnectingAffordance(t *testing.T) {
	withHostDestination(t, "deploy@bastion.example.com")
	withHostTunnelReason(t, "remotehost: ssh tunnel to deploy@bastion.example.com exited: signal: terminated")

	views := map[string]string{
		"repo list": normalizedRender(RepoListModel{err: errForwardedSocketDial, width: 100}.View().Content),
		"trash":     normalizedRender(TrashModel{err: errForwardedSocketDial, width: 100}.View().Content),
	}
	for name, rendered := range views {
		assertNoForwardedSocketDial(t, name, rendered)
		if !strings.Contains(rendered, "Reconnecting to deploy@bastion.example.com") {
			t.Errorf("%s %q must show the reconnecting affordance naming the destination", name, rendered)
		}
		if !strings.Contains(rendered, "signal: terminated") {
			t.Errorf("%s %q must report why the tunnel went down", name, rendered)
		}
		if !strings.Contains(rendered, "[esc] back") {
			t.Errorf("%s %q must keep its action bar", name, rendered)
		}
	}

	// …and a local run is untouched, which is acceptance criterion 6.
	withHostDestination(t, "")
	local := normalizedRender(RepoListModel{err: errForwardedSocketDial, width: 100}.View().Content)
	if !strings.Contains(local, "dial unix") {
		t.Errorf("local repo list %q must keep showing the real error", local)
	}
}

// TestRPCStatusMessageSubstitutesOnlyForADroppedTunnel is the status-line half
// of acceptance criterion 2. A status line is where an *action* failure lands —
// pressing [d] on a cron job with the tunnel down — and it is a different
// surface from the full-screen error view, so it needs its own substitution and
// its own one-line shape.
func TestRPCStatusMessageSubstitutesOnlyForADroppedTunnel(t *testing.T) {
	withHostDestination(t, "deploy@bastion.example.com")
	withHostTunnelReason(t, "remotehost: ssh tunnel to deploy@bastion.example.com exited: signal: terminated")

	got := rpcStatusMessage("Delete failed", errForwardedSocketDial)
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

	// An answer from the remote daemon is the most useful thing on screen and
	// must survive verbatim, or --host would swallow every real failure.
	realFailure := errors.New("not_found: cron job cron-1 does not exist")
	if got := rpcStatusMessage("Delete failed", realFailure); got != "Delete failed: not_found: cron job cron-1 does not exist" {
		t.Errorf("status = %q, want the daemon's own answer", got)
	}

	// …and a local run is untouched, which is acceptance criterion 6.
	withHostDestination(t, "")
	if got := rpcStatusMessage("Delete failed", errForwardedSocketDial); !strings.Contains(got, "dial unix") {
		t.Errorf("local status %q must keep showing the real error", got)
	}
}

// withHostTunnelHealth registers a fixed supervisor liveness answer for one test
// and restores the previous reader afterwards.
func withHostSupervisorRunning(t *testing.T, healthy bool, registered bool) {
	t.Helper()
	previous := hostSupervisorRunning
	t.Cleanup(func() { SetHostSupervisorRunning(previous) })
	if !registered {
		SetHostSupervisorRunning(nil)
		return
	}
	SetHostSupervisorRunning(func() bool { return healthy })
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

	stopped := hostDownRemediation("deploy@bastion.example.com")
	if strings.Contains(stopped, "keeps redialling") {
		t.Errorf("remediation %q promises a redial after the supervisor exited", stopped)
	}
	if !strings.Contains(stopped, "Restart boss") {
		t.Errorf("remediation %q must tell the user the one thing that recovers it", stopped)
	}
	if !strings.Contains(stopped, "signal: terminated") {
		t.Errorf("remediation %q must still report why the tunnel went down", stopped)
	}

	// A live supervisor keeps the reconnecting wording — and so does an
	// unregistered reader, because claiming "nobody is redialling" without
	// evidence is worse than staying quiet: it would send a user to restart boss
	// mid-backoff, moments before it recovered on its own.
	withHostSupervisorRunning(t, true, true)
	if live := hostDownRemediation("deploy@bastion.example.com"); !strings.Contains(live, "keeps redialling") {
		t.Errorf("remediation %q must promise the redial that is actually happening", live)
	}
	withHostSupervisorRunning(t, false, false)
	if unset := hostDownRemediation("deploy@bastion.example.com"); !strings.Contains(unset, "keeps redialling") {
		t.Errorf("remediation %q must not claim the supervisor died with no reader registered", unset)
	}
}

// TestRenderDaemonErrorShowsTheRetainedTunnelReason is acceptance criterion 3:
// the classified failure recordFailure already produced must be readable from a
// running TUI, with no debugger and no source access.
func TestRenderDaemonErrorShowsTheRetainedTunnelReason(t *testing.T) {
	withHostDestination(t, "deploy@bastion.example.com")
	withHostTunnelReason(t, "remotehost: ssh tunnel to deploy@bastion.example.com exited: signal: terminated")

	rendered := normalizedRender(HomeModel{err: errForwardedSocketDial, width: 100}.renderDaemonError())
	if !strings.Contains(rendered, "signal: terminated") {
		t.Fatalf("home daemon error %q must report why the tunnel went down", rendered)
	}
	// …and where the whole history lives, since the screen only shows the last
	// one. The source has to be named: `boss tail` alone defaults to the bossd
	// log (cmd/tail.go resolveSources), which under --host is a local daemon
	// boss is not talking to and never carries the supervisor's line. Pinning
	// the bare prefix would pass against exactly that mistake.
	if !strings.Contains(rendered, "boss tail boss") {
		t.Fatalf("home daemon error %q must point at the boss log, not the bossd default", rendered)
	}
}

// TestRenderDaemonErrorKeepsTheLocalErrorVisible is acceptance criterion 6 for
// this view: with no --host destination the raw error is still the most useful
// thing on screen and must not be swallowed.
func TestRenderDaemonErrorKeepsTheLocalErrorVisible(t *testing.T) {
	withHostDestination(t, "")
	withHostTunnelReason(t, "")

	rendered := normalizedRender(HomeModel{err: errForwardedSocketDial, width: 100}.renderDaemonError())
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

	if _, ok := hostDialAffordance(errors.New("not_found: session sess-1 does not exist")); ok {
		t.Fatal("an ordinary RPC error was replaced by the reconnecting affordance")
	}
	if _, ok := hostDialAffordance(nil); ok {
		t.Fatal("a nil error produced a reconnecting affordance")
	}
	if _, ok := hostDialAffordance(errForwardedSocketDial); !ok {
		t.Fatal("a forwarded-socket dial failure must yield the reconnecting affordance")
	}

	withHostDestination(t, "")
	if _, ok := hostDialAffordance(errForwardedSocketDial); ok {
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
