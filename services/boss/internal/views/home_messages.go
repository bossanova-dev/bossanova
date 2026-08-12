// Home's message types and the poll tick that drives them. Split out of
// home.go (BOS-526); the declarations are unchanged.

package views

import (
	"time"

	tea "charm.land/bubbletea/v2"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// sessionListMsg carries the result of a ListSessions RPC call,
// along with daemon-side heartbeat statuses for cross-instance display.
type sessionListMsg struct {
	homeGeneration uint64
	pollID         uint64
	sessions       []*pb.Session
	daemonStatuses map[string]string // session_id → status string
	// daemonWaitingReasons carries session_id → why the session is parked on an
	// external event (BOS-668), e.g. "awaiting checks_passed_ready on
	// acme/widget#123". Only populated for sessions whose aggregate status is
	// waiting; nil on an older daemon that does not stamp the field.
	daemonWaitingReasons map[string]string
	err                  error
}

// homeGenerationSequence gives each HomeModel a process-unique identity. It
// prevents a command started by a replaced HomeModel from mutating its
// successor when both happen to issue the same poll ID.
var homeGenerationSequence uint64

// repoCountMsg carries the number of registered repos.
type repoCountMsg struct {
	count int
	err   error
}

// authStatusMsg carries the result of checking auth status.
type authStatusMsg struct {
	generation uint64
	loggedIn   bool
	email      string
	// needsRelogin reports stored-but-unusable credentials (BOS-659): the
	// email above is the retained identity, but the daemon can no longer
	// refresh, so Home offers [l]ogin and explains why. Distinct from
	// never having logged in, where both fields below are zero.
	needsRelogin bool
	// reloginReason is one of auth.ReloginReason*, rendered through
	// auth.ReloginReasonDescription. Never carries token material.
	reloginReason string
}

type cloudAccessMsg struct {
	status *pb.CloudAccessStatus
	err    error
}

type homeCloudCheckoutMsg struct {
	url string
	err error
}

type startSubscriptionFlowMsg struct{}

type upgradeCheckMsg struct {
	current   string
	latest    string
	url       string
	available bool
	err       error
}

type upgradeRunMsg struct {
	output string
	err    error
}

type daemonRestartMsg struct {
	err error
}

// logoutMsg carries the result of the confirmed logout action. Logout mutates
// the shared credential record behind a cross-process lock that bossd can be
// holding, so it genuinely fails; before BOS-659 the TUI discarded the error
// and the board simply stayed signed in with no explanation.
type logoutMsg struct {
	err    error
	notify tea.Cmd
}

// sessionRenamedMsg carries the result of the UpdateSession RPC issued when a
// rename is committed (BOS-837). session is the daemon's post-write record, so
// the board adopts the title the daemon actually stored rather than the one the
// TUI hoped for; it is nil when err is set, and the row keeps its old title.
type sessionRenamedMsg struct {
	session *pb.Session
	err     error
}

// tickMsg signals a polling refresh.
type tickMsg struct{}

func tickCmd() tea.Cmd {
	return tea.Tick(pollInterval, func(time.Time) tea.Msg {
		return tickMsg{}
	})
}
