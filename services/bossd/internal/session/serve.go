package session

import (
	"context"
	"fmt"
)

// ServeStopper stops repository services before session teardown.
type ServeStopper interface {
	Stop(context.Context, string) error
}

// SetServeStopper wires the daemon's serve runner before accepting requests.
func (l *Lifecycle) SetServeStopper(stopper ServeStopper) { l.serveStopper = stopper }

// StopServeSession stops services without changing the session's agent state.
func (l *Lifecycle) StopServeSession(ctx context.Context, id string) error {
	if l.serveStopper == nil {
		return nil
	}
	return l.serveStopper.Stop(ctx, id)
}

func (l *Lifecycle) stopServicesForArchive(ctx context.Context, id string) error {
	if err := l.StopServeSession(ctx, id); err != nil {
		return fmt.Errorf("stop session services: %w", err)
	}
	_ = l.killChatTmuxSessions(ctx, id)
	return nil
}
