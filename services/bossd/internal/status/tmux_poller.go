package status

import (
	"context"
	"strings"
	"sync"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/safego"
	"github.com/recurser/bossalib/statusdetect"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/status/questionsignal"
	"github.com/recurser/bossd/internal/tmux"
	"github.com/rs/zerolog"
)

// TmuxStatusPoller polls tmux pane content for active chats and feeds
// working/idle/question statuses into the status tracker.
type TmuxStatusPoller struct {
	tracker      *Tracker
	chats        db.AgentChatStore
	sessions     db.SessionStore
	tmux         *tmux.Client
	agentClients map[string]agent.AgentRunnerClient
	logger       zerolog.Logger

	// questionSignals is the structured "a question is pending" store (BOS-485).
	// A nil store means pure regex behavior — questionState falls back to the
	// per-agent HasQuestionPrompt pane scraper exactly as before, so existing
	// wiring and tests are unaffected until SetQuestionSignals injects a store.
	questionSignals *questionsignal.Store

	mu            sync.Mutex
	prevCaptures  map[string]captureEntry // agentSessionID -> previous capture
	missingLogged map[string]struct{}     // agent name -> already-logged "missing client" warning

	done chan struct{} // closed when Run's goroutine exits
}

type captureEntry struct {
	content string
	at      time.Time
}

// NewTmuxStatusPoller creates a new poller. sessions may be nil in tests that
// don't exercise the transcript-aware question-suppression path. agentClients
// is the per-name registry of AgentRunnerClient gRPC clients used to dispatch
// HasQuestionPrompt / LastTurnIsUser to the right plugin based on each chat's
// AgentName. A nil or empty map disables prompt detection — every poll lands
// in the "no client" branch and the chat goes IDLE.
func NewTmuxStatusPoller(tracker *Tracker, chats db.AgentChatStore, sessions db.SessionStore, tmux *tmux.Client, agentClients map[string]agent.AgentRunnerClient, logger zerolog.Logger) *TmuxStatusPoller {
	if agentClients == nil {
		agentClients = map[string]agent.AgentRunnerClient{}
	}
	return &TmuxStatusPoller{
		tracker:       tracker,
		chats:         chats,
		sessions:      sessions,
		tmux:          tmux,
		agentClients:  agentClients,
		logger:        logger,
		prevCaptures:  make(map[string]captureEntry),
		missingLogged: make(map[string]struct{}),
		done:          make(chan struct{}),
	}
}

// SetQuestionSignals injects the structured question-signal store (BOS-485).
// Call before Run/Bootstrap. Passing nil (or never calling this) keeps the
// poller on the pure regex fallback path.
func (p *TmuxStatusPoller) SetQuestionSignals(store *questionsignal.Store) {
	p.questionSignals = store
}

// PollInterval is the interval between tmux status polls.
const PollInterval = 3 * time.Second

// IdleThreshold is the duration of unchanged output before reporting idle.
const IdleThreshold = 5 * time.Second

// capturedTailMaxBytes bounds the pane tail captured at process death (~4 KiB)
// so a runaway agent transcript can't bloat the ephemeral diagnostic store
// (BOS-477).
const capturedTailMaxBytes = 4096

// boundedTail returns the last <= maxBytes bytes of s, advanced forward to the
// next whole-line boundary so the returned tail never begins mid-line. Input at
// or under maxBytes is returned unchanged.
func boundedTail(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	tail := s[len(s)-maxBytes:]
	// Drop a leading partial line so the tail starts cleanly; guard against
	// consuming the whole slice when the only newline is the final byte.
	if nl := strings.IndexByte(tail, '\n'); nl >= 0 && nl+1 < len(tail) {
		tail = tail[nl+1:]
	}
	return tail
}

// Run starts the background polling goroutine. It stops when ctx is cancelled.
func (p *TmuxStatusPoller) Run(ctx context.Context) {
	safego.Go(p.logger, func() {
		defer close(p.done)
		ticker := time.NewTicker(PollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.pollOnce(ctx)
			}
		}
	})
}

// Done returns a channel closed when Run's goroutine exits.
func (p *TmuxStatusPoller) Done() <-chan struct{} { return p.done }

