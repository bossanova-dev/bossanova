package views

// Message types and the confirm-mode enum for the chat picker. Split out of
// chatpicker.go (BOS-527); the declarations are unchanged.

import (
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// chatPickerSessionMsg carries a session fetched via RPC for the chat picker.
type chatPickerSessionMsg struct {
	session *pb.Session
}

// chatPickerErrMsg signals a fetch error in the chat picker.
type chatPickerErrMsg struct {
	err error
}

// chatsListedMsg carries the result of listing chats via RPC,
// along with daemon-side heartbeat statuses for cross-instance display.
type chatsListedMsg struct {
	chats            []*pb.ClaudeChat
	daemonStatuses   map[string]string    // agent_session_id → status string
	daemonLastOutput map[string]time.Time // agent_session_id → last PTY output time
	// daemonWaitingReasons maps agent_session_id → why the chat is parked on an
	// external event (BOS-668), e.g. "awaiting checks_passed_ready on
	// acme/widget#123". Only waiting chats appear.
	daemonWaitingReasons map[string]string
	err                  error
}

// chatTitlesBackfilledMsg carries updated titles for chats that were "New chat".
type chatTitlesBackfilledMsg struct {
	updates map[string]string // agent_session_id -> title
}

// chatPickerRefreshMsg carries refreshed session + daemon statuses for polling.
type chatPickerRefreshMsg struct {
	session              *pb.Session
	daemonStatuses       map[string]string
	daemonLastOutput     map[string]time.Time
	daemonWaitingReasons map[string]string
}

// chatDeletedMsg signals that a chat was deleted (or failed to delete).
type chatDeletedMsg struct {
	agentSessionID string
	err            error
}

// newTabResultMsg carries the result of an async openInNewTab call.
type newTabResultMsg struct {
	err error
}

type repoWebLink struct {
	provider string
	url      string
}

type repoWebLinkMsg struct {
	link repoWebLink
	// repoID and prNumber tag the session state this link was computed for, so
	// the handler can discard a slow in-flight fetch (e.g. one started before a
	// PR existed) that resolves after a newer fetch already installed the right
	// link.
	repoID   string
	prNumber int
}

type webOpenResultMsg struct {
	err error
}

// mergeResultMsg carries the result of an async MergeSession RPC call.
// sessionID tags the session this merge was issued for so a completion that
// resolves after the user navigated to a different session is discarded rather
// than acted on.
type mergeResultMsg struct {
	sessionID string
	err       error
}

// archiveResultMsg carries the result of an async ArchiveSession RPC call.
// sessionID tags the session this archive was issued for so a completion that
// resolves after the user navigated to a different session is discarded rather
// than acted on.
type archiveResultMsg struct {
	sessionID string
	err       error
}

// confirmKind identifies which destructive action a y/n confirmation prompt
// is gating. Stored as a plain enum on the model (NOT a pointer/closure) so it
// is safe under Bubble Tea's value-copy of models between updates.
type confirmKind int

const (
	confirmNone confirmKind = iota
	confirmDelete
	confirmMerge
	confirmArchive
	confirmSwitch // switch-account against a mid-turn (WORKING) chat
)

// wakeResultMsg carries the result of an async WakeChat RPC call.
type wakeResultMsg struct {
	agentSessionID string
	resp           *pb.WakeChatResponse
	err            error
}

// switchAccountsLoadedMsg carries the accounts fetched for the switch-account
// picker (BOS-171), scoped to the target chat's provider (agent name). A load
// error is non-fatal: the picker still opens with just the unmanaged row so the
// operator can at least switch to account 0.
type switchAccountsLoadedMsg struct {
	accounts []*pb.Account
	err      error
}

// switchAccountResultMsg carries the result of an async SwitchSessionAccount
// RPC call (BOS-171: stop → swap → resume under the chosen account).
type switchAccountResultMsg struct {
	resp *pb.SwitchSessionAccountResponse
	err  error
}
