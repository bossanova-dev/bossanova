package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/rs/zerolog"
)

type backgroundDiscoveryChatStore struct {
	db.AgentChatStore
	updated chan string
	// current is what GetByAgentSessionID returns — the background writer
	// re-reads it to honor the never-overwrite-a-non-nil-id invariant (BOS-290).
	// nil ⇒ the store reports no stored row, so the write proceeds.
	current *models.AgentChat
}

func (s *backgroundDiscoveryChatStore) GetByAgentSessionID(_ context.Context, _ string) (*models.AgentChat, error) {
	return s.current, nil
}

func (s *backgroundDiscoveryChatStore) UpdateProviderSessionID(_ context.Context, _ string, providerSessionID *string) error {
	if providerSessionID != nil {
		s.updated <- *providerSessionID
	}
	return nil
}

type delayedInteractiveSessionResolver struct {
	mu    sync.Mutex
	calls []resolverCall
}

func (r *delayedInteractiveSessionResolver) ResolveInteractiveSessionID(_ context.Context, agentName, workDir, requestedSessionID string, launchedAfter, chatCreatedAt time.Time, allowLegacyBackfill bool, panePID int) (interactiveSessionResolution, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, resolverCall{
		agentName:           agentName,
		workDir:             workDir,
		requestedSessionID:  requestedSessionID,
		launchedAfter:       launchedAfter,
		chatCreatedAt:       chatCreatedAt,
		allowLegacyBackfill: allowLegacyBackfill,
		panePID:             panePID,
	})
	if len(r.calls) < 2 {
		return interactiveSessionResolution{}, nil
	}
	return interactiveSessionResolution{SessionID: "codex-real-session"}, nil
}

func (r *delayedInteractiveSessionResolver) snapshotCalls() []resolverCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]resolverCall(nil), r.calls...)
}

type legacyInteractiveSessionResolver struct {
	mu    sync.Mutex
	calls []resolverCall
}

func (r *legacyInteractiveSessionResolver) ResolveInteractiveSessionID(_ context.Context, agentName, workDir, requestedSessionID string, launchedAfter, chatCreatedAt time.Time, allowLegacyBackfill bool, panePID int) (interactiveSessionResolution, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, resolverCall{
		agentName:           agentName,
		workDir:             workDir,
		requestedSessionID:  requestedSessionID,
		launchedAfter:       launchedAfter,
		chatCreatedAt:       chatCreatedAt,
		allowLegacyBackfill: allowLegacyBackfill,
		panePID:             panePID,
	})
	if allowLegacyBackfill {
		return interactiveSessionResolution{SessionID: "codex-legacy-session"}, nil
	}
	return interactiveSessionResolution{}, nil
}

func (r *legacyInteractiveSessionResolver) snapshotCalls() []resolverCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]resolverCall(nil), r.calls...)
}