// pollOnce scans all chats with non-null tmux_session_name and updates statuses.
//
// The DB is re-queried every tick. prevCaptures is content-comparison cache
// only — it is never the source of truth for which chats to poll. This makes
// the poller self-healing: a transient DB or tmux error that drops a chat
// from prevCaptures, or a chat that was never registered in the first place,
// is rediscovered on the next tick.
func (p *TmuxStatusPoller) pollOnce(ctx context.Context) {
	chats, err := p.chats.ListWithTmuxSession(ctx)
	if err != nil {
		p.logger.Warn().Err(err).Msg("pollOnce: failed to list chats with tmux sessions")
		return
	}

	now := time.Now()

	// Filter to chats whose tmux session is alive right now. Chats whose DB
	// tmux reference no longer exists must actively report STOPPED; otherwise
	// the persisted session display label can remain stuck at "working" until
	// some unrelated status change triggers a recompute.
	activeChats := make([]*models.AgentChat, 0, len(chats))
	seen := make(map[string]bool, len(chats))
	for _, chat := range chats {
		if chat.TmuxSessionName == nil || *chat.TmuxSessionName == "" {
			p.tracker.Update(chat.AgentSessionID, pb.ChatStatus_CHAT_STATUS_STOPPED, now)
			p.tracker.SetAuthFailed(chat.AgentSessionID, false)
			p.tracker.SetTransientAPIError(chat.AgentSessionID, false)
			continue
		}
		if !p.tmux.HasSession(ctx, *chat.TmuxSessionName) {
			p.tracker.Update(chat.AgentSessionID, pb.ChatStatus_CHAT_STATUS_STOPPED, now)
			p.tracker.SetAuthFailed(chat.AgentSessionID, false)
			p.tracker.SetTransientAPIError(chat.AgentSessionID, false)
			continue
		}
		// With remain-on-exit armed (BOS-477), a chat whose agent process exited
		// keeps the tmux session alive as a pane_dead zombie, so HasSession stays
		// true. Capture the final tail ONCE at death, then reap the pane so it
		// can't linger, and report STOPPED. The KillSession runs unconditionally
		// (even if capture failed) so a dead pane never survives as a zombie.
		if dead, _ := p.tmux.PaneDead(ctx, *chat.TmuxSessionName); dead {
			if tail, err := p.tmux.CapturePane(ctx, *chat.TmuxSessionName); err == nil {
				p.tracker.SetCapturedOutput(chat.AgentSessionID, boundedTail(tail, capturedTailMaxBytes))
			}
			_ = p.tmux.KillSession(ctx, *chat.TmuxSessionName)
			p.tracker.Update(chat.AgentSessionID, pb.ChatStatus_CHAT_STATUS_STOPPED, now)
			p.tracker.SetAuthFailed(chat.AgentSessionID, false)
			p.tracker.SetTransientAPIError(chat.AgentSessionID, false)
			continue
		}
		activeChats = append(activeChats, chat)
		seen[chat.AgentSessionID] = true
	}

	// GC prevCaptures entries for chats that are no longer in the active set
	// (DB row removed, tmux name cleared, or tmux session died). Also drop any
	// structured question-signal record for a departed chat (BOS-485 plan step
	// 6): a chat whose Notification fired but was never answered, then stopped,
	// is filtered out before questionState, so its record would otherwise linger
	// (harmless for correctness — a gone chat can't re-surface QUESTION — but
	// never physically reclaimed before the TTL). Clearing here bounds the store
	// to live chats. Clear keys on AgentSessionID, which for the claude writer
	// equals the store key (chatResumeSessionID); a future agent whose provider
	// id differs still self-heals via the TTL.
	p.mu.Lock()
	for id := range p.prevCaptures {
		if !seen[id] {
			delete(p.prevCaptures, id)
			if p.questionSignals != nil {
				p.questionSignals.Clear(id)
			}
		}
	}
	p.mu.Unlock()

	for _, chat := range activeChats {
		agentSessionID := chat.AgentSessionID
		tmuxName := *chat.TmuxSessionName
		content, err := p.tmux.CapturePane(ctx, tmuxName)
		if err != nil {
			p.logger.Debug().Err(err).
				Str("agentSessionID", agentSessionID).
				Str("tmuxSession", tmuxName).
				Msg("failed to capture tmux pane")
			continue
		}

		p.refreshChatTitle(ctx, chat)

		// Detect the login-required terminal shape from the SAME captured pane
		// content (genuine agent output, not pipe-pane mtime) and record/clear
		// the auth marker. Narrow, whole-line detection that fails toward NOT
		// flagging (see statusdetect.IsLoginRequired); the server reads this
		// marker to surface the AGENT_AUTH_FAILED attention reason.
		p.tracker.SetAuthFailed(agentSessionID, statusdetect.IsLoginRequired([]byte(content)))

		// Reuse the SAME already-captured pane content to record/clear the
		// transient-API-failure marker (a 5xx/gateway banner that aborted the
		// turn). No extra capture, no extra tmux round-trip — one read feeds
		// both overlays and the status decision below.
		p.tracker.SetTransientAPIError(agentSessionID, statusdetect.IsTransientAPIError([]byte(content)))

		// Resolve limit + question state before taking p.mu — both may issue
		// plugin RPCs / DB queries and we hold the mutex only briefly below.
		// A usage-limit banner wins over every other signal (plan D-C), so
		// resolve it first and let it short-circuit the status switch.
		paneLimited, limitResetAt := p.limitState(ctx, chat, content)
		paneShowsQuestion, questionSuppressed := p.questionState(ctx, chat, content)

		p.mu.Lock()
		prev, hasPrev := p.prevCaptures[agentSessionID]
		captureChanged := !hasPrev || content != prev.content || prev.at.IsZero()

		var status pb.ChatStatus
		wouldBeIdle := false
		switch {
		case paneLimited:
			// Highest precedence: a limited pane beats question/working/idle.
			// When the banner later redraws away, limitState returns false and
			// the chat falls through to the existing branches (WORKING/IDLE).
			status = pb.ChatStatus_CHAT_STATUS_LIMITED
		case paneShowsQuestion && !questionSuppressed:
			status = pb.ChatStatus_CHAT_STATUS_QUESTION
		case questionSuppressed:
			// The pane still matches the question pattern but the transcript
			// shows the user has answered — Claude is about to render its
			// response. Report WORKING explicitly so the UI doesn't briefly
			// flash IDLE when the old question capture is already past the
			// idle threshold.
			status = pb.ChatStatus_CHAT_STATUS_WORKING
		case captureChanged:
			status = pb.ChatStatus_CHAT_STATUS_WORKING
		case now.Sub(prev.at) > IdleThreshold:
			// The pane is unchanged past the idle threshold. Defer the final
			// IDLE decision until after we release p.mu: a genuinely-busy pane
			// (a running background shell, an active spinner) stays static yet
			// must not flip to IDLE. paneShowsWorking below issues the plugin
			// RPC only in this would-be-idle branch, so active chats add none.
			wouldBeIdle = true
			status = pb.ChatStatus_CHAT_STATUS_IDLE
		default:
			// Content unchanged but not yet past idle threshold -- keep working.
			status = pb.ChatStatus_CHAT_STATUS_WORKING
		}

		// LastOutputAt is the last pane content change time; ReceivedAt in the
		// tracker remains the fresh heartbeat time.
		lastOutputAt := now
		if captureChanged {
			p.prevCaptures[agentSessionID] = captureEntry{content: content, at: now}
		} else {
			lastOutputAt = prev.at
		}
		p.mu.Unlock()

		// Consult the working indicator only when we would otherwise report
		// IDLE (RPC held outside p.mu, and skipped entirely for active chats).
		// Preserves QUESTION > WORKING > IDLE: this branch is only reached when
		// the pane is neither a live question nor freshly changed.
		if wouldBeIdle && p.paneShowsWorking(ctx, chat, content) {
			status = pb.ChatStatus_CHAT_STATUS_WORKING
		}

		if paneLimited {
			p.tracker.UpdateLimited(agentSessionID, limitResetAt, lastOutputAt)
		} else {
			p.tracker.Update(agentSessionID, status, lastOutputAt)
		}
	}
}

