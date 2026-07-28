package views

// The new-session wizard's two enums — sessionType and newSessionPhase — the
// *Msg types its Update arms match on, and the plain data structs it carries
// (sessionTypeOption, formData). The *Msg types are wizard-local with one
// exception: agentsMsg is package-wide — ChatPickerModel.Update matches on it
// too (chatpicker.go, chatpicker_handlers.go). The framework messages Update also
// handles — tea.WindowSizeMsg, tea.PasteMsg, tea.KeyMsg, spinner.TickMsg — are
// declared upstream, not here. Split out of newsession.go (BOS-528); the
// declarations are unchanged.

import (
	"github.com/recurser/boss/internal/client"
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

// reposMsg carries the result of a ListRepos RPC call.
type reposMsg struct {
	repos []*pb.Repo
	err   error
}

// agentsMsg carries the result of a ListAgents RPC call. Agents drive the
// per-session agent-select phase shown when more than one agent runner is
// loaded and the user did not pre-pick one via `--agent`.
type agentsMsg struct {
	agents []client.AgentInfo
	err    error
}

// prsMsg carries the result of a ListRepoPRs RPC call.
type prsMsg struct {
	prs []*pb.PRSummary
	err error
}

// issuesMsg carries the result of a ListTrackerIssues RPC call. seq is the
// monotonic sequence number in effect when the fetch was issued; the handler
// drops the response when it no longer matches m.issueSearchSeq (meaning the
// user typed further or navigated away). query is still used to distinguish
// the initial unfiltered load from an empty search result.
type issuesMsg struct {
	issues []*pb.TrackerIssue
	err    error
	seq    uint64
	query  string
}

// searchIssuesTickMsg fires after the debounce window elapses. The seq field
// is a monotonic counter incremented on every keystroke that changes the
// query — when the tick fires we ignore it unless seq is still the latest, so
// a burst of keystrokes only triggers one search at the end.
type searchIssuesTickMsg struct {
	seq   uint64
	query string
}

// createSessionStreamMsg carries the opened stream or error.
type createSessionStreamMsg struct {
	stream client.CreateSessionStream
	err    error
}

// setupScriptLineMsg carries a single line of setup script output.
type setupScriptLineMsg struct {
	text string
}

// streamSessionCreatedMsg carries the final session from the stream.
type streamSessionCreatedMsg struct {
	session *pb.Session
}

// streamErrorMsg carries an error from the stream.
type streamErrorMsg struct {
	err error
}

// sessionTypeOption defines a row in the session-type selection table.
type sessionTypeOption struct {
	label string
	desc  string
	typ   sessionType
}

// formData holds huh form-bound values on the heap so that Value() pointers
// remain valid across bubbletea value-receiver copies of NewSessionModel.
type formData struct {
	title string
}
