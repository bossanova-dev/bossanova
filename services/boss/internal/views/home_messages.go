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
	err            error
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
	loggedIn bool
	email    string
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

// tickMsg signals a polling refresh.
type tickMsg struct{}

func tickCmd() tea.Cmd {
	return tea.Tick(pollInterval, func(time.Time) tea.Msg {
		return tickMsg{}
	})
}