// refreshChatTitle asks the chat's AgentRunner plugin to extract a chat title
// from the on-disk transcript. Placeholder titles ("" or "New chat") may be
// backfilled from first-user-message heuristics; non-placeholder titles are
// overwritten only when the plugin reports an explicit agent rename.
//
// This is the daemon-side counterpart to the TUI's best-effort title
// backfill — it's what makes Codex chats render with their first user
// message instead of "New chat" in the web UI (which only reads
// chat.Title from the database, with no filesystem fallback).
func (p *TmuxStatusPoller) refreshChatTitle(ctx context.Context, chat *models.AgentChat) {
	if chat == nil || p.sessions == nil {
		return
	}
	client, ok := p.agentClients[chat.AgentName]
	if !ok {
		p.logMissingAgentOnce(chat.AgentName)
		return
	}
	sess, err := p.sessions.Get(ctx, chat.SessionID)
	if err != nil || sess == nil || sess.WorktreePath == "" {
		return
	}
	resp, err := client.GetChatTitle(ctx, &pb.GetChatTitleRequest{
		WorkDir:   sess.WorktreePath,
		SessionId: chatResumeSessionID(chat),
	})
	if err != nil || resp == nil || !resp.GetSupported() {
		return
	}
	title := strings.TrimSpace(resp.GetTitle())
	if title == "" || title == strings.TrimSpace(chat.Title) {
		return
	}
	if !resp.GetExplicit() && !shouldRefreshChatTitle(chat.Title) {
		return
	}
	if err := p.chats.UpdateTitleByAgentSessionID(ctx, chat.AgentSessionID, title); err != nil {
		p.logger.Warn().Err(err).
			Str("agentSessionID", chat.AgentSessionID).
			Msg("tmux poller: failed to update chat title")
		return
	}
	chat.Title = title
}

