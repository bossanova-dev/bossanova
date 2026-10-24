// Home's message types and the poll tick that drives them. Split out of
// home.go (BOS-526); the declarations are unchanged.

package home

import (
	"time"

	tea "charm.land/bubbletea/v2"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// sessionListMsg carries the result of a ListSessions RPC call,
// along with daemon-side heartbeat statuses for cross-instance display.
type SessionListMsg struct {
	HomeGeneration    uint64
	PollID            uint64
	Sessions          []*pb.Session
	HeartbeatStatuses map[string]string // session_id → status string
	// daemonWaitingReasons carries session_id → why the session is parked on an
	// external event (BOS-668), e.g. "awaiting checks_passed_ready on
	// acme/widget#123". Only populated for sessions whose aggregate status is
	// waiting; nil on an older daemon that does not stamp the field.
	DaemonWaitingReasons map[string]string
	// daemonPhases carries the skill-reported working phase from status polling.
	DaemonPhases map[string]string
	// sessionReadFailures carries the organizations whose sessions could not be
	// read on this poll (BOS-1151). A cloud read fans out across every
	// organization the caller belongs to, so one organization failing yields a
	// partial list rather than an error: sessions is what was served, and this
	// says what is missing. Always empty against a local daemon.
	SessionReadFailures []*pb.OrganizationSessionReadFailure
	Err                 error
}

// homeGenerationSequence gives each Model a process-unique identity. It
// prevents a command started by a replaced Model from mutating its
// successor when both happen to issue the same poll ID.
var homeGenerationSequence uint64

// repoCountMsg carries the number of registered repos.
type RepoCountMsg struct {
	Count int
	Err   error
}

// authStatusMsg carries the result of checking auth status.
type AuthStatusMsg struct {
	Generation uint64
	LoggedIn   bool
	Email      string
	// needsRelogin reports stored-but-unusable credentials (BOS-659): the
	// email above is the retained identity, but the daemon can no longer
	// refresh, so Home offers [l]ogin and explains why. Distinct from
	// never having logged in, where both fields below are zero.
	NeedsRelogin bool
	// reloginReason is one of auth.ReloginReason*, rendered through
	// auth.ReloginReasonDescription. Never carries token material.
	ReloginReason string
}

type cloudAccessMsg struct {
	Status *pb.CloudAccessStatus
	Err    error
}

type homeCloudCheckoutMsg struct {
	URL string
	Err error
}

type StartSubscriptionFlowMsg struct{}

type upgradeCheckMsg struct {
	Current   string
	Latest    string
	URL       string
	Available bool
	Err       error
}

type upgradeRunMsg struct {
	Output string
	Err    error
}

type daemonRestartMsg struct {
	Err error
}

// logoutMsg carries the result of the confirmed logout action. Logout mutates
// the shared credential record behind a cross-process lock that bossd can be
// holding, so it genuinely fails; before BOS-659 the TUI discarded the error
// and the board simply stayed signed in with no explanation.
type LogoutMsg struct {
	Err    error
	notify tea.Cmd
}

// sessionRenamedMsg carries the result of the UpdateSession RPC issued when a
// rename is committed (BOS-837). session is the daemon's post-write record, so
// the board adopts the title the daemon actually stored rather than the one the
// TUI hoped for; it is nil when err is set, and the row keeps its old title.
type SessionRenamedMsg struct {
	Session *pb.Session
	Err     error
}

// sessionMovedMsg carries the result of the MoveSession RPC issued by the
// hidden alt+up / alt+down reorder chords (BOS-1231). The daemon's refreshed
// session is deliberately NOT carried: its only new information is list_rank,
// which no column renders, and adopting the whole record would discard the
// poll-derived display state the board has layered on — the same reason
// handleSessionRenamed patches one field rather than swapping the row.
//
// moved mirrors MoveSessionResponse.is_moved: false is a SUCCESSFUL no-op (the
// session was already at the boundary), never an error.
type sessionMovedMsg struct {
	SessionID string
	Moved     bool
	Err       error
}

// tickMsg signals a polling refresh.
type TickMsg struct{}

func TickCmd() tea.Cmd {
	return tea.Tick(pollInterval, func(time.Time) tea.Msg {
		return TickMsg{}
	})
}