func TestBackgroundProviderSessionIDDiscoveryPersistsDelayedCodexID(t *testing.T) {
	oldTimeout := providerSessionIDBackgroundDiscoveryTimeout
	oldInterval := providerSessionIDBackgroundDiscoveryPollInterval
	providerSessionIDBackgroundDiscoveryTimeout = time.Second
	providerSessionIDBackgroundDiscoveryPollInterval = 5 * time.Millisecond
	defer func() {
		providerSessionIDBackgroundDiscoveryTimeout = oldTimeout
		providerSessionIDBackgroundDiscoveryPollInterval = oldInterval
	}()

	store := &backgroundDiscoveryChatStore{updated: make(chan string, 1)}
	resolver := &delayedInteractiveSessionResolver{}
	s := &Server{
		agentChats: store,
		logger:     zerolog.Nop(),
	}

	launchedAt := time.Now()
	// Empty tmuxName + nil s.tmux ⇒ background discovery resolves with pane pid
	// 0 (time-window fallback), the exact behavior this test pins.
	s.discoverProviderSessionIDInBackground(&models.AgentChat{
		AgentSessionID: "boss-session-id",
		AgentName:      "codex",
	}, "/tmp/worktree", "", launchedAt, resolver)

	select {
	case got := <-store.updated:
		if got != "codex-real-session" {
			t.Fatalf("provider session id = %q, want codex-real-session", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for background provider session id persistence")
	}

	calls := resolver.snapshotCalls()
	if len(calls) < 2 {
		t.Fatalf("resolver calls = %d, want at least 2", len(calls))
	}
	last := calls[len(calls)-1]
	if last.agentName != "codex" || last.workDir != "/tmp/worktree" || last.requestedSessionID != "boss-session-id" {
		t.Fatalf("resolver called with %+v", last)
	}
	if last.launchedAfter.IsZero() || last.allowLegacyBackfill {
		t.Fatalf("resolver launch/backfill fields wrong: %+v", last)
	}
}

// TestBackgroundProviderSessionIDDiscoverySkipsWriteWhenAlreadyBound pins the
// BOS-290 invariant at the background write site: if another path binds a
// provider id while this goroutine polls, the re-resolved id must NOT overwrite
// it. The store reports a non-nil current id, so the goroutine must terminate
// without writing even though the resolver returns a (different) id.
func TestBackgroundProviderSessionIDDiscoverySkipsWriteWhenAlreadyBound(t *testing.T) {
	oldTimeout := providerSessionIDBackgroundDiscoveryTimeout
	oldInterval := providerSessionIDBackgroundDiscoveryPollInterval
	providerSessionIDBackgroundDiscoveryTimeout = time.Second
	providerSessionIDBackgroundDiscoveryPollInterval = 5 * time.Millisecond
	defer func() {
		providerSessionIDBackgroundDiscoveryTimeout = oldTimeout
		providerSessionIDBackgroundDiscoveryPollInterval = oldInterval
	}()

	bound := "already-bound-id"
	store := &backgroundDiscoveryChatStore{
		updated: make(chan string, 1),
		current: &models.AgentChat{AgentSessionID: "boss-session-id", ProviderSessionID: &bound},
	}
	resolver := &delayedInteractiveSessionResolver{}
	s := &Server{
		agentChats: store,
		logger:     zerolog.Nop(),
	}

	s.discoverProviderSessionIDInBackground(&models.AgentChat{
		AgentSessionID: "boss-session-id",
		AgentName:      "codex",
	}, "/tmp/worktree", "", time.Now(), resolver)

	select {
	case got := <-store.updated:
		t.Fatalf("wrote provider session id %q over an already-bound id (must never overwrite)", got)
	case <-time.After(200 * time.Millisecond):
		// No write within a comfortable multiple of the poll interval: the
		// guard held and the goroutine returned without clobbering.
	}
}

func TestBackfillCodexProviderSessionIDPersistsBeforeAttachResume(t *testing.T) {
	store := &backgroundDiscoveryChatStore{updated: make(chan string, 1)}
	resolver := &legacyInteractiveSessionResolver{}
	s := &Server{
		agentChats: store,
		logger:     zerolog.Nop(),
	}
	createdAt := time.Now().Add(-time.Minute)
	chat := &models.AgentChat{
		AgentSessionID: "boss-session-id",
		AgentName:      "codex",
		CreatedAt:      createdAt,
	}

	ok, reason, err := s.backfillCodexProviderSessionID(context.Background(), chat, "/tmp/worktree", resolver)
	if err != nil {
		t.Fatalf("backfillCodexProviderSessionID: %v", err)
	}
	if !ok || reason != "" {
		t.Fatalf("ok/reason = %v/%q, want true/empty", ok, reason)
	}
	select {
	case got := <-store.updated:
		if got != "codex-legacy-session" {
			t.Fatalf("provider session id = %q, want codex-legacy-session", got)
		}
	default:
		t.Fatal("provider session id was not persisted")
	}
	if chat.ProviderSessionID == nil || *chat.ProviderSessionID != "codex-legacy-session" {
		t.Fatalf("chat provider session id = %v, want codex-legacy-session", chat.ProviderSessionID)
	}

	calls := resolver.snapshotCalls()
	if len(calls) != 1 {
		t.Fatalf("resolver calls = %d, want 1", len(calls))
	}
	call := calls[0]
	if !call.allowLegacyBackfill || call.chatCreatedAt != createdAt || call.requestedSessionID != "boss-session-id" {
		t.Fatalf("resolver called with %+v", call)
	}
}