// shouldRefreshChatTitle reports whether a stored chat title is still a
// placeholder that we should try to overwrite with the first real user
// message. We only refresh empty strings and the literal "New chat"
// placeholder (case- and whitespace-insensitive). Any other value is
// treated as a user-customised title and left alone.
func shouldRefreshChatTitle(title string) bool {
	switch strings.ToLower(strings.TrimSpace(title)) {
	case "", "new chat":
		return true
	default:
		return false
	}
}

// RegisterChat adds a chat to the polling set. Called when a new tmux session
// is created so the poller starts tracking it immediately.
func (p *TmuxStatusPoller) RegisterChat(agentSessionID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.prevCaptures[agentSessionID]; !ok {
		p.prevCaptures[agentSessionID] = captureEntry{}
	}
}

// UnregisterChat removes a chat from the polling set.
func (p *TmuxStatusPoller) UnregisterChat(agentSessionID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.prevCaptures, agentSessionID)
}

// Bootstrap discovers pre-existing tmux sessions from the database and seeds
// the poller with their current status. This must be called before Run() so
// that sessions surviving a daemon restart are immediately tracked with the
// correct status (idle or question) instead of being left unknown.
func (p *TmuxStatusPoller) Bootstrap(ctx context.Context) {
	chats, err := p.chats.ListWithTmuxSession(ctx)
	if err != nil {
		p.logger.Warn().Err(err).Msg("bootstrap: failed to list chats with tmux sessions")
		return
	}

	now := time.Now()
	// Use a timestamp in the past so the next pollOnce sees unchanged content
	// as having exceeded IdleThreshold, and reports idle. It is also the
	// LastOutputAt we seed into the tracker (below): on restart we have not
	// observed any genuine output, so exporting `now` would make a stalled
	// pane that survived the restart read as freshly active via
	// last_agent_activity_at until the threshold elapses. Seeding pastTime
	// keeps the heartbeat honest — Tracker.Update stamps ReceivedAt=now
	// internally, so status/display freshness is unaffected.
	pastTime := now.Add(-IdleThreshold - time.Second)

	for _, chat := range chats {
		tmuxName := *chat.TmuxSessionName
		if !p.tmux.HasSession(ctx, tmuxName) {
			continue
		}

		content, err := p.tmux.CapturePane(ctx, tmuxName)
		if err != nil {
			p.logger.Debug().Err(err).
				Str("agentSessionID", chat.AgentSessionID).
				Str("tmuxSession", tmuxName).
				Msg("bootstrap: failed to capture tmux pane")
			continue
		}

		p.tracker.SetAuthFailed(chat.AgentSessionID, statusdetect.IsLoginRequired([]byte(content)))
		// Same single capture feeds the transient-API-failure marker, so a chat
		// whose turn died on a 5xx banner is flagged from the first bootstrap
		// pass rather than waiting a poll tick.
		p.tracker.SetTransientAPIError(chat.AgentSessionID, statusdetect.IsTransientAPIError([]byte(content)))

		paneLimited, limitResetAt := p.limitState(ctx, chat, content)
		paneShowsQuestion, questionSuppressed := p.questionState(ctx, chat, content)

		var status pb.ChatStatus
		switch {
		case paneLimited:
			// Seed LIMITED for a session that restarts while its pane still
			// shows the usage-cap banner. Highest precedence, mirrors pollOnce.
			status = pb.ChatStatus_CHAT_STATUS_LIMITED
		case paneShowsQuestion && !questionSuppressed:
			status = pb.ChatStatus_CHAT_STATUS_QUESTION
		case questionSuppressed:
			// Mirror pollOnce: the pane still matches the question pattern but
			// the transcript shows the user has answered. Report WORKING so the
			// UI doesn't flash IDLE before the first poll cycle corrects it.
			status = pb.ChatStatus_CHAT_STATUS_WORKING
		case p.paneShowsWorking(ctx, chat, content):
			// A session restored mid-background-shell (or with an active
			// spinner) seeds WORKING, not IDLE, so it doesn't flash idle until
			// the pane next changes. Mirrors pollOnce's would-be-idle override.
			status = pb.ChatStatus_CHAT_STATUS_WORKING
		default:
			status = pb.ChatStatus_CHAT_STATUS_IDLE
		}

		p.mu.Lock()
		p.prevCaptures[chat.AgentSessionID] = captureEntry{content: content, at: pastTime}
		p.mu.Unlock()

		if paneLimited {
			p.tracker.UpdateLimited(chat.AgentSessionID, limitResetAt, pastTime)
		} else {
			p.tracker.Update(chat.AgentSessionID, status, pastTime)
		}
		p.logger.Debug().
			Str("agentSessionID", chat.AgentSessionID).
			Str("tmuxSession", tmuxName).
			Str("status", status.String()).
			Msg("bootstrap: seeded chat status")
	}

	if len(chats) > 0 {
		p.logger.Info().Int("count", len(chats)).Msg("bootstrap: discovered chats with tmux sessions")
	}
}

