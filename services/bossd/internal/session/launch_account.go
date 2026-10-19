package session

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/safego"
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
			return nil, nil, l.MoveChatError(ctx, *next, err)
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
	if err := l.writeSessionAccount(ctx, sess.ID, accountID); err != nil {
		return err
	}
	sess.AccountID = &accountID
	l.relabelSessionTokenChats(ctx, sess.ID)
	return nil
}

// writeSessionAccount persists the session's account binding and nothing else.
// It touches no tmux, so a failover can run it before the replayed response
// streams.
func (l *Lifecycle) writeSessionAccount(ctx context.Context, sessionID, accountID string) error {
	ptr := &accountID
	if _, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{AccountID: &ptr}); err != nil {
		return fmt.Errorf("could not move this session to %s: %w", l.resolveAccountLabel(ctx, accountID), err)
	}
	return nil
}

// MoveChatError is the operator-facing text for a chat row whose account write
// failed: the target by its label, then the cause.
func (l *Lifecycle) MoveChatError(ctx context.Context, accountID string, cause error) error {
	return fmt.Errorf("could not move this chat to %s: %w", l.resolveAccountLabel(ctx, accountID), cause)
}

// relabelTimeout bounds one pass over a session's chat rows.
const relabelTimeout = 5 * time.Second

// relabelRuns tracks the rewrites of the chat rows that follow a session's
// account. Rewrites of one session never overlap: a move that lands while one
// runs marks it to run again, and the running one reads the session's account
// afresh, so the later move wins even when the earlier rewrite is wedged.
// Background rewrites run on a context shutdown cancels and are joined before
// the database closes. The zero value is ready to use.
type relabelRuns struct {
	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
	closed bool
	wg     sync.WaitGroup
	// active holds each session with a rewrite running. The value is true when
	// a later move landed during that rewrite.
	active map[string]bool
}

// claim reports whether the caller now owns sessionID's rewrite. When another
// rewrite owns it, claim marks that one to run again and returns false.
func (r *relabelRuns) claim(sessionID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, running := r.active[sessionID]; running {
		r.active[sessionID] = true
		return false
	}
	if r.active == nil {
		r.active = map[string]bool{}
	}
	r.active[sessionID] = false
	return true
}

// again reports whether a move landed during the rewrite that just ended, and
// releases the claim when none did.
func (r *relabelRuns) again(sessionID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active[sessionID] {
		r.active[sessionID] = false
		return true
	}
	delete(r.active, sessionID)
	return false
}

func (r *relabelRuns) release(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.active, sessionID)
}

// track registers a background rewrite and returns the shutdown context it
// runs on. It refuses once shutdown has begun.
func (r *relabelRuns) track() (context.Context, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, false
	}
	r.initLocked()
	r.wg.Add(1)
	return r.ctx, true
}

func (r *relabelRuns) initLocked() {
	if r.ctx == nil {
		r.ctx, r.cancel = context.WithCancel(context.Background()) //nolint:gosec // cancel is stored and called in StopBackgroundRelabels
	}
}

// StopBackgroundRelabels refuses new background rewrites of chat rows, cancels
// the running ones, and waits for them to return or for ctx to end. It reports
// whether every one returned. Daemon shutdown calls it before the database
// closes, so no rewrite writes to a closed store.
func (l *Lifecycle) StopBackgroundRelabels(ctx context.Context) bool {
	r := &l.relabels
	r.mu.Lock()
	r.closed = true
	r.initLocked()
	r.mu.Unlock()
	r.cancel()
	done := safego.Go(l.logger, r.wg.Wait)
	select {
	case <-done:
		return true
	case <-ctx.Done():
		select {
		case <-done:
			return true
		default:
			return false
		}
	}
}

// BackgroundRelabelsStopped reports whether StopBackgroundRelabels has run, so
// a shutdown test can tell the daemon stopped them.
func (l *Lifecycle) BackgroundRelabelsStopped() bool {
	l.relabels.mu.Lock()
	defer l.relabels.mu.Unlock()
	return l.relabels.closed
}

func (l *Lifecycle) canRelabel() bool {
	return l.agentChats != nil && l.proxyRegistrar != nil && l.proxyPort != 0
}

// relabelSessionTokenChatsInBackground runs relabelSessionTokenChats off the
// caller's path. A failover calls it so the replayed response never waits on
// tmux: the rows are display labels, and billing already follows the session
// binding the commit wrote. A failure only logs.
func (l *Lifecycle) relabelSessionTokenChatsInBackground(sessionID string) {
	if !l.canRelabel() || !l.relabels.claim(sessionID) {
		return
	}
	l.relabelUntilSettled(sessionID)
}

// relabelSessionTokenChats rewrites the rows of running panes whose baked token
// resolves to the session, so each row names the session's current account.
// When another rewrite of the session is running it returns at once, and that
// rewrite runs again and picks up this move's account.
func (l *Lifecycle) relabelSessionTokenChats(ctx context.Context, sessionID string) {
	if !l.canRelabel() || !l.relabels.claim(sessionID) {
		return
	}
	l.claimedRelabelPass(context.WithoutCancel(ctx), sessionID)
	if l.relabels.again(sessionID) {
		l.relabelUntilSettled(sessionID)
	}
}

