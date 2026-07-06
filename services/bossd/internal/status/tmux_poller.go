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

// PollInterval is the interval between tmux status polls.
const PollInterval = 3 * time.Second

// IdleThreshold is the duration of unchanged output before reporting idle.
const IdleThreshold = 5 * time.Second

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
			continue
		}
		if !p.tmux.HasSession(ctx, *chat.TmuxSessionName) {
			p.tracker.Update(chat.AgentSessionID, pb.ChatStatus_CHAT_STATUS_STOPPED, now)
			p.tracker.SetAuthFailed(chat.AgentSessionID, false)
			continue
		}
		activeChats = append(activeChats, chat)
		seen[chat.AgentSessionID] = true
	}

	// GC prevCaptures entries for chats that are no longer in the active set
	// (DB row removed, tmux name cleared, or tmux session died).
	p.mu.Lock()
	for id := range p.prevCaptures {
		if !seen[id] {
			delete(p.prevCaptures, id)
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

// refreshChatTitle asks the chat's AgentRunner plugin to extract a chat
// title from the on-disk transcript and persists it whenever the stored
// title is still a placeholder ("" or "New chat"). Run from every poll
// tick on the active chat set: the plugin call is cheap (a JSONL scan over
// the first ~50 lines) and idempotent (shouldRefreshChatTitle gates the
// path so chats with real titles never hit the plugin again).
//
// This is the daemon-side counterpart to the TUI's best-effort title
// backfill — it's what makes Codex chats render with their first user
// message instead of "New chat" in the web UI (which only reads
// chat.Title from the database, with no filesystem fallback).
func (p *TmuxStatusPoller) refreshChatTitle(ctx context.Context, chat *models.AgentChat) {
	if chat == nil || !shouldRefreshChatTitle(chat.Title) || p.sessions == nil {
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

// questionState resolves whether the captured pane content shows a question
// prompt and, if so, whether the user has already answered. Both signals are
// dispatched per-agent: HasQuestionPrompt and LastTurnIsUser run on the
// AgentRunner plugin matching chat.AgentName so each agent owns its own
// pane regex and transcript schema. When no client is registered for the
// chat's AgentName the chat falls through as "no question" — fail-open so a
// missing plugin can never lock a chat in QUESTION forever.
func (p *TmuxStatusPoller) questionState(ctx context.Context, chat *models.AgentChat, content string) (paneShowsQuestion, questionSuppressed bool) {
	if chat == nil {
		return false, false
	}
	client, ok := p.agentClients[chat.AgentName]
	if !ok {
		p.logMissingAgentOnce(chat.AgentName)
		return false, false
	}
	hpResp, err := client.HasQuestionPrompt(ctx, &pb.HasQuestionPromptRequest{PaneContent: []byte(content)})
	if err != nil || hpResp == nil || !hpResp.GetHasPrompt() {
		return false, false
	}
	paneShowsQuestion = true
	if p.sessions == nil {
		return paneShowsQuestion, false
	}
	sess, err := p.sessions.Get(ctx, chat.SessionID)
	if err != nil || sess == nil || sess.WorktreePath == "" {
		return paneShowsQuestion, false
	}
	luResp, err := client.LastTurnIsUser(ctx, &pb.LastTurnIsUserRequest{
		WorkDir:        sess.WorktreePath,
		AgentSessionId: chatResumeSessionID(chat),
	})
	questionSuppressed = err == nil && luResp != nil && luResp.GetIsUser()
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
