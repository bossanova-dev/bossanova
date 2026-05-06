package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/rs/zerolog"
)

var _ AgentRunner = (*Dispatcher)(nil)

// ErrAgentNotLoaded is returned by Dispatcher methods when the resolved
// agent name has no entry in the runner registry.
var ErrAgentNotLoaded = errors.New("agent not loaded")

// Dispatcher is a per-call agent router that implements AgentRunner by
// resolving the session's configured agent name to a concrete AgentRunner
// from a name-keyed registry. Sessions that don't specify an agent (or whose
// lookup errors) fall back to defaultAgent. Pure routing — no caching, no
// metrics.
type Dispatcher struct {
	// runners is read-only after construction; do not mutate.
	runners      map[string]AgentRunner
	lookup       func(sessionID string) (string, error)
	defaultAgent string
	logger       zerolog.Logger
}

// NewDispatcher builds a Dispatcher.
//
//   - runners is the registry of loaded agent runners keyed by plugin name
//     (typically derived from plugin.Host.AgentRunners()). A nil map is
//     accepted (lookups simply return the zero value), but lookup must not
//     be nil.
//   - lookup resolves a session ID to its configured agent name. The
//     dispatcher does not own any session store; the lifecycle wires this
//     closure with the actual DB lookup. Must not be nil — NewDispatcher
//     panics if it is, since a nil lookup is a programmer error that would
//     nil-deref on the first method call.
//   - defaultAgent is used when the session has no configured agent or when
//     lookup returns an empty string / error. Typically Settings.DefaultAgent
//     ("claude" by default).
//   - logger is used for diagnostics on lookup failures and unknown agents.
func NewDispatcher(runners map[string]AgentRunner, lookup func(sessionID string) (string, error), defaultAgent string, logger zerolog.Logger) *Dispatcher {
	if lookup == nil {
		panic("agent.NewDispatcher: lookup must not be nil")
	}
	return &Dispatcher{
		runners:      runners,
		lookup:       lookup,
		defaultAgent: defaultAgent,
		logger:       logger,
	}
}

// resolve picks the agent name for sessionID, falling back to defaultAgent
// when lookup errors or returns empty. It returns the AgentRunner and the
// resolved name. A nil runner means no plugin is loaded under that name —
// callers must surface this as an error (or false, for IsRunning).
func (d *Dispatcher) resolve(sessionID string) (AgentRunner, string) {
	name, err := d.lookup(sessionID)
	if err != nil {
		d.logger.Warn().
			Err(err).
			Str("session_id", sessionID).
			Str("default_agent", d.defaultAgent).
			Msg("agent lookup failed; falling back to default agent")
		name = ""
	}
	if name == "" {
		name = d.defaultAgent
	}
	return d.runners[name], name
}

// Start routes to the resolved agent's Start.
func (d *Dispatcher) Start(ctx context.Context, workDir, plan string, resume *string, sessionID string) (string, error) {
	runner, name := d.resolve(sessionID)
	if runner == nil {
		return "", fmt.Errorf("agent %q not loaded: %w", name, ErrAgentNotLoaded)
	}
	return runner.Start(ctx, workDir, plan, resume, sessionID)
}

// Stop routes to the resolved agent's Stop.
func (d *Dispatcher) Stop(sessionID string) error {
	runner, name := d.resolve(sessionID)
	if runner == nil {
		return fmt.Errorf("agent %q not loaded: %w", name, ErrAgentNotLoaded)
	}
	return runner.Stop(sessionID)
}

// IsRunning routes to the resolved agent's IsRunning. If the resolved agent
// is not loaded, returns false and logs — IsRunning has no error channel
// and a true positive would be misleading.
func (d *Dispatcher) IsRunning(sessionID string) bool {
	runner, name := d.resolve(sessionID)
	if runner == nil {
		d.logger.Warn().
			Str("session_id", sessionID).
			Str("agent", name).
			Msg("IsRunning: agent not loaded")
		return false
	}
	return runner.IsRunning(sessionID)
}

// ExitError routes to the resolved agent's ExitError.
func (d *Dispatcher) ExitError(sessionID string) error {
	runner, name := d.resolve(sessionID)
	if runner == nil {
		return fmt.Errorf("agent %q not loaded: %w", name, ErrAgentNotLoaded)
	}
	return runner.ExitError(sessionID)
}

// Subscribe routes to the resolved agent's Subscribe.
func (d *Dispatcher) Subscribe(ctx context.Context, sessionID string) (<-chan OutputLine, error) {
	runner, name := d.resolve(sessionID)
	if runner == nil {
		return nil, fmt.Errorf("agent %q not loaded: %w", name, ErrAgentNotLoaded)
	}
	return runner.Subscribe(ctx, sessionID)
}

// History routes to the resolved agent's History. If the resolved agent is
// not loaded, returns nil — History has no error channel and an empty
// history is a safe default.
func (d *Dispatcher) History(sessionID string) []OutputLine {
	runner, name := d.resolve(sessionID)
	if runner == nil {
		d.logger.Warn().
			Str("session_id", sessionID).
			Str("agent", name).
			Msg("History: agent not loaded")
		return nil
	}
	return runner.History(sessionID)
}
