package status

import (
	"context"
	"sync"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/safego"
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

	// Filter to chats whose tmux session is alive right now.
	activeChats := make([]*models.AgentChat, 0, len(chats))
	seen := make(map[string]bool, len(chats))
	for _, chat := range chats {
		if chat.TmuxSessionName == nil || *chat.TmuxSessionName == "" {
			continue
		}
		if !p.tmux.HasSession(ctx, *chat.TmuxSessionName) {
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

	now := time.Now()
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

		// Resolve question state before taking p.mu — questionState
		// may issue plugin RPCs / DB queries and we hold the mutex only
		// briefly below.
		paneShowsQuestion, questionSuppressed := p.questionState(ctx, chat, content)

		p.mu.Lock()
		prev, hasPrev := p.prevCaptures[agentSessionID]

		var status pb.ChatStatus
		switch {
		case paneShowsQuestion && !questionSuppressed:
			status = pb.ChatStatus_CHAT_STATUS_QUESTION
		case questionSuppressed:
			// The pane still matches the question pattern but the transcript
			// shows the user has answered — Claude is about to render its
			// response. Report WORKING explicitly so the UI doesn't briefly
			// flash IDLE when the old question capture is already past the
			// idle threshold.
			status = pb.ChatStatus_CHAT_STATUS_WORKING
		case !hasPrev || content != prev.content:
			status = pb.ChatStatus_CHAT_STATUS_WORKING
		case now.Sub(prev.at) > IdleThreshold:
			status = pb.ChatStatus_CHAT_STATUS_IDLE
		default:
			// Content unchanged but not yet past idle threshold -- keep working.
			status = pb.ChatStatus_CHAT_STATUS_WORKING
		}

		// Update capture entry: only update timestamp when content changed.
		if !hasPrev || content != prev.content {
			p.prevCaptures[agentSessionID] = captureEntry{content: content, at: now}
		}
		p.mu.Unlock()

		p.tracker.Update(agentSessionID, status, now)
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
	// as having exceeded IdleThreshold, and reports idle.
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

		paneShowsQuestion, questionSuppressed := p.questionState(ctx, chat, content)

		var status pb.ChatStatus
		switch {
		case paneShowsQuestion && !questionSuppressed:
			status = pb.ChatStatus_CHAT_STATUS_QUESTION
		case questionSuppressed:
			// Mirror pollOnce: the pane still matches the question pattern but
			// the transcript shows the user has answered. Report WORKING so the
			// UI doesn't flash IDLE before the first poll cycle corrects it.
			status = pb.ChatStatus_CHAT_STATUS_WORKING
		default:
			status = pb.ChatStatus_CHAT_STATUS_IDLE
		}

		p.mu.Lock()
		p.prevCaptures[chat.AgentSessionID] = captureEntry{content: content, at: pastTime}
		p.mu.Unlock()

		p.tracker.Update(chat.AgentSessionID, status, now)
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
		AgentSessionId: chat.AgentSessionID,
	})
	questionSuppressed = err == nil && luResp != nil && luResp.GetIsUser()
	return paneShowsQuestion, questionSuppressed
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
