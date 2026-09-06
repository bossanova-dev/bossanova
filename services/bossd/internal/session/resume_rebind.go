package session

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/recurser/bossd/internal/db"
)

// ErrResumedChatMissing is the sentinel behind ResumedChatMissingError. A
// resume names a chat that must already exist; when no row carries the id
// there is nothing to resume, and the run is refused rather than reconstructed
// from the parent session's defaults.
//
// It deliberately does NOT wrap db.ErrAgentChatNotFound. Several callers treat
// that error as "fine, make one" — routing the refusal through it would let an
// existing errors.Is branch quietly downgrade the refusal back into a spawn
// (BOS-973).
var ErrResumedChatMissing = errors.New("resumed chat row is missing")

// ResumedChatMissingError names the agent_session_id whose chat row is absent,
// so an operator reading the refusal can find the chat that went missing.
type ResumedChatMissingError struct {
	AgentSessionID string
}

func (e *ResumedChatMissingError) Error() string {
	return fmt.Sprintf("cannot resume agent session %q: no agent_chats row carries that id", e.AgentSessionID)
}

func (e *ResumedChatMissingError) Unwrap() error { return ErrResumedChatMissing }

// GRPCStatus classifies the refusal as FailedPrecondition, matching the other
// pre-launch refusals StartTmuxChat returns (tmux unavailable, no agent runner
// loaded, no worktree path). Without it the typed error carries no status and
// reaches a client as codes.Internal — indistinguishable from a daemon crash,
// when it is in fact a caller-side precondition the caller can act on.
//
// This is additive: Unwrap still returns ErrResumedChatMissing, so
// errors.Is/errors.As behaviour is unchanged.
func (e *ResumedChatMissingError) GRPCStatus() *grpcstatus.Status {
	return grpcstatus.New(codes.FailedPrecondition, e.Error())
}

// rebindResumedChat is the single seam through which every resume path
// rewrites an existing chat row (BOS-1143). Both the tmux chat launcher and
// the orphan-run resumer go through it, so the identity-preservation rule —
// update in place, never delete-and-recreate, and only touch the columns the
// caller names — is bound once instead of re-derived per call site (BOS-894).
//
// A missing row is translated into the typed ResumedChatMissingError refusal.
func (l *Lifecycle) rebindResumedChat(ctx context.Context, agentSessionID string, params db.RebindResumedChatParams) error {
	if l.agentChats == nil {
		return fmt.Errorf("rebind resumed chat %q: agent chat store not configured", agentSessionID)
	}
	err := l.agentChats.RebindResumedChat(ctx, agentSessionID, params)
	if errors.Is(err, db.ErrAgentChatNotFound) {
		return &ResumedChatMissingError{AgentSessionID: agentSessionID}
	}
	if err != nil {
		return fmt.Errorf("rebind resumed chat %q: %w", agentSessionID, err)
	}
	return nil
}
