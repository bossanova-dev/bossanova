package agent

import (
	"context"
	"math/rand"
	"time"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/safego"
)

// Completer is the host-side hook PollFallback signals when an agent run
// completes. Implemented by *plugin.HostServiceServer.SignalRunComplete
// (in-process, no auth — see CompleteAgentRun for the external auth-checked
// path). The interface keeps the package-internal coupling explicit and
// avoids an import cycle between the agent and plugin packages.
type Completer interface {
	SignalRunComplete(agentSessionID, exitError string)
}

// PollFallback runs a per-run goroutine that polls the agent plugin's
// ExitStatus on a jittered cadence. When IsComplete becomes true the
// goroutine signals the host's run-completion path and exits.
//
// Used for agent plugins (e.g. codex) whose ConfigureFinalizeHook returns
// IsSupported=false — without a finalize hook, the daemon needs an active
// way to learn that the run finished. Plugins that own a finalize hook
// (e.g. claude) skip this entirely; their Stop hook drives CompleteAgentRun
// directly.
type PollFallback struct {
	logger    zerolog.Logger
	cadence   time.Duration
	jitter    time.Duration
	completer Completer
}

// NewPollFallback constructs a PollFallback. cadence is the base poll
// interval; jitter is added/subtracted uniformly per iteration to avoid
// thundering-herd patterns when multiple runs are armed simultaneously.
// A zero jitter disables jittering. The completer is invoked exactly once
// per armed run, when ExitStatus first reports IsComplete.
func NewPollFallback(logger zerolog.Logger, cadence, jitter time.Duration, c Completer) *PollFallback {
	return &PollFallback{logger: logger, cadence: cadence, jitter: jitter, completer: c}
}

// Arm spawns the polling goroutine. The goroutine exits when ctx is done
// or when ExitStatus reports IsComplete (the latter signals the completer
// before returning). Safe to call from any goroutine — internally uses
// safego.Go so a panic in the polling loop is logged and recovered rather
// than crashing the daemon.
func (p *PollFallback) Arm(ctx context.Context, agentSessionID string, client AgentRunnerClient) {
	safego.Go(p.logger, func() {
		for {
			d := p.cadence
			if p.jitter > 0 {
				d += time.Duration(rand.Int63n(int64(p.jitter*2))) - p.jitter //nolint:gosec // jitter doesn't need crypto-grade randomness
				if d < time.Millisecond {
					d = time.Millisecond
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(d):
			}
			resp, err := client.ExitStatus(ctx, &bossanovav1.AgentExitStatusRequest{SessionId: agentSessionID})
			if err != nil {
				p.logger.Debug().Err(err).Str("agent_session", agentSessionID).Msg("poll fallback: ExitStatus error; will retry")
				continue
			}
			if !resp.IsComplete {
				continue
			}
			p.completer.SignalRunComplete(agentSessionID, resp.ExitError)
			return
		}
	})
}
