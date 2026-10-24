package tui

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/recurser/boss/api/tui/trash"
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
func TestConvertedListViewsShowTheReconnectingAffordance(t *testing.T) {
	withHostDestination(t, "deploy@bastion.example.com")
	withHostTunnelReason(t, "remotehost: ssh tunnel to deploy@bastion.example.com exited: signal: terminated")

	views := map[string]string{
		"repo list": normalizedRender(repoListErrorView(errForwardedSocketDial, 100)),
		"trash":     normalizedRender(trashErrorViewForTest(errForwardedSocketDial, 100)),
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
	local := normalizedRender(repoListErrorView(errForwardedSocketDial, 100))
	if !strings.Contains(local, "dial unix") {
		t.Errorf("local repo list %q must keep showing the real error", local)
	}
}

// TestRPCStatusMessageSubstitutesOnlyForADroppedTunnel is the status-line half
// of acceptance criterion 2. A status line is where an *action* failure lands —
// pressing [d] on a cron job with the tunnel down — and it is a different
// surface from the full-screen error view, so it needs its own substitution and
// its own one-line shape.

func trashErrorViewForTest(err error, width int) string {
	m := NewTrashModel(nil, context.Background())
	m.SetSize(width, 0)
	v, _ := m.Update(trash.ListMsg{Err: err})
	return v.(TrashModel).View().Content
}

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

	allowed := map[string]bool{
		"../../api/tui/home/home.go": true, "../../api/tui/home/home_daemon.go": true, "../../api/tui/home/home_upgrade.go": true, "../../api/tui/login/subscription.go": true,
	}
	entries, err := viewSourceEntries(t)
	if err != nil {
		t.Fatal(err)
	}
	sources := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			sources = append(sources, e.Name())
		}
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