// relabelUntilSettled takes over a claimed rewrite in a tracked background
// goroutine and repeats it while later moves keep landing.
func (l *Lifecycle) relabelUntilSettled(sessionID string) {
	ctx, ok := l.relabels.track()
	if !ok {
		l.relabels.release(sessionID)
		return
	}
	// Joined through relabels.wg by StopBackgroundRelabels.
	_ = safego.Go(l.logger, func() {
		defer l.relabels.wg.Done()
		for {
			l.claimedRelabelPass(ctx, sessionID)
			if ctx.Err() != nil {
				l.relabels.release(sessionID)
				return
			}
			if !l.relabels.again(sessionID) {
				return
			}
		}
	})
}

// claimedRelabelPass runs one pass for the caller that holds sessionID's claim.
// A panic in the pass releases the claim as it unwinds: a claim left set would
// turn every later move on the session into a mark that nothing runs.
func (l *Lifecycle) claimedRelabelPass(ctx context.Context, sessionID string) {
	finished := false
	defer func() {
		if !finished {
			l.relabels.release(sessionID)
		}
	}()
	l.relabelPass(ctx, sessionID)
	finished = true
}

// relabelPass reads the session's account and rewrites the rows of its running
// same-agent chats whose pane holds the session token. It asks the proxy what a
// token resolves to instead of comparing it with the session's current token:
// after a daemon restart the proxy holds only digests, and a pane started
// before the restart still bills through the session.
func (l *Lifecycle) relabelPass(parent context.Context, sessionID string) {
	ctx, cancel := context.WithTimeout(parent, relabelTimeout)
	defer cancel()
	sess, err := l.sessions.Get(ctx, sessionID)
	if err != nil {
		l.logger.Warn().Err(err).Str("session", sessionID).Msg("account: failed to read the session whose chats follow its account")
		return
	}
	if sess == nil || sess.AccountID == nil {
		return
	}
	accountID := *sess.AccountID
	chats, err := l.agentChats.ListBySession(ctx, sess.ID)
	if err != nil {
		l.logger.Warn().Err(err).Str("session", sess.ID).Msg("account: failed to list the chats that follow the session account")
		return
	}
	var stale []*models.AgentChat
	for _, chat := range chats {
		if chat != nil && chat.AccountID != nil && *chat.AccountID != accountID &&
			sameAgentSessionChat(sess, chat) && l.isChatRunning(chat.AgentSessionID) {
			stale = append(stale, chat)
		}
	}
	for i, chat := range stale {
		if l.relabelChat(ctx, sess, accountID, chat) || ctx.Err() == nil {
			continue
		}
		l.logUnfinishedPass(ctx.Err(), sess.ID, len(stale), len(stale)-i-1)
		return
	}
}

// logUnfinishedPass reports a pass that stopped mid-read. The row being read
// was not rewritten, and notReached rows after it were never read. A cancelled
// pass was stopped by daemon shutdown: the inline pass runs on a context that
// cannot be cancelled, so only the background one's shutdown context can.
func (l *Lifecycle) logUnfinishedPass(cause error, sessionID string, total, notReached int) {
	why := "ran out of time"
	if errors.Is(cause, context.Canceled) {
		why = "was stopped by daemon shutdown while"
	}
	l.logger.Warn().Err(cause).Str("session", sessionID).
		Int("rows_not_rewritten", notReached+1).Int("rows_not_reached", notReached).
		Msgf("account: %s moving chat rows to the session's new account; %d of %d running chats may still show the previous account (1 cut off mid-read, %d never reached)",
			why, notReached+1, total, notReached)
}

// relabelChat moves one chat row to accountID when its pane holds the session
// token, and reports whether it did.
func (l *Lifecycle) relabelChat(ctx context.Context, sess *models.Session, accountID string, chat *models.AgentChat) bool {
	pane := persistedChatPaneName(chat)
	if pane == "" {
		pane = tmux.ChatSessionName(sess.RepoID, chat.AgentSessionID)
	}
	// A pane that went away since the tracker's last poll fails this read.
	baked, ok := l.paneBakedProxyURL(ctx, pane)
	if !ok {
		return false
	}
	token, ok := parseBakedProxyToken(baked, l.proxyPort)
	if !ok {
		return false
	}
	if target, ok := l.proxyRegistrar.TokenTarget(token); !ok || target != sess.ID {
		return false
	}
	if err := l.agentChats.UpdateAccountIDByAgentSessionID(ctx, chat.AgentSessionID, &accountID); err != nil {
		l.logger.Warn().Err(err).Str("agent_session", chat.AgentSessionID).Str("account_id", accountID).
			Msg("account: failed to relabel a chat that moved with the session account")
		return false
	}
	return true
}

// isChatRunning asks the status tracker whether a chat's pane is up. With no
// tracker wired every chat counts as running, so rows still follow a move.
func (l *Lifecycle) isChatRunning(agentSessionID string) bool {
	return l.chatRunning == nil || l.chatRunning(agentSessionID)
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