// questionState resolves whether the chat is asking a question and, if so,
// whether the user has already answered.
//
// The structured signal is primary (BOS-485): if the question-signal store
// holds a fresh pending record for this chat (set by the agent's Notification
// hook via POST /hooks/question/{id}), the chat is treated as showing a
// question WITHOUT consulting the pane regex. When there is no record (or no
// store is wired), it falls back to the per-agent HasQuestionPrompt pane
// scraper byte-for-byte as before — so no regression when the explicit signal
// is absent.
//
// Either way, the answer is reconciled the same way: LastTurnIsUser (run per
// agent against the on-disk transcript) reports whether the user has already
// responded. When they have, the pending record is cleared (mirroring the
// regex path's questionSuppressed) so a stale Notification can't re-assert
// QUESTION; the TTL is the backstop for a missed clear.
//
// When no client is registered for the chat's AgentName the regex path can't
// run, so we fail open to "no question" — a missing plugin can never lock a
// chat in QUESTION. A structured record that predates a plugin unload still
// surfaces the question (it was an explicit signal) but can't be reconciled
// without a client until the TTL ages it out.
func (p *TmuxStatusPoller) questionState(ctx context.Context, chat *models.AgentChat, content string) (paneShowsQuestion, questionSuppressed bool) {
	if chat == nil {
		return false, false
	}
	resumeID := chatResumeSessionID(chat)

	structured := false
	if p.questionSignals != nil {
		if _, ok := p.questionSignals.Get(resumeID); ok {
			structured = true
			paneShowsQuestion = true
		}
	}

	client, ok := p.agentClients[chat.AgentName]
	if !ok {
		p.logMissingAgentOnce(chat.AgentName)
		// No client: can't run the regex path or reconcile via LastTurnIsUser.
		// Surface the structured signal (if any) unsuppressed; otherwise no
		// question.
		return paneShowsQuestion, false
	}

	if !structured {
		// Regex fallback — byte-for-byte the historical behavior.
		hpResp, err := client.HasQuestionPrompt(ctx, &pb.HasQuestionPromptRequest{PaneContent: []byte(content)})
		if err != nil || hpResp == nil || !hpResp.GetHasPrompt() {
			return false, false
		}
		paneShowsQuestion = true
	}

	if p.sessions == nil {
		return paneShowsQuestion, false
	}
	sess, err := p.sessions.Get(ctx, chat.SessionID)
	if err != nil || sess == nil || sess.WorktreePath == "" {
		return paneShowsQuestion, false
	}
	luResp, err := client.LastTurnIsUser(ctx, &pb.LastTurnIsUserRequest{
		WorkDir:        sess.WorktreePath,
		AgentSessionId: resumeID,
	})
	questionSuppressed = err == nil && luResp != nil && luResp.GetIsUser()
	if questionSuppressed && p.questionSignals != nil {
		// The user answered — drop any pending structured record so it can't
		// re-fire QUESTION next poll. No-op when there was no record.
		p.questionSignals.Clear(resumeID)
	}
	return paneShowsQuestion, questionSuppressed
}

