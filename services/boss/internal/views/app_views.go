package views

import (
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// View identifies which screen is currently active.
type View int

const (
	ViewHome View = iota
	ViewNewSession
	ViewAttach
	ViewChatPicker
	ViewRepoAdd
	ViewRepoList
	ViewRepoSettings
	ViewTrash
	ViewSettings
	ViewSessionSettings
	ViewLogin
	ViewBugReport
	ViewCron
	ViewCronForm
	ViewOnboarding
	// ViewAgentAccounts is appended at the END of the const block so existing iota
	// values do not shift (BOS-265).
	ViewAgentAccounts
	// ViewAgentAccountEdit is the per-account inline-edit form (label/status/priority)
	// reached from the accounts list (BOS-266). Appended after ViewAgentAccounts to
	// preserve existing iota values.
	ViewAgentAccountEdit
	// ViewAgentAccountRegister is the native add-account flow for Claude and Codex
	// (BOS-267), launched by [a] from the accounts list. Appended at the END of
	// the const block so existing iota values do not shift.
	ViewAgentAccountRegister
	// ViewGeneralSettings is the general (global) settings list reached from the
	// Settings hub (BOS-511). Appended at the END of the const block so existing
	// iota values do not shift.
	ViewGeneralSettings
)

// switchViewMsg requests the app to switch to a different view.
type switchViewMsg struct {
	view       View
	sessionID  string           // used for ViewAttach and ViewChatPicker
	resumeID   string           // Claude Code session UUID to resume (ViewAttach only)
	chatID     string           // agent session ID of the chat to select once the list loads (ViewChatPicker only)
	agentName  string           // optional per-chat agent override (ViewAttach only); empty = inherit session
	armedNote  string           // what a waiting chat is parked on, shown while launching (ViewAttach only)
	returnView View             // optional view to return to when the target view is cancelled
	firstRepo  bool             // true when add-repo was opened from the zero-repo home empty state (return home on cancel)
	account    *pb.AgentAccount // selected account carrier for ViewAgentAccountEdit (BOS-266)
	// reauthAccountID switches ViewAgentAccountRegister into in-place
	// reauthentication for that account instead of adding a new one
	// (BOS-1142): the provider chooser is skipped and the flow replaces the
	// existing row's credential.
	reauthAccountID string
}

type repoAddCompletedMsg struct {
	repos       []*pb.Repo
	err         error
	highlightID string
}
