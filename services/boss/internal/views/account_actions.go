package views

import (
	"context"
	"fmt"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// account_actions.go holds the shared disable/enable/remove lifecycle helpers
// (BOS-268) used by BOTH the accounts list (accounts_list.go) and the account
// edit form (account_edit.go), so the two surfaces cannot drift in behavior or
// prompt copy.
//
// Keymap reconciliation: the list binds `[space]` = toggle disable/enable and
// `d` = remove. BOS-268 reassigned `d` from BOS-265's disable-stub to remove and
// introduced the toggle; BOS-392 rebound the toggle from `x` to `[space]` so it
// matches the cron list's `[space] toggle`.
//
// Account status vocabulary mirrors lib/bossalib/models/account.go
// (AccountStatusActive / AccountStatusDisabled), which this package cannot
// import across the module boundary; the daemon validates the same two values
// in services/bossd/internal/server/account.go (validAccountStatus).
const (
	accountStatusActive   = "active"
	accountStatusDisabled = "disabled"
)

// --- Shared lifecycle messages ---

// accountStatusUpdatedMsg carries the result of an UpdateAccount status flip
// (disable ⇄ enable) issued from either the list or the edit view.
type accountStatusUpdatedMsg struct {
	id     string
	status string
	err    error
}

// accountRemovedMsg carries the result of a RemoveAccount call. Removal purges
// the keyring credential and is irreversible.
type accountRemovedMsg struct {
	id  string
	err error
}

// sessionsLoadedMsg carries the result of the ListSessions call batched in the
// accounts-list Init, used to compute the bound-session warning shown before a
// disable/remove. Best-effort/advisory: on error the count is reported unknown
// and prompt copy degrades to a generic warning rather than claiming zero.
type sessionsLoadedMsg struct {
	sessions []*pb.Session
	err      error
}

// --- Shared command builders ---

// updateAccountStatusCmd issues UpdateAccount with only Id + Status set
// (present-only field semantics; label/priority/allowed_models untouched),
// returning an accountStatusUpdatedMsg. status must be accountStatusActive or
// accountStatusDisabled.
func updateAccountStatusCmd(c client.BossClient, ctx context.Context, id, status string) tea.Cmd {
	return func() tea.Msg {
		st := status
		_, err := c.UpdateAccount(ctx, &pb.UpdateAccountRequest{Id: id, Status: &st})
		return accountStatusUpdatedMsg{id: id, status: status, err: err}
	}
}

// removeAccountCmd issues RemoveAccount(id), returning an accountRemovedMsg.
func removeAccountCmd(c client.BossClient, ctx context.Context, id string) tea.Cmd {
	return func() tea.Msg {
		err := c.RemoveAccount(ctx, id)
		return accountRemovedMsg{id: id, err: err}
	}
}

// --- Bound-session detection + prompt copy ---

// boundSessionCount counts sessions bound to accountID. known reports whether
// the session list was available: when false (never loaded or errored), prompt
// copy degrades to a generic warning rather than claiming zero. The count is
// advisory — the daemon may spawn/rotate between the count and the RPC — so the
// callers fail toward warning (a zero-but-unknown count still warns).
func boundSessionCount(sessions []*pb.Session, sessionsKnown bool, accountID string) (count int, known bool) {
	if !sessionsKnown {
		return 0, false
	}
	for _, s := range sessions {
		if s.GetAccountId() == accountID {
			count++
		}
	}
	return count, true
}

// accountDisablePromptText builds the confirm-prompt copy for disabling an
// account bound to live sessions (an unbound, known-safe disable skips the
// prompt entirely). It states the bound count, or a generic warning when the
// session list is unavailable, and explains that running chats may be
// interrupted.
func accountDisablePromptText(acct *pb.Account, count int, known bool) string {
	label := accountRowLabel(acct)
	switch {
	case !known:
		return fmt.Sprintf(
			"Disable account %q? Running chats bound to this account may be interrupted.", label)
	case count == 1:
		return fmt.Sprintf(
			"Disable account %q? 1 running chat is bound to this account and may be interrupted.", label)
	default:
		return fmt.Sprintf(
			"Disable account %q? %d running chats are bound to this account and may be interrupted.", label, count)
	}
}

// accountRemovePromptText builds the confirm-prompt copy for removing an
// account. Removal is always confirmed (it purges the keyring credential and is
// irreversible). When the account is bound (or the count is unknown) the copy
// escalates: it recommends disable over remove and explains that running chats
// may continue or fail depending on spawn state — never silently broken.
func accountRemovePromptText(acct *pb.Account, count int, known bool) string {
	label := accountRowLabel(acct)
	base := fmt.Sprintf(
		"Remove account %q? This purges its stored credential and cannot be undone.", label)
	switch {
	case known && count == 0:
		return base
	case !known:
		return base + " Running chats bound to this account may continue or fail depending on" +
			" spawn state — consider disable instead of remove."
	case count == 1:
		return base + " 1 running chat is bound to this account and may continue or fail depending on" +
			" spawn state — consider disable instead of remove."
	default:
		return base + fmt.Sprintf(" %d running chats are bound to this account and may continue or fail"+
			" depending on spawn state — consider disable instead of remove.", count)
	}
}

// --- Reauthenticate in place (BOS-1142) ---

// Reauthentication is not an RPC this package issues directly. The credential
// is acquired by an isolated device login and handed to the daemon's existing
// RefreshAccount — the mutating owner of the stored credential — so the action
// here is a view switch into that flow rather than a tea.Cmd wrapping a client
// call. What lives here is the copy and the eligibility rule the list and the
// edit view must not state differently.

// accountReauthProvider is the only provider whose in-place device login is
// implemented. Claude accounts are refreshed by pasting a setup token
// (`boss account refresh`), which is a different acquisition path.
const accountReauthProvider = "codex"

// accountReauthSupported reports whether [R]eauth can run for this account.
func accountReauthSupported(a *pb.Account) bool {
	return a != nil && a.GetProvider() == accountReauthProvider
}

// accountReauthUnsupportedStatus is the in-place refusal shown when [R] is
// pressed on an account whose provider has no device-login flow. It names the
// alternative rather than reporting a bare "not supported", so the operator's
// next move is in the message.
func accountReauthUnsupportedStatus(provider string) string {
	if provider == "" {
		provider = "this"
	}
	return fmt.Sprintf(
		"Reauthenticate is a %s device login; %s accounts refresh with a pasted credential instead (boss account refresh <id>).",
		accountReauthProvider, provider)
}