// paneShowsWorking asks the chat's AgentRunner plugin whether the captured
// pane content shows an affirmative "still working" marker (a running
// background shell or an active spinner). It is dispatched per-agent over the
// HasWorkingIndicator RPC so each agent owns its own grammar, exactly like
// questionState. Called only in the would-be-idle branch of pollOnce/Bootstrap
// so active chats add no extra RPC. When no client is registered for the
// chat's AgentName it fails open to "not working" — a missing plugin can never
// pin a chat in WORKING.
func (p *TmuxStatusPoller) paneShowsWorking(ctx context.Context, chat *models.AgentChat, content string) bool {
	if chat == nil {
		return false
	}
	client, ok := p.agentClients[chat.AgentName]
	if !ok {
		p.logMissingAgentOnce(chat.AgentName)
		return false
	}
	resp, err := client.HasWorkingIndicator(ctx, &pb.HasWorkingIndicatorRequest{PaneContent: []byte(content)})
	return err == nil && resp != nil && resp.GetIsWorking()
}

// limitState resolves whether the captured pane content shows a usage-limit
// banner and, if so, when the limit resets. It is dispatched per-agent over the
// DetectUsageLimit RPC so each agent owns its own banner grammar and reset
// parser, exactly like questionState / paneShowsWorking. When no client is
// registered for the chat's AgentName, or the RPC errors, it fails open to "not
// limited" — a missing plugin can never pin a chat in LIMITED. resetAt carries
// the parsed reset time when the banner supplied one, and is zero otherwise.
func (p *TmuxStatusPoller) limitState(ctx context.Context, chat *models.AgentChat, content string) (limited bool, resetAt time.Time) {
	if chat == nil {
		return false, time.Time{}
	}
	client, ok := p.agentClients[chat.AgentName]
	if !ok {
		p.logMissingAgentOnce(chat.AgentName)
		return false, time.Time{}
	}
	resp, err := client.DetectUsageLimit(ctx, &pb.DetectUsageLimitRequest{PaneContent: []byte(content)})
	if err != nil || resp == nil || !resp.GetLimited() {
		return false, time.Time{}
	}
	if r := resp.GetResetAt(); r != nil {
		resetAt = r.AsTime()
	}
	return true, resetAt
}

func chatResumeSessionID(chat *models.AgentChat) string {
	if chat == nil {
		return ""
	}
	if chat.ProviderSessionID != nil && *chat.ProviderSessionID != "" {
		return *chat.ProviderSessionID
	}
	return chat.AgentSessionID
}

// logMissingAgentOnce emits a single warning per unknown agent name so the
// daemon log stays quiet when an old chat references a plugin that's no
// longer loaded — without dropping the signal entirely.
func (p *TmuxStatusPoller) logMissingAgentOnce(name string) {
	p.mu.Lock()
	if _, already := p.missingLogged[name]; already {
		p.mu.Unlock()
		return
	}
	p.missingLogged[name] = struct{}{}
	p.mu.Unlock()
	p.logger.Warn().Str("agent", name).Msg("tmux poller: no AgentRunnerClient for agent name; question detection disabled for chats with this agent")
}
