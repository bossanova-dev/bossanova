package status

import (
	"context"
	"sync"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/safego"
	"github.com/recurser/bossalib/statusdetect"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/tmux"
	"github.com/rs/zerolog"
)

// TmuxStatusPoller polls tmux pane content for active chats and feeds
// working/idle/question statuses into the status tracker.
type TmuxStatusPoller struct {
	tracker *Tracker
	chats   db.ClaudeChatStore
	tmux    *tmux.Client
	logger  zerolog.Logger

	mu           sync.Mutex
	prevCaptures map[string]captureEntry // claudeID -> previous capture

	done chan struct{} // closed when Run's goroutine exits
}

type captureEntry struct {
	content string
	at      time.Time
}

// NewTmuxStatusPoller creates a new poller.
func NewTmuxStatusPoller(tracker *Tracker, chats db.ClaudeChatStore, tmux *tmux.Client, logger zerolog.Logger) *TmuxStatusPoller {
	return &TmuxStatusPoller{
		tracker:      tracker,
		chats:        chats,
		tmux:         tmux,
		logger:       logger,
		prevCaptures: make(map[string]captureEntry),
		done:         make(chan struct{}),
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
func (p *TmuxStatusPoller) pollOnce(ctx context.Context) {
	// We don't have a "list all chats with tmux names" query, so we need
	// to work from the previous captures map to know which chats to check.
	// On top of that, scan for newly active chats from the tracker's entries.
	p.mu.Lock()
	activeClaudes := make(map[string]string) // claudeID -> tmuxSessionName
	for claudeID := range p.prevCaptures {
		activeClaudes[claudeID] = ""
	}
	p.mu.Unlock()

	// Look up tmux session names for known active chats.
	for claudeID := range activeClaudes {
		chat, err := p.chats.GetByClaudeID(ctx, claudeID)
		if err != nil || chat.TmuxSessionName == nil || *chat.TmuxSessionName == "" {
			// Chat removed or no longer has a tmux session.
			p.mu.Lock()
			delete(p.prevCaptures, claudeID)
			p.mu.Unlock()
			continue
		}
		if !p.tmux.HasSession(ctx, *chat.TmuxSessionName) {
			// Tmux session died.
			p.mu.Lock()
			delete(p.prevCaptures, claudeID)
			p.mu.Unlock()
			continue
		}
		activeClaudes[claudeID] = *chat.TmuxSessionName
	}

	// Capture pane and detect status for each active chat.
	now := time.Now()
	for claudeID, tmuxName := range activeClaudes {
		if tmuxName == "" {
			continue
		}
		content, err := p.tmux.CapturePane(ctx, tmuxName)
		if err != nil {
			p.logger.Debug().Err(err).
				Str("claudeID", claudeID).
				Str("tmuxSession", tmuxName).
				Msg("failed to capture tmux pane")
			continue
		}

		p.mu.Lock()
		prev, hasPrev := p.prevCaptures[claudeID]

		var status pb.ChatStatus
		if statusdetect.HasQuestionPrompt([]byte(content)) {
			status = pb.ChatStatus_CHAT_STATUS_QUESTION
		} else if !hasPrev || content != prev.content {
			status = pb.ChatStatus_CHAT_STATUS_WORKING
		} else if now.Sub(prev.at) > IdleThreshold {
			status = pb.ChatStatus_CHAT_STATUS_IDLE
		} else {
			// Content unchanged but not yet past idle threshold -- keep working.
			status = pb.ChatStatus_CHAT_STATUS_WORKING
		}

		// Update capture entry: only update timestamp when content changed.
		if !hasPrev || content != prev.content {
			p.prevCaptures[claudeID] = captureEntry{content: content, at: now}
		}
		p.mu.Unlock()

		p.tracker.Update(claudeID, status, now)
	}
}

// RegisterChat adds a chat to the polling set. Called when a new tmux session
// is created so the poller starts tracking it immediately.
func (p *TmuxStatusPoller) RegisterChat(claudeID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.prevCaptures[claudeID]; !ok {
		p.prevCaptures[claudeID] = captureEntry{}
	}
}

// UnregisterChat removes a chat from the polling set.
func (p *TmuxStatusPoller) UnregisterChat(claudeID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.prevCaptures, claudeID)
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
				Str("claudeID", chat.ClaudeID).
				Str("tmuxSession", tmuxName).
				Msg("bootstrap: failed to capture tmux pane")
			continue
		}

		var status pb.ChatStatus
		if statusdetect.HasQuestionPrompt([]byte(content)) {
			status = pb.ChatStatus_CHAT_STATUS_QUESTION
		} else {
			status = pb.ChatStatus_CHAT_STATUS_IDLE
		}

		p.mu.Lock()
		p.prevCaptures[chat.ClaudeID] = captureEntry{content: content, at: pastTime}
		p.mu.Unlock()

		p.tracker.Update(chat.ClaudeID, status, now)
		p.logger.Debug().
			Str("claudeID", chat.ClaudeID).
			Str("tmuxSession", tmuxName).
			Str("status", status.String()).
			Msg("bootstrap: seeded chat status")
	}

	if len(chats) > 0 {
		p.logger.Info().Int("count", len(chats)).Msg("bootstrap: discovered chats with tmux sessions")
	}
}
