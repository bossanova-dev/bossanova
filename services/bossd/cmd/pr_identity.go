package main

import (
	"context"

	"github.com/rs/zerolog"

	"github.com/recurser/bossd/internal/prstack"
)

// prStackReader serves a session's live open-PR result.
type prStackReader interface {
	Stack(sessionID string) (prstack.Result, bool)
}

// prIdentityWriter stores a session's PR author and target branch.
type prIdentityWriter interface {
	SetPRAuthorAndTarget(ctx context.Context, id string, prNumber int, author, target string) error
}

// onPRStackChange returns the PR-stack change callback: it stores the
// session's PR author and target, then republishes the session, so the
// published delta and every later read carry the stored values.
func onPRStackChange(stacks prStackReader, store prIdentityWriter, publish func(ctx context.Context, sessionID string), logger zerolog.Logger) func(sessionID string) {
	return func(sessionID string) {
		ctx := context.Background()
		persistPRIdentity(ctx, stacks, store, sessionID, logger)
		publish(ctx, sessionID)
	}
}

// persistPRIdentity stores the live PR author and base branch for sessionID,
// so both outlive the PR leaving the open-PR listing and a daemon restart. A
// session with no live result keeps what is stored. A failed write is logged
// and never blocks the republish that follows it.
func persistPRIdentity(ctx context.Context, stacks prStackReader, store prIdentityWriter, sessionID string, logger zerolog.Logger) {
	r, ok := stacks.Stack(sessionID)
	if !ok || r.PRNumber == 0 {
		return
	}
	if err := store.SetPRAuthorAndTarget(ctx, sessionID, r.PRNumber, r.Author, r.BaseBranch); err != nil {
		logger.Warn().Err(err).Str("session_id", sessionID).Int("pr_number", r.PRNumber).Msg("pr stack: store PR author and target failed")
	}
}
