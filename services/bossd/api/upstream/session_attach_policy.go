package upstream

import (
	"context"

	"github.com/recurser/bossalib/kit/config"
)

// Retain revocation until the stream ends: the writer may still have queued
// chunks after the pump finishes, even if sharing has since been re-enabled.
type sessionAttachState struct {
	cancel    context.CancelFunc
	done      chan struct{}
	sessionID string
	revoked   bool // guarded by sessionAttachMu
}

func (c *StreamClient) allowSessionAttach(ctx context.Context, commandID, sessionID string) bool {
	c.sessionAttachMu.Lock()
	registered := c.sessionAttaches[commandID]
	if registered != nil {
		sessionID = registered.sessionID
	}
	c.sessionAttachMu.Unlock()
	object, err := CommandTargetObject{}, errCommandTarget
	if c.targetObjects != nil {
		object, err = c.targetObjects.GetTarget(ctx, "session", sessionID)
	}
	private := err != nil || object.RepoID == "" || repoCloudControlMode(ctx, c.cloudControl, object.RepoID) == config.CloudControlPrivate
	c.sessionAttachMu.Lock()
	defer c.sessionAttachMu.Unlock()
	state := c.sessionAttaches[commandID]
	if private {
		if state != nil {
			state.revoked = true
			state.cancel()
		}
		return false
	}
	return state == nil || !state.revoked
}

// Join pumps before the shared outbound channel is closed.
func (c *StreamClient) cancelAndWaitSessionAttaches() {
	c.sessionAttachMu.Lock()
	states := make([]*sessionAttachState, 0, len(c.sessionAttaches))
	for _, state := range c.sessionAttaches {
		state.cancel()
		states = append(states, state)
	}
	c.sessionAttachMu.Unlock()
	for _, state := range states {
		<-state.done
	}
}
