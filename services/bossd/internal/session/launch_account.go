package session

import (
	"context"
	"fmt"
	"time"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/account"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/tmux"
)

// NoUsableAccountError refuses a launch whose account is out of service when no
// other account of its provider can take over. Its text is shown to the
// operator as is.
type NoUsableAccountError struct {
	Provider string
	Label    string
	Reason   string
}

func (e *NoUsableAccountError) Error() string {
	return fmt.Sprintf("no %s account can be used: %s is %s and no other account is available",
		e.Provider, e.Label, e.Reason)
}

// InServiceAccount returns the account a launch should run on in place of
// accountID. A binding to an out-of-service account (disabled, or suspended by
// its provider) is replaced with the provider's default account, and replaced
// reports the swap so the caller can persist it onto the binding it read. Any
// other binding, including none, comes back unchanged.
func (l *Lifecycle) InServiceAccount(ctx context.Context, provider string, accountID *string) (_ *string, replaced bool, _ error) {
	if accountID == nil || *accountID == "" || l.accountGetter == nil {
		return accountID, false, nil
	}
	acct, err := l.accountGetter(ctx, *accountID)
	if err != nil || !acct.IsOutOfService() {
		return accountID, false, nil
	}
	provider = proxyAgentName(provider)
	next := ""
	if l.defaultAccountResolver != nil {
		if next, err = l.defaultAccountResolver(ctx, provider, time.Now()); err != nil {
			return nil, false, fmt.Errorf("choose an account to replace %s: %w", accountLabel(acct), err)
		}
	}
	if next == "" || next == *accountID {
		return nil, false, &NoUsableAccountError{Provider: provider, Label: accountLabel(acct), Reason: outOfServiceReason(acct)}
	}
	return &next, true, nil
}

// InServiceSpawnTarget is effectiveSpawnTarget with an out-of-service account
// replaced by its provider's default. The swap is written back to the binding
// the account came from: the chat row when the chat carries its own account,
// otherwise the session for a chat running the session's agent. Resume and
// restart paths call it so they move off a disabled account the same way a
// fresh launch does, instead of stopping at the injection backstop.
func (l *Lifecycle) InServiceSpawnTarget(ctx context.Context, sess *models.Session) (*models.Session, *models.AgentChat, error) {
	spawn, chat := l.effectiveSpawnTarget(ctx, sess)
	next, replaced, err := l.InServiceAccount(ctx, spawn.AgentName, spawn.AccountID)
	if err != nil || !replaced {
		return spawn, chat, err
	}
	switch {
	case chat != nil && chat.AccountID != nil:
		if err := l.agentChats.UpdateAccountIDByAgentSessionID(ctx, chat.AgentSessionID, next); err != nil {
			return nil, nil, fmt.Errorf("move chat %s to account %q: %w", chat.AgentSessionID, *next, err)
		}
		moved := *chat
		moved.AccountID = next
		chat = &moved
	case proxyAgentName(spawn.AgentName) == proxyAgentName(sess.AgentName):
		if err := l.MoveSessionAccount(ctx, sess, *next); err != nil {
			return nil, nil, err
		}
	}
	view := *spawn
	view.AccountID = next
	return &view, chat, nil
}

// MoveSessionAccount rebinds the session to accountID, the account its next
// chat starts on. A live pane started from the session's account holds the
// session proxy token and reads its bearer from this binding, so it moves too.
// Its chat row is rewritten to match, so the row shows the account the pane
// bills. Panes holding a chat token read their own row and stay where they are.
func (l *Lifecycle) MoveSessionAccount(ctx context.Context, sess *models.Session, accountID string) error {
	ptr := &accountID
	if _, err := l.sessions.Update(ctx, sess.ID, db.UpdateSessionParams{AccountID: &ptr}); err != nil {
		return fmt.Errorf("move session %s to account %q: %w", sess.ID, accountID, err)
	}
	sess.AccountID = ptr
	l.relabelSessionTokenChats(ctx, sess, accountID)
	return nil
}

func (l *Lifecycle) relabelSessionTokenChats(ctx context.Context, sess *models.Session, accountID string) {
	if l.agentChats == nil {
		return
	}
	chats, err := l.agentChats.ListBySession(ctx, sess.ID)
	if err != nil {
		l.logger.Warn().Err(err).Str("session", sess.ID).Msg("account: failed to list the chats that follow the session account")
		return
	}
	sessionURL := ""
	for _, chat := range chats {
		if chat == nil || chat.AccountID == nil || *chat.AccountID == accountID || !sameAgentSessionChat(sess, chat) {
			continue
		}
		pane := persistedChatPaneName(chat)
		if pane == "" {
			pane = tmux.ChatSessionName(sess.RepoID, chat.AgentSessionID)
		}
		if !l.paneIsLive(ctx, pane) {
			continue
		}
		baked, ok := l.paneBakedProxyURL(ctx, pane)
		if !ok {
			continue
		}
		if sessionURL == "" {
			if sessionURL = l.sessionProxyURL(sess.ID); sessionURL == "" {
				return
			}
		}
		if baked != sessionURL {
			continue
		}
		if err := l.agentChats.UpdateAccountIDByAgentSessionID(ctx, chat.AgentSessionID, &accountID); err != nil {
			l.logger.Warn().Err(err).Str("agent_session", chat.AgentSessionID).Str("account_id", accountID).
				Msg("account: failed to relabel a chat that moved with the session account")
		}
	}
}

func accountLabel(a *models.Account) string {
	if a.Label != "" {
		return a.Label
	}
	return account.ShortID(a.ID)
}

func outOfServiceReason(a *models.Account) string {
	if a.Status == models.AccountStatusDisabled {
		return "disabled"
	}
	return "suspended by its provider"
}
