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
	// ViewAccounts is appended at the END of the const block so existing iota
	// values do not shift (BOS-265).
	ViewAccounts
	// ViewAccountEdit is the per-account inline-edit form (label/status/priority)
	// reached from the accounts list (BOS-266). Appended after ViewAccounts to
	// preserve existing iota values.
	ViewAccountEdit
	// ViewAccountRegister is the native add-account flow for Claude and Codex
	// (BOS-267), launched by [a] from the accounts list. Appended at the END of
	// the const block so existing iota values do not shift.
	ViewAccountRegister
	// ViewGeneralSettings is the general (global) settings list reached from the
	// Settings hub (BOS-511). Appended at the END of the const block so existing
	// iota values do not shift.
	ViewGeneralSettings
)

// switchViewMsg requests the app to switch to a different view.
type switchViewMsg struct {
	view       View
	sessionID  string      // used for ViewAttach and ViewChatPicker
	resumeID   string      // Claude Code session UUID to resume (ViewAttach only)
	agentName  string      // optional per-chat agent override (ViewAttach only); empty = inherit session
	returnView View        // optional view to return to when the target view is cancelled
	firstRepo  bool        // true when add-repo was opened from the zero-repo home empty state (return home on cancel)
	account    *pb.Account // selected account carrier for ViewAccountEdit (BOS-266)
}

type repoAddCompletedMsg struct {
	repos       []*pb.Repo
	err         error
	highlightID string
}
