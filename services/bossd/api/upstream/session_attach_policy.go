package upstream

import (
	"context"

	"github.com/recurser/bossalib/kit/config"
)

// Retain revocation until the stream ends: the writer may still have queued
// chunks after the pump finishes, even if sharing has since been re-enabled.
type sessionAttachState struct {
	cancel  context.CancelFunc
	done    chan struct{}
	revoked bool // guarded by sessionAttachMu
}

func (c *StreamClient) allowSessionAttach(commandID string) bool {
	private := cloudControlMode(c.cloudControl) == config.CloudControlPrivate
	c.sessionAttachMu.Lock()
	defer c.sessionAttachMu.Unlock()
	if private {
		for _, state := range c.sessionAttaches {
			state.revoked = true
			state.cancel()
		}
		return false
	}
	state := c.sessionAttaches[commandID]
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
