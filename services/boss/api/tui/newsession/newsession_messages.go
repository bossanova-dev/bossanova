package newsession

import (
	"github.com/recurser/boss/internal/domain/rpc"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// sessionType identifies the kind of session to create.
type sessionType int

const (
	sessionTypeQuickChat    sessionType = iota // Quick chat in base folder
	sessionTypeNewPR                           // Create a new PR
	sessionTypeExistingPR                      // Work on an existing PR
	sessionTypeExecutePlan                     // Execute a plan (placeholder)
	sessionTypeLinearTicket                    // Work on a Linear issue
	sessionTypeSentryIssue                     // Fix a Sentry issue
)

// newSessionPhase tracks the current phase of the wizard.
type newSessionPhase int

const (
	newSessionPhaseLoading     newSessionPhase = iota // Fetching repos
	newSessionPhaseRepoSelect                         // Table-based repo picker
	newSessionPhaseAgentSelect                        // Table-based agent picker (multi-agent installs)
	newSessionPhaseTypeSelect                         // Table-based session type picker
	newSessionPhasePRSelect                           // Table-based PR picker
	newSessionPhaseIssueSelect                        // Table-based issue picker (Linear)
	newSessionPhaseForm                               // Main huh form active
	newSessionPhaseCreating                           // Waiting for CreateSession RPC
	newSessionPhaseDone                               // Terminal
)

// ReposMsg carries the result of a ListRepos RPC call.
type ReposMsg struct {
	Repos []*pb.Repo
	Err   error
}

// agentsMsg carries the result of a ListAgents RPC call. Agents drive the
// per-session agent-select phase shown when more than one agent runner is
// loaded and the user did not pre-pick one via `--agent`.
type agentsMsg struct {
	agents []rpc.AgentInfo
	err    error
}

// PrsMsg carries the result of a ListRepoPRs RPC call.
type PrsMsg struct {
	PRs []*pb.PRSummary
	Err error
}

// IssuesMsg carries the result of a ListTrackerIssues RPC call. seq is the
// monotonic sequence number in effect when the fetch was issued; the handler
// drops the response when it no longer matches m.issueSearchSeq (meaning the
// user typed further or navigated away). query is still used to distinguish
// the initial unfiltered load from an empty search result.
type IssuesMsg struct {
	Issues []*pb.TrackerIssue
	Err    error
	Seq    uint64
	Query  string
}

// searchIssuesTickMsg fires after the debounce window elapses. The seq field
// is a monotonic counter incremented on every keystroke that changes the
// query — when the tick fires we ignore it unless seq is still the latest, so
// a burst of keystrokes only triggers one search at the end.
type searchIssuesTickMsg struct {
	seq   uint64
	query string
}

type streamIdentity struct{ _ byte }

// createSessionStreamMsg carries the opened stream or error.
type createSessionStreamMsg struct {
	streamID *streamIdentity
	stream   rpc.CreateSessionStream
	err      error
}

// setupScriptLineMsg carries a single line of setup script output.
type setupScriptLineMsg struct {
	streamID *streamIdentity
	text     string
}

// streamSessionAcceptedMsg carries the accepted (non-terminal) session from the
// stream — the row exists and is addressable, but its bootstrap is still
// running (BOS-720). The stream stays open; the settled session arrives later as
// streamSessionCreatedMsg.
type streamSessionAcceptedMsg struct {
	streamID *streamIdentity
	session  *pb.Session
}

// streamSessionCreatedMsg carries the final session from the stream.
type streamSessionCreatedMsg struct {
	streamID *streamIdentity
	session  *pb.Session
}

// streamErrorMsg carries an error from the stream.
type streamErrorMsg struct {
	streamID *streamIdentity
	err      error
}

// sessionTypeOption defines a row in the session-type selection table.
type sessionTypeOption struct {
	label string
	desc  string
	typ   sessionType
}

// formData holds huh form-bound values on the heap so that Value() pointers
// remain valid across bubbletea value-receiver copies of Model.
type formData struct {
	title string
}
