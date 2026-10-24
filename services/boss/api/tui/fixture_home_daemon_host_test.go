package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/recurser/boss/api/tui/repos"

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
func repoListErrorView(err error, w int) string {
	m := repos.NewRepoListModel(nil, context.Background())
	m.SetSize(w, 0)
	v, _ := m.Update(repos.RepoListLoadedMsg{Err: err})
	return v.View().Content
}
